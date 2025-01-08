package websocket

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/ws"
	"github.com/rs/zerolog"
	"github.com/sony/gobreaker"
	"github.com/sourcegraph/conc"
)

type Config struct {
	WSURL                  string
	ReconnectInterval      time.Duration
	MaxReconnectAttempts   int
	CircuitBreakerSettings gobreaker.Settings
}

type connectionState int

const (
	disconnected connectionState = iota
	connecting
	connected
)

type WSClient struct {
	config Config
	logger zerolog.Logger
	client *ws.Client

	subscriptions   map[string]*Subscription
	subscriptionsMu sync.RWMutex

	done           chan struct{}
	reconnectWG    sync.WaitGroup
	circuitBreaker *gobreaker.CircuitBreaker

	wg conc.WaitGroup

	// Connection state management
	state   connectionState
	stateMu sync.RWMutex
}

// TransactionInfo contains relevant information about a transaction
type TransactionInfo struct {
	Signature string
	Slot      uint64
	Logs      []string
	Error     error
	Timestamp time.Time
}

type Subscription struct {
	callback   func(TransactionInfo)
	sub        *ws.LogSubscription
	commitment rpc.CommitmentType
}

// DefaultConfig returns a Config with sensible default values
func DefaultConfig() Config {
	return Config{
		ReconnectInterval:    5 * time.Second,
		MaxReconnectAttempts: 5,
		CircuitBreakerSettings: gobreaker.Settings{
			Name:        "solana-ws",
			MaxRequests: 3,
			Interval:    10 * time.Second,
			Timeout:     60 * time.Second,
			ReadyToTrip: func(counts gobreaker.Counts) bool {
				failureRatio := float64(counts.TotalFailures) / float64(counts.Requests)
				return counts.Requests >= 3 && failureRatio >= 0.6
			},
		},
	}
}

func NewClient(config Config, logger zerolog.Logger) (*WSClient, error) {
	// Validate required fields
	if config.WSURL == "" {
		return nil, fmt.Errorf("WSURL is required")
	}

	// Apply defaults for unset fields
	if config.ReconnectInterval == 0 {
		config.ReconnectInterval = DefaultConfig().ReconnectInterval
	}
	if config.MaxReconnectAttempts == 0 {
		config.MaxReconnectAttempts = DefaultConfig().MaxReconnectAttempts
	}

	// Check if CircuitBreakerSettings needs defaults
	defaultSettings := DefaultConfig().CircuitBreakerSettings
	if config.CircuitBreakerSettings.Name == "" &&
		config.CircuitBreakerSettings.MaxRequests == 0 &&
		config.CircuitBreakerSettings.Interval == 0 &&
		config.CircuitBreakerSettings.Timeout == 0 &&
		config.CircuitBreakerSettings.ReadyToTrip == nil {
		config.CircuitBreakerSettings = defaultSettings
	} else if config.CircuitBreakerSettings.ReadyToTrip == nil {
		// If CircuitBreakerSettings is provided but ReadyToTrip is not, use the default
		config.CircuitBreakerSettings.ReadyToTrip = defaultSettings.ReadyToTrip
	}

	return &WSClient{
		config:         config,
		logger:         logger,
		subscriptions:  make(map[string]*Subscription),
		done:           make(chan struct{}),
		circuitBreaker: gobreaker.NewCircuitBreaker(config.CircuitBreakerSettings),
	}, nil
}

func (c *WSClient) getState() connectionState {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.state
}

func (c *WSClient) setState(state connectionState) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.state = state
}

func (c *WSClient) Connect(ctx context.Context) error {
	// Check if already connected or connecting
	currentState := c.getState()
	if currentState == connected {
		return nil
	}
	if currentState == connecting {
		return fmt.Errorf("connection already in progress")
	}

	c.setState(connecting)

	client, err := ws.Connect(ctx, c.config.WSURL)
	if err != nil {
		c.setState(disconnected)
		return fmt.Errorf("failed to connect to WebSocket: %w", err)
	}

	c.client = client
	c.setState(connected)

	c.logger.Info().
		Str("url", c.config.WSURL).
		Msg("Successfully connected to WebSocket")

	return nil
}

func (c *WSClient) SubscribeToWallet(ctx context.Context, address string, callback func(TransactionInfo)) error {
	if c.client == nil {
		if err := c.Connect(ctx); err != nil {
			return fmt.Errorf("client not connected and failed to connect: %w", err)
		}
	}

	pubKey, err := solana.PublicKeyFromBase58(address)
	if err != nil {
		return fmt.Errorf("invalid wallet address: %w", err)
	}

	// Execute subscription through circuit breaker
	_, err = c.circuitBreaker.Execute(func() (interface{}, error) {
		sub, err := c.client.LogsSubscribeMentions(
			pubKey,
			rpc.CommitmentConfirmed,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to subscribe: %w", err)
		}

		c.subscriptionsMu.Lock()
		c.subscriptions[address] = &Subscription{
			callback:   callback,
			sub:        sub,
			commitment: rpc.CommitmentConfirmed,
		}
		c.subscriptionsMu.Unlock()

		// Start subscription handler
		c.wg.Go(func() {
			c.handleSubscription(address, sub)
		})

		return nil, nil
	})

	return err
}

func (c *WSClient) handleSubscription(address string, sub *ws.LogSubscription) {
	defer func() {
		c.subscriptionsMu.Lock()
		delete(c.subscriptions, address)
		c.subscriptionsMu.Unlock()
	}()

	for {
		select {
		case <-c.done:
			return
		default:
			notification, err := sub.Recv(context.Background())
			if err != nil {
				c.logger.Error().
					Err(err).
					Str("wallet", address).
					Msg("Error receiving log notification")

				if err := c.attemptReconnect(); err != nil {
					c.logger.Error().
						Err(err).
						Msg("Failed to reconnect")
					return
				}
				return
			}

			c.subscriptionsMu.RLock()
			subscription, exists := c.subscriptions[address]
			if exists && subscription.callback != nil {
				var txError error
				if notification.Value.Err != nil {
					txError = fmt.Errorf("%v", notification.Value.Err)
				}

				txInfo := TransactionInfo{
					Signature: notification.Value.Signature.String(),
					Slot:      notification.Context.Slot,
					Logs:      notification.Value.Logs,
					Error:     txError,
					Timestamp: time.Now(),
				}
				subscription.callback(txInfo)
			}
			c.subscriptionsMu.RUnlock()
		}
	}
}

func (c *WSClient) UnsubscribeFromWallet(ctx context.Context, address string) error {
	c.subscriptionsMu.Lock()
	defer c.subscriptionsMu.Unlock()

	sub, exists := c.subscriptions[address]
	if !exists {
		return fmt.Errorf("no subscription found for address: %s", address)
	}

	sub.sub.Unsubscribe()

	delete(c.subscriptions, address)
	return nil
}

func (c *WSClient) attemptReconnect() error {
	c.reconnectWG.Add(1)
	defer c.reconnectWG.Done()

	// Reset state and client
	c.setState(disconnected)
	if c.client != nil {
		c.client.Close()
		c.client = nil
	}

	attempts := 0
	for attempts < c.config.MaxReconnectAttempts {
		c.logger.Info().
			Int("attempt", attempts+1).
			Int("maxAttempts", c.config.MaxReconnectAttempts).
			Msg("Attempting to reconnect")

		if err := c.Connect(context.Background()); err != nil {
			c.logger.Error().Err(err).Msg("Reconnection attempt failed")
			attempts++
			time.Sleep(c.config.ReconnectInterval)
			continue
		}

		if err := c.resubscribeAll(); err != nil {
			c.logger.Error().
				Err(err).
				Msg("Failed to resubscribe wallets")

			// Close the connection and try again
			if c.client != nil {
				c.client.Close()
				c.client = nil
			}
			c.setState(disconnected)

			attempts++
			time.Sleep(c.config.ReconnectInterval)
			continue
		}

		c.logger.Info().Msg("Successfully reconnected and resubscribed")
		return nil
	}

	return fmt.Errorf("max reconnection attempts reached")
}

func (c *WSClient) resubscribeAll() error {
	c.subscriptionsMu.RLock()
	subscriptions := make(map[string]func(TransactionInfo))
	for address, sub := range c.subscriptions {
		subscriptions[address] = sub.callback
	}
	c.subscriptionsMu.RUnlock()

	for address, callback := range subscriptions {
		if err := c.SubscribeToWallet(context.Background(), address, callback); err != nil {
			return fmt.Errorf("failed to resubscribe wallet %s: %w", address, err)
		}
	}

	return nil
}

func (c *WSClient) Close() error {
	close(c.done)

	c.reconnectWG.Wait()
	c.wg.Wait()

	if c.client != nil {
		c.setState(disconnected)
		c.client.Close()
		c.client = nil
	}

	return nil
}
