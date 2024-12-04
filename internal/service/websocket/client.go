package websocket

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/ws"
	"github.com/rs/zerolog"
	"github.com/sourcegraph/conc/pool"
	"golang.org/x/time/rate"
)

// Config holds the client configuration
type Config struct {
	WSURL           string
	RPCURL          string
	MaxRetries      int
	BackoffMin      time.Duration
	BackoffMax      time.Duration
	HealthCheckFreq time.Duration
	RateLimit       rate.Limit
}

// DefaultConfig returns a default configuration
func DefaultConfig() Config {
	return Config{
		MaxRetries:      5,
		BackoffMin:      time.Second,
		BackoffMax:      time.Minute,
		HealthCheckFreq: 15 * time.Second,
		RateLimit:       rate.Every(100 * time.Millisecond), // 10 requests per second
	}
}

type Client struct {
	config Config
	client *ws.Client
	logger zerolog.Logger

	subscriptions sync.Map // map[string]*Subscription
	limiter       *rate.Limiter

	ctx        context.Context
	cancelFunc context.CancelFunc
	wg         sync.WaitGroup

	mu        sync.RWMutex
	connected bool
}

type Subscription struct {
	Address  string
	Callback func(string)
	Sub      *ws.LogSubscription
}

func NewClient(cfg Config, logger zerolog.Logger) (*Client, error) {
	ctx, cancel := context.WithCancel(context.Background())

	c := &Client{
		config:     cfg,
		logger:     logger.With().Str("component", "websocket").Logger(),
		limiter:    rate.NewLimiter(cfg.RateLimit, 1),
		ctx:        ctx,
		cancelFunc: cancel,
	}

	if err := c.connect(); err != nil {
		cancel()
		return nil, fmt.Errorf("initial connection failed: %w", err)
	}

	c.startHealthCheck()
	return c, nil
}

func (c *Client) connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
	defer cancel()

	wsClient, err := ws.Connect(ctx, c.config.WSURL)
	if err != nil {
		return fmt.Errorf("ws connection failed: %w", err)
	}

	c.client = wsClient
	c.connected = true

	c.logger.Info().
		Str("url", c.config.WSURL).
		Msg("websocket connected successfully")

	return nil
}

func (c *Client) SubscribeToWallet(ctx context.Context, address string, callback func(string)) error {
	pubkey := solana.MustPublicKeyFromBase58(address)

	sub, err := c.client.LogsSubscribeMentions(pubkey, rpc.CommitmentConfirmed)
	if err != nil {
		return fmt.Errorf("subscription failed: %w", err)
	}

	subscription := &Subscription{
		Address:  address,
		Callback: callback,
		Sub:      sub,
	}

	c.subscriptions.Store(address, subscription)

	// Start monitoring in a separate goroutine managed by conc pool
	p := pool.New().WithContext(ctx).WithCancelOnError()
	p.Go(func(ctx context.Context) error {
		return c.monitorWallet(ctx, subscription)
	})

	return nil
}

func (c *Client) monitorWallet(ctx context.Context, sub *Subscription) error {
	logger := c.logger.With().Str("wallet", sub.Address).Logger()

	defer func() {
		if sub.Sub != nil {
			sub.Sub.Unsubscribe()
		}
		c.subscriptions.Delete(sub.Address)
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			if err := c.limiter.Wait(ctx); err != nil {
				return fmt.Errorf("rate limiter wait failed: %w", err)
			}

			got, err := sub.Sub.Recv(ctx)
			if err != nil {
				logger.Error().Err(err).Msg("error receiving log notification")

				// Check connection state before attempting reconnect
				c.mu.RLock()
				isConnected := c.connected && c.client != nil
				c.mu.RUnlock()

				if !isConnected || strings.Contains(err.Error(), "i/o timeout") {
					logger.Warn().Msg("detected connection issue, attempting reconnection")

					if err := c.reconnect(); err != nil {
						logger.Error().Err(err).Msg("reconnection failed")
						time.Sleep(time.Second) // Brief pause before next attempt
						continue
					}

					if err := c.resubscribe(sub); err != nil {
						logger.Error().Err(err).Msg("resubscription failed")
						time.Sleep(time.Second) // Brief pause before next attempt
						continue
					}

					logger.Info().Msg("successfully reconnected and resubscribed")
					continue
				}

				// For other types of errors, just continue monitoring
				continue
			}

			if got != nil && got.Value.Signature != (solana.Signature{}) {
				sub.Callback(got.Value.Signature.String())
			}
		}
	}
}

func (c *Client) reconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.client != nil {
		c.client.Close()
		c.client = nil
	}

	var err error
	for i := 0; i < c.config.MaxRetries; i++ {
		if i > 0 {
			backoff := exponentialBackoff(i, c.config.BackoffMin, c.config.BackoffMax)
			time.Sleep(backoff)
		}

		if err = c.connect(); err == nil {
			return nil
		}

		c.logger.Warn().
			Err(err).
			Int("attempt", i+1).
			Int("max_attempts", c.config.MaxRetries).
			Msg("reconnection attempt failed")
	}

	return fmt.Errorf("failed to reconnect after %d attempts: %w", c.config.MaxRetries, err)
}

func (c *Client) resubscribe(sub *Subscription) error {
	pubkey := solana.MustPublicKeyFromBase58(sub.Address)

	newSub, err := c.client.LogsSubscribeMentions(pubkey, rpc.CommitmentConfirmed)
	if err != nil {
		return fmt.Errorf("resubscription failed: %w", err)
	}

	sub.Sub = newSub
	return nil
}

func (c *Client) startHealthCheck() {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()

		ticker := time.NewTicker(c.config.HealthCheckFreq)
		defer ticker.Stop()

		for {
			select {
			case <-c.ctx.Done():
				return
			case <-ticker.C:
				if !c.isHealthy() {
					c.logger.Warn().Msg("unhealthy connection detected, attempting reconnect")
					if err := c.reconnect(); err != nil {
						c.logger.Error().Err(err).Msg("health check reconnection failed")
					}
				}
			}
		}
	}()
}

func (c *Client) isHealthy() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.connected || c.client == nil {
		return false
	}

	// Test connection with a dummy subscription
	_, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	testKey := solana.MustPublicKeyFromBase58("11111111111111111111111111111111")
	sub, err := c.client.LogsSubscribeMentions(testKey, rpc.CommitmentConfirmed)
	if err != nil {
		return false
	}
	sub.Unsubscribe()
	return true
}

func (c *Client) UnsubscribeFromWallet(ctx context.Context, address string) error {
	value, exists := c.subscriptions.LoadAndDelete(address)
	if !exists {
		return fmt.Errorf("no subscription found for address: %s", address)
	}

	sub, ok := value.(*Subscription)
	if !ok {
		return fmt.Errorf("invalid subscription type for address: %s", address)
	}

	if sub.Sub != nil {
		sub.Sub.Unsubscribe()
	}

	c.logger.Info().
		Str("wallet", address).
		Msg("unsubscribed from wallet")

	return nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Cancel context to stop all goroutines
	c.cancelFunc()

	// Unsubscribe all active subscriptions
	c.subscriptions.Range(func(key, value interface{}) bool {
		if sub, ok := value.(*Subscription); ok && sub.Sub != nil {
			sub.Sub.Unsubscribe()
		}
		c.subscriptions.Delete(key)
		return true
	})

	// Close the client connection
	if c.client != nil {
		c.client.Close()
		c.client = nil
	}

	c.connected = false

	// Wait for all goroutines to finish
	c.wg.Wait()

	return nil
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func exponentialBackoff(attempt int, min, max time.Duration) time.Duration {
	backoff := min * time.Duration(1<<uint(attempt))
	if backoff > max {
		return max
	}
	return backoff
}
