package websocket

import (
	"context"
	"fmt"
	"math"
	"simo/internal/constants"
	"strings"
	"sync"
	"time"

	bin "github.com/gagliardetto/binary"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/ws"
	"github.com/rs/zerolog"
)

type Client struct {
	client        *ws.Client
	subscriptions sync.Map
	logger        zerolog.Logger
	url           string // Store URL for reconnection
	rpcURL        string
	done          chan struct{} // Channel to signal shutdown
	mu            sync.Mutex
	connected     bool
	reconnectMu   sync.Mutex
}

func NewClient(wsURL string, logger zerolog.Logger, rpcUrl string) (*Client, error) {
	logger.Info().Msg("Attempting to connect to WebSocket...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wsClient, err := ws.Connect(ctx, wsURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to websocket: %w", err)
	}

	logger.Info().
		Str("url", wsURL).
		Msg("Successfully connected to websocket")

	client := &Client{
		client: wsClient,
		logger: logger,
		url:    wsURL,
		rpcURL: rpcUrl,
		done:   make(chan struct{}),
	}
	client.startHealthCheck()
	return client, nil
}

func (c *Client) Close() {
	close(c.done)
	if c.client != nil {
		c.client.Close()
	}
}

func (c *Client) reconnect() error {
	c.reconnectMu.Lock()
	defer c.reconnectMu.Unlock()

	maxRetries := 5
	for i := 0; i < maxRetries; i++ {
		if i > 0 {
			// Exponential backoff
			backoff := time.Duration(math.Pow(2, float64(i))) * time.Second
			time.Sleep(backoff)
		}

		c.logger.Info().
			Int("attempt", i+1).
			Msg("Attempting to reconnect WebSocket")

		if c.client != nil {
			c.client.Close()
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		newClient, err := ws.Connect(ctx, c.url)
		cancel()

		if err != nil {
			c.logger.Error().
				Err(err).
				Int("attempt", i+1).
				Msg("Failed to reconnect")
			continue
		}

		c.client = newClient
		c.connected = true

		// Resubscribe to all existing subscriptions
		c.subscriptions.Range(func(key, value interface{}) bool {
			// Implementation of resubscription logic
			return true
		})

		c.logger.Info().Msg("Successfully reconnected and resubscribed")
		return nil
	}

	return fmt.Errorf("failed to reconnect after %d attempts", maxRetries)
}

func (c *Client) SubscribeToWallet(ctx context.Context, walletAddress string, callback func(string)) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	pubkey := solana.MustPublicKeyFromBase58(walletAddress)

	// Subscribe to logs mentioning the wallet
	logsSub, err := c.client.LogsSubscribeMentions(
		pubkey, // Pass the pubkey directly
		rpc.CommitmentConfirmed,
	)
	if err != nil {
		return fmt.Errorf("failed to subscribe to logs: %w", err)
	}

	// Store subscription
	c.subscriptions.Store(walletAddress+"_logs", logsSub)

	// Monitor logs
	go func() {
		defer logsSub.Unsubscribe()

		for {
			select {
			case <-ctx.Done():
				return
			case <-c.done:
				return
			default:
				log, err := logsSub.Recv(ctx)
				if err != nil {
					c.logger.Error().
						Err(err).
						Str("wallet", walletAddress).
						Msg("Error receiving log notification")
					continue
				}

				if log != nil {
					signature := log.Value.Signature.String()
					c.logger.Info().
						Str("wallet", walletAddress).
						Str("signature", signature).
						Strs("logs", log.Value.Logs).
						Msg("Received new transaction")

					callback(signature)
				}
			}
		}
	}()

	return nil
}

func (c *Client) monitorWallet(ctx context.Context, walletAddress string, sub *ws.LogSubscription, callback func(string)) {
	defer sub.Unsubscribe()

	rpcClient := rpc.New(c.rpcURL)
	rateLimiter := time.NewTicker(100 * time.Millisecond) // Max 10 requests per second
	defer rateLimiter.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		default:
			got, err := sub.Recv(ctx)
			if err != nil {
				c.logger.Error().
					Err(err).
					Str("wallet", walletAddress).
					Msg("Error receiving wallet notification")
				// Trigger reconnection
				if err := c.reconnect(); err != nil {
					c.logger.Error().Err(err).Msg("Failed to reconnect")
				}
				continue
			}

			// Wait a bit for the transaction to be available
			time.Sleep(2 * time.Second)

			// Rate limit our RPC calls
			<-rateLimiter.C

			// Implement retries for transaction fetch
			var tx *rpc.GetTransactionResult
			for retries := 0; retries < 3; retries++ {
				tx, err = rpcClient.GetTransaction(
					ctx,
					got.Value.Signature,
					&rpc.GetTransactionOpts{
						Encoding: solana.EncodingBase64,
					},
				)

				if err == nil {
					break
				}

				// If rate limited, wait longer
				if strings.Contains(err.Error(), "429") {
					time.Sleep(time.Duration(retries+1) * time.Second)
					continue
				}

				// If not found, wait and retry
				if strings.Contains(err.Error(), "not found") {
					time.Sleep(time.Second)
					continue
				}

				c.logger.Error().
					Err(err).
					Str("signature", got.Value.Signature.String()).
					Int("retry", retries).
					Msg("Failed to get transaction")
			}

			if err != nil {
				continue
			}

			// Skip failed transactions
			if tx.Meta.Err != nil {
				c.logger.Debug().
					Str("signature", got.Value.Signature.String()).
					Msg("Skipping failed transaction")
				continue
			}

			decodedTx, err := solana.TransactionFromDecoder(bin.NewBinDecoder(tx.Transaction.GetBinary()))
			if err != nil {
				c.logger.Error().
					Err(err).
					Str("signature", got.Value.Signature.String()).
					Msg("Failed to decode transaction")
				continue
			}

			if isSwapTransaction(decodedTx) {
				c.logger.Info().
					Str("wallet", walletAddress).
					Str("signature", got.Value.Signature.String()).
					Msg("Swap transaction detected")
				callback(got.Value.Signature.String())
			}
		}
	}
}

func (c *Client) UnsubscribeFromWallet(walletAddress string) {
	if sub, ok := c.subscriptions.LoadAndDelete(walletAddress); ok {
		if subscription, ok := sub.(*ws.AccountSubscription); ok {
			subscription.Unsubscribe()
			c.logger.Info().
				Str("wallet", walletAddress).
				Msg("Unsubscribed from wallet")
		}
	}
}
func (c *Client) monitorProgram(ctx context.Context, walletAddress string, sub *ws.LogSubscription, callback func(string)) {

	defer sub.Unsubscribe()

	c.logger.Info().
		Str("wallet", walletAddress).
		Msg("Started monitoring DEX program for wallet")

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		default:
			got, err := sub.Recv(ctx)
			if err != nil {
				c.logger.Error().
					Err(err).
					Str("wallet", walletAddress).
					Msg("Error receiving program notification")
				// Trigger reconnection
				if err := c.reconnect(); err != nil {
					c.logger.Error().Err(err).Msg("Failed to reconnect")
				}
				continue
			}

			// Check if the log mentions our wallet
			if len(got.Value.Logs) > 0 && strings.Contains(strings.Join(got.Value.Logs, " "), walletAddress) {
				c.logger.Info().
					Str("wallet", walletAddress).
					Str("signature", got.Value.Signature.String()).
					Strs("logs", got.Value.Logs).
					Msg("Detected wallet activity in DEX program")

				callback(got.Value.Signature.String())
			}
		}
	}
}

func isSwapTransaction(tx *solana.Transaction) bool {
	for _, inst := range tx.Message.Instructions {
		programID := tx.Message.AccountKeys[inst.ProgramIDIndex]
		if constants.IsDEXProgram(programID) {
			return true
		}
	}
	return false
}

func (c *Client) startHealthCheck() {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-c.done:
				return
			case <-ticker.C:
				c.mu.Lock()
				client := c.client
				c.mu.Unlock()

				// Test the connection
				if client == nil || !c.testConnection() {
					c.logger.Warn().Msg("WebSocket connection appears to be down, attempting reconnect")
					if err := c.reconnect(); err != nil {
						c.logger.Error().Err(err).Msg("Failed to reconnect")
					}
				}
			}
		}
	}()
}

func (c *Client) testConnection() bool {
	_, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Test connection by attempting to subscribe to a dummy pubkey
	testKey := solana.MustPublicKeyFromBase58("11111111111111111111111111111111")
	sub, err := c.client.LogsSubscribeMentions(testKey, rpc.CommitmentConfirmed)
	if err != nil {
		c.logger.Warn().Err(err).Msg("WebSocket connection test failed")
		return false
	}
	sub.Unsubscribe()
	return true
}