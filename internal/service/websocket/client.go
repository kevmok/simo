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

	pool        *pool.ContextPool // conc pool for managing goroutines
	reconnectCh chan struct{}
	healthyCh   chan struct{}

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
		config:      cfg,
		logger:      logger.With().Str("component", "websocket").Logger(),
		limiter:     rate.NewLimiter(cfg.RateLimit, 1),
		ctx:         ctx,
		cancelFunc:  cancel,
		pool:        pool.New().WithContext(ctx).WithCancelOnError(),
		reconnectCh: make(chan struct{}, 1),
		healthyCh:   make(chan struct{}, 1),
	}

	if err := c.connect(); err != nil {
		cancel()
		return nil, fmt.Errorf("initial connection failed: %w", err)
	}

	c.startConnectionManager()
	return c, nil
}

func (c *Client) startConnectionManager() {
	c.pool.Go(func(ctx context.Context) error {
		healthTicker := time.NewTicker(c.config.HealthCheckFreq)
		defer healthTicker.Stop()

		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-healthTicker.C:
				if !c.isHealthy() {
					c.triggerReconnect()
				}
			case <-c.reconnectCh:
				if err := c.handleReconnection(); err != nil {
					c.logger.Error().Err(err).Msg("reconnection failed")
				}
			}
		}
	})
}
func (c *Client) triggerReconnect() {
	select {
	case c.reconnectCh <- struct{}{}:
	default:
		// Reconnection already queued
	}
}

func (c *Client) handleReconnection() error {
	c.logger.Info().Msg("starting reconnection process")

	backoffDuration := c.config.BackoffMin
	attempts := 0

	for attempts < c.config.MaxRetries {
		if err := c.reconnect(); err == nil {
			if err := c.resubscribeAll(); err == nil {
				c.logger.Info().Msg("successfully reconnected and resubscribed")
				return nil
			} else {
				c.logger.Error().Err(err).Msg("resubscription failed")
			}
		}

		attempts++
		backoffDuration = min(backoffDuration*2, c.config.BackoffMax)

		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		case <-time.After(backoffDuration):
			continue
		}
	}

	return fmt.Errorf("failed to reconnect after %d attempts", c.config.MaxRetries)
}

func (c *Client) resubscribeAll() error {
	p := pool.New().WithContext(c.ctx).WithCancelOnError()

	c.subscriptions.Range(func(key, value interface{}) bool {
		sub, ok := value.(*Subscription)
		if !ok {
			return true
		}

		p.Go(func(ctx context.Context) error {
			if err := c.resubscribe(sub); err != nil {
				return fmt.Errorf("failed to resubscribe %s: %w", sub.Address, err)
			}
			return nil
		})
		return true
	})

	// Wait for all resubscriptions and collect errors
	if err := p.Wait(); err != nil {
		return fmt.Errorf("resubscription failed: %w", err)
	}

	return nil
}

func (c *Client) connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.logger.Debug().Msg("attempting to establish websocket connection")

	ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
	defer cancel()

	wsClient, err := ws.Connect(ctx, c.config.WSURL)
	if err != nil {
		c.logger.Error().
			Err(err).
			Str("url", c.config.WSURL).
			Msg("websocket connection failed")
		return fmt.Errorf("ws connection failed: %w", err)
	}

	c.client = wsClient
	c.connected = true

	c.logger.Info().
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

	c.pool.Go(func(ctx context.Context) error {
		return c.monitorWallet(ctx, subscription)
	})

	return nil
}

func (c *Client) monitorWallet(ctx context.Context, sub *Subscription) error {
	logger := c.logger.With().Str("wallet", sub.Address).Logger()
	logger.Info().Msg("starting wallet monitoring")

	defer func() {
		if r := recover(); r != nil {
			logger.Error().
				Interface("panic", r).
				Msg("recovered from panic in monitorWallet")
		}
		if sub.Sub != nil {
			logger.Debug().Msg("unsubscribing wallet subscription")
			sub.Sub.Unsubscribe()
		}
		c.subscriptions.Delete(sub.Address)
		logger.Info().Msg("wallet monitoring stopped")
	}()

	reconnectAttempts := 0
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
				reconnectAttempts++
				logger.Error().
					Err(err).
					Int("reconnect_attempt", reconnectAttempts).
					Msg("error receiving log notification")

				c.triggerReconnect()

				select {
				case <-c.healthyCh:
					reconnectAttempts = 0
					continue
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(30 * time.Second):
					logger.Warn().Msg("timeout waiting for healthy connection")
					if reconnectAttempts > c.config.MaxRetries {
						return fmt.Errorf("max reconnection attempts reached")
					}
					continue
				}
			}

			reconnectAttempts = 0
			if got != nil && got.Value.Signature != (solana.Signature{}) {
				logger.Debug().
					Str("signature", got.Value.Signature.String()).
					Msg("received transaction notification")
				sub.Callback(got.Value.Signature.String())
			}
		}
	}
}

func (c *Client) reconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.client != nil {
		c.logger.Info().Msg("closing existing connection")
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

	c.logger.Info().Msg("initiating websocket client shutdown")

	// Cancel context first to stop all operations
	c.cancelFunc()

	// Clear channels
	for len(c.reconnectCh) > 0 {
		<-c.reconnectCh
	}
	for len(c.healthyCh) > 0 {
		<-c.healthyCh
	}

	// Unsubscribe all active subscriptions
	activeSubscriptions := 0
	c.subscriptions.Range(func(key, value interface{}) bool {
		if sub, ok := value.(*Subscription); ok && sub.Sub != nil {
			c.logger.Debug().
				Str("address", sub.Address).
				Msg("unsubscribing from wallet")
			sub.Sub.Unsubscribe()
			activeSubscriptions++
		}
		c.subscriptions.Delete(key)
		return true
	})

	// Close the client connection
	if c.client != nil {
		c.logger.Debug().Msg("closing websocket connection")
		c.client.Close()
		c.client = nil
	}

	c.connected = false

	// Wait for all goroutines to finish
	c.pool.Wait()

	c.logger.Info().
		Int("unsubscribed_wallets", activeSubscriptions).
		Msg("websocket client shutdown complete")

	return nil
}

func (c *Client) signalHealthy() {
	select {
	case c.healthyCh <- struct{}{}:
	default:
	}
}

func (c *Client) isHealthy() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if !c.connected || c.client == nil {
		c.logger.Debug().Msg("connection check failed: client not connected")
		return false
	}

	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()

	done := make(chan bool, 1)
	cleanup := make(chan struct{})

	go func() {
		defer close(done)
		testKey := solana.MustPublicKeyFromBase58("11111111111111111111111111111111")
		sub, err := c.client.LogsSubscribeMentions(testKey, rpc.CommitmentConfirmed)
		if err != nil {
			c.logger.Debug().Err(err).Msg("health check subscription failed")
			done <- false
			return
		}

		select {
		case <-cleanup:
			// Ensure cleanup happens if context is cancelled
			if sub != nil {
				sub.Unsubscribe()
			}
		case <-ctx.Done():
			if sub != nil {
				sub.Unsubscribe()
			}
		default:
			if sub != nil {
				sub.Unsubscribe()
			}
			done <- true
		}
	}()

	select {
	case result := <-done:
		close(cleanup)
		if result {
			c.signalHealthy()
		}
		return result
	case <-ctx.Done():
		close(cleanup)
		c.logger.Debug().Msg("health check timed out")
		return false
	}
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
