package websocket

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/eapache/go-resiliency/breaker"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/ws"
	"github.com/rs/zerolog"
	"github.com/sourcegraph/conc/iter"
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

	// New connection-related configurations
	ConnTimeout  time.Duration // Timeout for establishing connections
	ReadTimeout  time.Duration // Timeout for read operations
	WriteTimeout time.Duration // Timeout for write operations
	BufferSize   int           // Size of message buffer channels

	// Concurrency controls
	WorkerCount int // Number of workers for subscription handling
	BatchSize   int // Size of batches for subscription operations

	// Circuit breaker settings
	FailureThreshold int           // Number of failures before circuit opens
	ResetTimeout     time.Duration // Time before attempting to close circuit
}

// DefaultConfig returns a default configuration
func DefaultConfig() Config {
	return Config{
		MaxRetries:      5,
		BackoffMin:      time.Second,
		BackoffMax:      time.Minute,
		HealthCheckFreq: 15 * time.Second,
		RateLimit:       rate.Every(100 * time.Millisecond),

		// New defaults
		ConnTimeout:      10 * time.Second,
		ReadTimeout:      5 * time.Second,
		WriteTimeout:     5 * time.Second,
		BufferSize:       1000,
		WorkerCount:      runtime.GOMAXPROCS(0), // Use available CPU cores
		BatchSize:        50,
		FailureThreshold: 5,
		ResetTimeout:     30 * time.Second,
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

	workerPool     *pool.ContextPool
	batchPool      *pool.ContextPool
	circuitBreaker *breaker.Breaker
	messageQueue   chan *Message
}

type Message struct {
	Address    string
	Payload    interface{}
	ReceivedAt time.Time
}

type Subscription struct {
	Address  string
	Callback func(string)
	Sub      *ws.LogSubscription
}

func NewClient(cfg Config, logger zerolog.Logger) (*Client, error) {
	// Create a cancellable context for the entire client
	ctx, cancel := context.WithCancel(context.Background())

	// Initialize the client with all required components
	c := &Client{
		// Basic configuration
		config:     cfg,
		logger:     logger.With().Str("component", "websocket").Logger(),
		ctx:        ctx,
		cancelFunc: cancel,

		// Rate limiting
		limiter: rate.NewLimiter(cfg.RateLimit, 1),

		// Connection state
		connected: false,

		// Concurrency components
		pool: pool.New().WithContext(ctx).WithCancelOnError(),
		workerPool: pool.New().WithContext(ctx).
			WithMaxGoroutines(cfg.WorkerCount).
			WithCancelOnError(),
		batchPool: pool.New().WithContext(ctx).
			WithMaxGoroutines(cfg.BatchSize).
			WithCancelOnError(),

		// Circuit breaker for connection management
		circuitBreaker: breaker.New(
			cfg.FailureThreshold,
			1, // One success required to close
			cfg.ResetTimeout,
		),

		// Message processing channel
		messageQueue: make(chan *Message, cfg.BufferSize),
	}

	// Establish initial connection
	if err := c.connect(); err != nil {
		cancel() // Clean up if connection fails
		return nil, fmt.Errorf("initial connection failed: %w", err)
	}

	// Start the message processing system
	c.pool.Go(func(ctx context.Context) error {
		return c.processSubscriptions(ctx)
	})

	// Start the health check monitoring
	c.pool.Go(func(ctx context.Context) error {
		ticker := time.NewTicker(c.config.HealthCheckFreq)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				if err := c.checkAndReconnect(); err != nil {
					c.logger.Error().
						Err(err).
						Msg("reconnection failed")
				}
			}
		}
	})

	// Log successful initialization
	c.logger.Info().
		Int("worker_count", cfg.WorkerCount).
		Int("batch_size", cfg.BatchSize).
		Int("buffer_size", cfg.BufferSize).
		Msg("websocket client initialized successfully")

	return c, nil
}

func (c *Client) checkAndReconnect() error {
	return c.circuitBreaker.Run(func() error {
		c.mu.Lock()
		defer c.mu.Unlock()

		if c.client == nil || !c.connected {
			return c.reconnect()
		}

		// Create a timeout context for the health check
		ctx, cancel := context.WithTimeout(c.ctx, c.config.ReadTimeout)
		defer cancel()

		// Use the context in the health check subscription
		testKey := solana.MustPublicKeyFromBase58("11111111111111111111111111111111")
		sub, err := c.client.LogsSubscribeMentions(testKey, rpc.CommitmentConfirmed)
		if err != nil {
			return c.reconnect()
		}

		// Important: We need to properly clean up the test subscription
		defer sub.Unsubscribe()

		// Test receiving a message with the timeout context
		select {
		case <-ctx.Done():
			return fmt.Errorf("health check timeout: %w", ctx.Err())
		default:
			// Try to receive one message to ensure the connection is truly alive
			if _, err := sub.Recv(ctx); err != nil {
				return c.reconnect()
			}
		}

		return nil
	})
}

func (c *Client) resubscribeAll() error {
	// First, collect all subscriptions into a slice since iter.ForEach works with slices
	var subs []*Subscription
	c.subscriptions.Range(func(key, value interface{}) bool {
		if sub, ok := value.(*Subscription); ok {
			subs = append(subs, sub)
		}
		return true
	})

	// Create a pool for parallel processing
	p := pool.New().
		WithContext(c.ctx).
		WithMaxGoroutines(c.config.WorkerCount).
		WithCancelOnError()

	// Use iter.ForEach for parallel processing
	iter.ForEach(subs, func(sub **Subscription) {
		p.Go(func(ctx context.Context) error {
			if err := c.resubscribe(*sub); err != nil {
				c.logger.Error().
					Err(err).
					Str("address", (*sub).Address).
					Msg("failed to resubscribe")
				return err
			}
			return nil
		})
	})

	return p.Wait()
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
	if c.client == nil {
		c.logger.Error().Msg("Cannot subscribe: websocket client is nil")
		return fmt.Errorf("websocket client not initialized")
	}

	c.logger.Info().
		Str("address", address).
		Msg("Starting wallet subscription")

	pubkey := solana.MustPublicKeyFromBase58(address)

	// Execute subscription through circuit breaker
	err := c.circuitBreaker.Run(func() error {
		if c.client == nil {
			return fmt.Errorf("websocket client is nil")
		}

		sub, err := c.client.LogsSubscribeMentions(pubkey, rpc.CommitmentConfirmed)
		if err != nil {
			c.logger.Error().
				Err(err).
				Str("address", address).
				Msg("Subscription failed")
			return fmt.Errorf("subscription failed: %w", err)
		}

		if sub == nil {
			return fmt.Errorf("received nil subscription")
		}

		subscription := &Subscription{
			Address:  address,
			Callback: callback,
			Sub:      sub,
		}

		c.subscriptions.Store(address, subscription)

		// Send messages to the processing queue
		c.pool.Go(func(ctx context.Context) error {
			defer func() {
				c.logger.Debug().
					Str("address", address).
					Msg("Cleaning up subscription")
				if sub != nil {
					subscription.Sub.Unsubscribe()
				}
				c.subscriptions.Delete(address)
			}()

			for {
				select {
				case <-ctx.Done():
					c.logger.Debug().
						Str("address", address).
						Msg("Subscription context canceled")
					return ctx.Err()
				default:
					if err := c.limiter.Wait(ctx); err != nil {
						c.logger.Debug().
							Str("address", address).
							Err(err).
							Msg("Rate limit exceeded")
						return err
					}

					got, err := subscription.Sub.Recv(ctx)
					if err != nil {
						// Use exponential backoff for reconnection
						for attempt := 0; attempt < c.config.MaxRetries; attempt++ {
							if err := c.checkAndReconnect(); err != nil {
								backoff := exponentialBackoff(attempt, c.config.BackoffMin, c.config.BackoffMax)
								c.logger.Debug().
									Str("address", address).
									Err(err).
									Int("attempt", attempt).
									Dur("backoff", backoff).
									Msg("Reconnection attempt failed, waiting before retry")
								time.Sleep(backoff)
								continue
							}
							c.logger.Info().
								Str("address", address).
								Int("attempt", attempt).
								Msg("Reconnection successful")
							break
						}
						continue
					}

					if got != nil && got.Value.Signature != (solana.Signature{}) {
						// Send to message queue for processing
						select {
						case c.messageQueue <- &Message{
							Address:    address,
							Payload:    got.Value.Signature.String(),
							ReceivedAt: time.Now(),
						}:
							c.logger.Debug().
								Str("address", address).
								Str("signature", got.Value.Signature.String()).
								Msg("Message queued for processing")
						default:
							c.logger.Warn().Msg("message queue full, dropping message")
							c.logger.Warn().
								Str("address", address).
								Str("signature", got.Value.Signature.String()).
								Msg("Message queue full, dropping message")
						}
					}
				}
			}
		})

		return nil
	})

	if err != nil {
		c.logger.Error().
			Err(err).
			Str("address", address).
			Msg("Wallet subscription failed")
		return fmt.Errorf("wallet subscription failed: %w", err)
	}

	c.logger.Info().
		Str("address", address).
		Msg("Wallet subscription completed successfully")

	return nil
}

func (c *Client) reconnect() error {
	c.logger.Info().Msg("Starting reconnection process")

	// Clean up old client
	if c.client != nil {
		c.logger.Debug().Msg("Closing old connection")
		c.client.Close()
		c.client = nil
		c.logger.Debug().Msg("Old connection closed successfully")
	} else {
		c.logger.Debug().Msg("No existing client to close")
	}
	c.connected = false

	// Track subscription count for logging
	var subCount int
	// Clean up old subscriptions with logging
	c.subscriptions.Range(func(key, value interface{}) bool {
		if sub, ok := value.(*Subscription); ok {
			if sub.Sub != nil {
				c.logger.Debug().
					Str("address", sub.Address).
					Msg("Unsubscribing wallet during reconnection")
				sub.Sub.Unsubscribe()
				sub.Sub = nil
				subCount++
			}
		}
		return true
	})
	c.logger.Debug().
		Int("subscription_count", subCount).
		Msg("Cleaned up existing subscriptions")

	// Try to reconnect with timeout context
	c.logger.Info().Msg("Attempting to establish new connection")
	if err := c.connect(); err != nil {
		c.logger.Error().
			Err(err).
			Msg("Failed to establish new connection")
		return fmt.Errorf("reconnection failed: %w", err)
	}
	c.logger.Info().Msg("Successfully established new connection")

	// Get current subscriptions for resubscription
	var subs []*Subscription
	c.subscriptions.Range(func(key, value interface{}) bool {
		if sub, ok := value.(*Subscription); ok {
			subs = append(subs, sub)
		}
		return true
	})

	c.logger.Info().
		Int("subscription_count", len(subs)).
		Msg("Starting resubscription process")

	// Resubscribe all with error tracking
	var resubErrors []error
	for _, sub := range subs {
		c.logger.Debug().
			Str("address", sub.Address).
			Msg("Attempting to resubscribe wallet")

		if err := c.resubscribe(sub); err != nil {
			c.logger.Error().
				Err(err).
				Str("address", sub.Address).
				Msg("Failed to resubscribe wallet")
			resubErrors = append(resubErrors, fmt.Errorf("failed to resubscribe %s: %w", sub.Address, err))
		} else {
			c.logger.Debug().
				Str("address", sub.Address).
				Msg("Successfully resubscribed wallet")
		}
	}

	if len(resubErrors) > 0 {
		// Combine all resubscription errors
		return fmt.Errorf("reconnection completed with resubscription errors: %v", resubErrors)
	}

	c.logger.Info().Msg("Reconnection process completed successfully")
	return nil
}

// Improve the resubscribe function too
func (c *Client) resubscribe(sub *Subscription) error {
	if sub == nil {
		return fmt.Errorf("cannot resubscribe nil subscription")
	}

	c.logger.Debug().
		Str("address", sub.Address).
		Msg("Attempting resubscription")

	pubkey := solana.MustPublicKeyFromBase58(sub.Address)

	// Add timeout context for subscription
	_, cancel := context.WithTimeout(c.ctx, c.config.ConnTimeout)
	defer cancel()

	newSub, err := c.client.LogsSubscribeMentions(pubkey, rpc.CommitmentConfirmed)
	if err != nil {
		return fmt.Errorf("resubscription failed: %w", err)
	}

	if newSub == nil {
		return fmt.Errorf("received nil subscription")
	}

	// Clean up old subscription if it exists
	if sub.Sub != nil {
		sub.Sub.Unsubscribe()
	}

	sub.Sub = newSub
	c.logger.Debug().
		Str("address", sub.Address).
		Msg("Resubscription successful")

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
	// Cancel context first to stop all operations
	c.cancelFunc()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Unsubscribe and clean up all subscriptions
	c.subscriptions.Range(func(key, value interface{}) bool {
		if sub, ok := value.(*Subscription); ok && sub.Sub != nil {
			c.logger.Debug().
				Str("address", sub.Address).
				Msg("unsubscribing from wallet")
			sub.Sub.Unsubscribe()
			sub.Sub = nil
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

func (c *Client) processSubscriptions(ctx context.Context) error {
	// Create a worker pool for processing messages
	c.workerPool = pool.New().WithContext(ctx).WithMaxGoroutines(c.config.WorkerCount).WithCancelOnError()

	// Create a buffered channel for messages
	c.messageQueue = make(chan *Message, c.config.BufferSize)

	// Start workers to process messages
	for i := 0; i < c.config.WorkerCount; i++ {
		c.workerPool.Go(func(ctx context.Context) error {
			return c.processMessages(ctx)
		})
	}

	return c.workerPool.Wait()
}

func (c *Client) processMessages(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg := <-c.messageQueue:
			// Process message in batches using conc
			if err := c.processBatch(ctx, msg); err != nil {
				c.logger.Error().Err(err).Msg("batch processing failed")
				continue
			}
		}
	}
}

func (c *Client) processBatch(ctx context.Context, msg *Message) error {
	// Look up only the specific subscription for this message
	value, exists := c.subscriptions.Load(msg.Address)
	if !exists {
		c.logger.Debug().
			Str("address", msg.Address).
			Msg("No subscription found for address")
		return nil
	}

	sub, ok := value.(*Subscription)
	if !ok {
		return fmt.Errorf("invalid subscription type for address: %s", msg.Address)
	}

	// Create a context with timeout for the operation
	msgCtx, cancel := context.WithTimeout(ctx, c.config.ReadTimeout)
	defer cancel()

	return c.handleSubscriptionMessage(msgCtx, sub, msg)
}

func (c *Client) handleSubscriptionMessage(ctx context.Context, sub *Subscription, msg *Message) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		if err := c.limiter.Wait(ctx); err != nil {
			return fmt.Errorf("rate limit exceeded: %w", err)
		}

		// Additional validation to ensure we're processing the right message
		if sub.Address != msg.Address {
			return fmt.Errorf("address mismatch: expected %s, got %s", sub.Address, msg.Address)
		}

		sub.Callback(msg.Payload.(string))
		return nil
	}
}
