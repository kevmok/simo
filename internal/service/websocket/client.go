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

	subscriptions sync.Map
	limiter       *rate.Limiter

	ctx        context.Context
	cancelFunc context.CancelFunc
	pool       *pool.ContextPool

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
		pool:       pool.New().WithContext(ctx).WithCancelOnError(),
	}

	if err := c.connect(); err != nil {
		cancel()
		return nil, fmt.Errorf("initial connection failed: %w", err)
	}

	// Single goroutine for health checks
	c.pool.Go(func(ctx context.Context) error {
		ticker := time.NewTicker(c.config.HealthCheckFreq)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				if err := c.checkAndReconnect(); err != nil {
					c.logger.Error().Err(err).Msg("reconnection failed")
				}
			}
		}
	})

	return c, nil
}

func (c *Client) checkAndReconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.client == nil || !c.connected {
		return c.reconnect()
	}

	// Simple ping test
	testKey := solana.MustPublicKeyFromBase58("11111111111111111111111111111111")
	if _, err := c.client.LogsSubscribeMentions(testKey, rpc.CommitmentConfirmed); err != nil {
		return c.reconnect()
	}

	return nil
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

	// Monitor in background
	c.pool.Go(func(ctx context.Context) error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				if err := c.limiter.Wait(ctx); err != nil {
					return err
				}

				got, err := subscription.Sub.Recv(ctx)
				if err != nil {
					// Trigger reconnect on error
					c.checkAndReconnect()
					time.Sleep(time.Second) // Brief pause before retry
					continue
				}

				if got != nil && got.Value.Signature != (solana.Signature{}) {
					callback(got.Value.Signature.String())
				}
			}
		}
	})

	return nil
}

func (c *Client) reconnect() error {
	if c.client != nil {
		c.client.Close()
		c.client = nil
	}
	c.connected = false

	// Try to reconnect
	if err := c.connect(); err != nil {
		return err
	}

	// Resubscribe all
	var resubError error
	c.subscriptions.Range(func(key, value interface{}) bool {
		sub := value.(*Subscription)
		if err := c.resubscribe(sub); err != nil {
			resubError = err
			return false
		}
		return true
	})

	return resubError
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
	c.cancelFunc()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Unsubscribe all
	c.subscriptions.Range(func(key, value interface{}) bool {
		if sub, ok := value.(*Subscription); ok && sub.Sub != nil {
			sub.Sub.Unsubscribe()
		}
		c.subscriptions.Delete(key)
		return true
	})

	if c.client != nil {
		c.client.Close()
		c.client = nil
	}

	c.connected = false
	c.pool.Wait()

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

func (c *Client) setConnectionState(connected bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connected != connected {
		c.connected = connected
		c.logger.Info().Bool("connected", connected).Msg("connection state changed")
	}
}
