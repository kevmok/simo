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
	"golang.org/x/exp/rand"
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

// SubscriptionStatus represents the current status of a subscription
type SubscriptionStatus struct {
	IsActive       bool      `json:"is_active"`
	LastActivity   time.Time `json:"last_activity"`
	ErrorCount     int       `json:"error_count"`
	IsReconnecting bool      `json:"is_reconnecting"`
}

// ConnectionStatus represents the current status of the WebSocket connection
type ConnectionStatus struct {
	State               string    `json:"state"`
	LastConnected       time.Time `json:"last_connected"`
	ReconnectCount      int       `json:"reconnect_count"`
	CircuitBreaker      string    `json:"circuit_breaker"`
	ActiveSubscriptions int       `json:"active_subscriptions"`
}

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
	state          connectionState
	stateMu        sync.RWMutex
	lastConnected  time.Time
	reconnectCount int
	reconnectMu    sync.Mutex // Mutex to prevent multiple simultaneous reconnection attempts

	// Subscription monitoring
	subscriptionStatus map[string]*SubscriptionStatus
	statusMu           sync.RWMutex
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

	config.CircuitBreakerSettings.OnStateChange = func(name string, from, to gobreaker.State) {
		logger.Info().
			Str("from", from.String()).
			Str("to", to.String()).
			Msg("Circuit breaker state changed")
	}

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
		config:             config,
		logger:             logger,
		subscriptions:      make(map[string]*Subscription),
		subscriptionStatus: make(map[string]*SubscriptionStatus),
		done:               make(chan struct{}),
		circuitBreaker:     gobreaker.NewCircuitBreaker(config.CircuitBreakerSettings),
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
	if state == connected && c.state != connected {
		c.lastConnected = time.Now()
	}
	c.state = state
}

// GetConnectionStatus returns the current status of the WebSocket connection
func (c *WSClient) GetConnectionStatus() ConnectionStatus {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()

	c.subscriptionsMu.RLock()
	activeCount := len(c.subscriptions)
	c.subscriptionsMu.RUnlock()

	var stateStr string
	switch c.state {
	case connected:
		stateStr = "connected"
	case connecting:
		stateStr = "connecting"
	case disconnected:
		stateStr = "disconnected"
	}

	return ConnectionStatus{
		State:               stateStr,
		LastConnected:       c.lastConnected,
		ReconnectCount:      c.reconnectCount,
		CircuitBreaker:      c.circuitBreaker.State().String(),
		ActiveSubscriptions: activeCount,
	}
}

// GetSubscriptionStatus returns the status of a specific subscription
func (c *WSClient) GetSubscriptionStatus(address string) (*SubscriptionStatus, error) {
	c.statusMu.RLock()
	status, exists := c.subscriptionStatus[address]
	c.statusMu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("no subscription found for address: %s", address)
	}

	return status, nil
}

// GetAllSubscriptionStatuses returns the status of all active subscriptions
func (c *WSClient) GetAllSubscriptionStatuses() map[string]*SubscriptionStatus {
	c.statusMu.RLock()
	defer c.statusMu.RUnlock()

	// Create a copy of the status map to avoid concurrent access issues
	statuses := make(map[string]*SubscriptionStatus)
	for addr, status := range c.subscriptionStatus {
		statusCopy := *status // Create a copy of the status
		statuses[addr] = &statusCopy
	}

	return statuses
}

// updateSubscriptionStatus updates the status of a subscription
func (c *WSClient) updateSubscriptionStatus(address string, update func(*SubscriptionStatus)) {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()

	status, exists := c.subscriptionStatus[address]
	if !exists {
		status = &SubscriptionStatus{
			IsActive:       true,
			LastActivity:   time.Now(),
			ErrorCount:     0,
			IsReconnecting: false,
		}
		c.subscriptionStatus[address] = status
	}

	update(status)
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
			defer func() {
				if r := recover(); r != nil {
					c.logger.Error().
						Str("address", address).
						Interface("panic", r).
						Msg("Recovered from panic in subscription handler")
				}
			}()
			c.handleSubscription(address, sub)
		})

		return nil, nil
	})

	return err
}

func (c *WSClient) handleSubscription(address string, sub *ws.LogSubscription) {
	// Initialize subscription status
	c.updateSubscriptionStatus(address, func(status *SubscriptionStatus) {
		status.IsActive = true
		status.LastActivity = time.Now()
		status.ErrorCount = 0
		status.IsReconnecting = false
	})

	defer func() {
		// Clean up subscription status
		c.updateSubscriptionStatus(address, func(status *SubscriptionStatus) {
			status.IsActive = false
			status.IsReconnecting = false
		})

		// Remove subscription from map
		c.subscriptionsMu.Lock()
		delete(c.subscriptions, address)
		c.subscriptionsMu.Unlock()

		// Recover from any panics
		if r := recover(); r != nil {
			c.logger.Error().
				Str("address", address).
				Interface("panic", r).
				Msg("Recovered from panic in subscription handler")
		}
	}()

	consecutiveErrors := 0
	errorBackoff := c.config.ReconnectInterval

	for {
		select {
		case <-c.done:
			c.logger.Debug().
				Str("address", address).
				Msg("Subscription handler exiting due to shutdown")
			return
		default:
			// Create a channel to receive the result of Recv()
			recvResult := make(chan struct {
				notification *ws.LogResult
				err          error
			}, 1)

			// Run Recv() in a goroutine to prevent blocking
			go func() {
				// Create a timeout context for the receive operation
				recvCtx, recvCancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer recvCancel()

				notif, err := sub.Recv(recvCtx)
				select {
				case recvResult <- struct {
					notification *ws.LogResult
					err          error
				}{notif, err}:
				default:
					// If the channel is blocked, log the error and return
					c.logger.Warn().Msg("Failed to send receive result, channel might be blocked")
				}
			}()

			// Wait for either shutdown signal or receive result
			select {
			case <-c.done:
				c.logger.Debug().
					Str("address", address).
					Msg("Subscription handler exiting due to shutdown")
				return
			case result := <-recvResult:
				if result.err != nil {
					// Handle error case
					c.handleSubscriptionError(address, result.err, &consecutiveErrors, &errorBackoff)
					continue
				}

				// Reset error counters on successful receive
				consecutiveErrors = 0
				errorBackoff = c.config.ReconnectInterval

				// Process successful notification
				c.processNotification(address, result.notification)
			}
		}
	}
}

func (c *WSClient) handleSubscriptionError(address string, err error, consecutiveErrors *int, errorBackoff *time.Duration) {
	c.logger.Error().
		Err(err).
		Str("wallet", address).
		Int("consecutive_errors", *consecutiveErrors+1).
		Msg("Error receiving log notification")

	// Update status with error
	c.updateSubscriptionStatus(address, func(status *SubscriptionStatus) {
		status.ErrorCount++
	})

	// Check if subscription still exists
	c.subscriptionsMu.RLock()
	_, exists := c.subscriptions[address]
	c.subscriptionsMu.RUnlock()

	if !exists {
		c.logger.Info().
			Str("wallet", address).
			Msg("Subscription removed, stopping handler")
		return
	}

	*consecutiveErrors++
	const maxConsecutiveErrors = 3

	// Handle reconnection after max errors
	if *consecutiveErrors >= maxConsecutiveErrors {
		c.logger.Warn().
			Str("wallet", address).
			Int("consecutive_errors", *consecutiveErrors).
			Msg("Too many consecutive errors, attempting reconnect")

		c.updateSubscriptionStatus(address, func(status *SubscriptionStatus) {
			status.IsReconnecting = true
		})

		if err := c.attemptReconnect(); err != nil {
			c.logger.Error().
				Err(err).
				Msg("Failed to reconnect")
			return
		}
		return
	}

	// Exponential backoff with jitter
	sleepDuration := *errorBackoff + time.Duration(rand.Int63n(int64(*errorBackoff/2)))
	time.Sleep(sleepDuration)
	*errorBackoff = time.Duration(float64(*errorBackoff) * 1.5)
	if *errorBackoff > 30*time.Second {
		*errorBackoff = 30 * time.Second
	}
}

func (c *WSClient) processNotification(address string, notification *ws.LogResult) {
	c.updateSubscriptionStatus(address, func(status *SubscriptionStatus) {
		status.LastActivity = time.Now()
		status.IsReconnecting = false
	})

	c.subscriptionsMu.RLock()
	subscription, exists := c.subscriptions[address]
	c.subscriptionsMu.RUnlock()

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

		// Execute callback with panic protection
		func() {
			defer func() {
				if r := recover(); r != nil {
					c.logger.Error().
						Str("address", address).
						Interface("panic", r).
						Msg("Recovered from panic in callback")
				}
			}()
			subscription.callback(txInfo)
		}()
	}
}

func (c *WSClient) UnsubscribeFromWallet(ctx context.Context, address string) error {
	c.subscriptionsMu.Lock()
	sub, exists := c.subscriptions[address]
	if !exists {
		c.subscriptionsMu.Unlock()
		return fmt.Errorf("no subscription found for address: %s", address)
	}

	// Mark subscription for deletion before unlocking
	delete(c.subscriptions, address)
	c.subscriptionsMu.Unlock()

	// Unsubscribe after removing from map to prevent reconnection attempts
	if sub != nil && sub.sub != nil {
		sub.sub.Unsubscribe()
	}

	return nil
}

func (c *WSClient) attemptReconnect() error {
	// Use reconnectMu to prevent multiple simultaneous reconnection attempts
	if !c.reconnectMu.TryLock() {
		return fmt.Errorf("reconnection already in progress")
	}
	defer c.reconnectMu.Unlock()

	c.reconnectWG.Add(1)
	defer c.reconnectWG.Done()

	// Create a context with timeout for the entire reconnection process
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.config.MaxReconnectAttempts)*c.config.ReconnectInterval*2)
	defer cancel()

	// Reset state and client under state mutex
	c.stateMu.Lock()
	c.state = disconnected
	if c.client != nil {
		c.client.Close()
		c.client = nil
	}
	c.stateMu.Unlock()

	attempts := 0
	backoff := c.config.ReconnectInterval

	for attempts < c.config.MaxReconnectAttempts {
		c.logger.Debug().
			Int("attempt", attempts+1).
			Msg("Performing reconnection attempt")
		// Check if we should stop reconnecting
		select {
		case <-c.done:
			return fmt.Errorf("client is shutting down")
		case <-ctx.Done():
			return fmt.Errorf("reconnection timeout: %w", ctx.Err())
		default:
		}

		c.logger.Info().
			Int("attempt", attempts+1).
			Int("maxAttempts", c.config.MaxReconnectAttempts).
			Dur("backoff", backoff).
			Msg("Attempting to reconnect")

		// Try to connect with a timeout
		connectCtx, connectCancel := context.WithTimeout(ctx, 30*time.Second)
		err := c.Connect(connectCtx)
		connectCancel()

		if err != nil {
			if err.Error() == "connection already in progress" {
				// Another goroutine is handling the connection
				return err
			}
			c.logger.Error().Err(err).Msg("Reconnection attempt failed")
			attempts++
			// Exponential backoff with a cap
			backoff = time.Duration(float64(backoff) * 1.5)
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			time.Sleep(backoff)
			continue
		}

		// Try to resubscribe with a timeout
		resubCtx, resubCancel := context.WithTimeout(ctx, 30*time.Second)
		err = c.resubscribeAll(resubCtx)
		resubCancel()

		if err != nil {
			c.logger.Error().
				Err(err).
				Msg("Failed to resubscribe wallets")

			c.stateMu.Lock()
			if c.client != nil {
				c.client.Close()
				c.client = nil
			}
			c.state = disconnected
			c.stateMu.Unlock()

			attempts++
			time.Sleep(backoff)
			continue
		}

		c.logger.Info().
			Int("attempts", attempts+1).
			Msg("Successfully reconnected and resubscribed")
		return nil
	}

	return fmt.Errorf("max reconnection attempts reached after %d tries", attempts)
}

func (c *WSClient) resubscribeAll(ctx context.Context) error {
	c.subscriptionsMu.RLock()
	subscriptions := make(map[string]func(TransactionInfo))
	for address, sub := range c.subscriptions {
		subscriptions[address] = sub.callback
	}
	c.subscriptionsMu.RUnlock()

	for address, callback := range subscriptions {
		select {
		case <-ctx.Done():
			return fmt.Errorf("resubscribe operation cancelled: %w", ctx.Err())
		default:
			if err := c.SubscribeToWallet(ctx, address, callback); err != nil {
				return fmt.Errorf("failed to resubscribe wallet %s: %w", address, err)
			}
		}
	}

	return nil
}

func (c *WSClient) Close() error {
	// Signal all goroutines to stop
	close(c.done)

	// Create a channel to signal completion of cleanup
	done := make(chan struct{})

	go func() {
		// Unsubscribe from all subscriptions first
		c.subscriptionsMu.Lock()
		for address, sub := range c.subscriptions {
			if sub != nil && sub.sub != nil {
				sub.sub.Unsubscribe()
				c.logger.Info().
					Str("wallet", address).
					Msg("Unsubscribed wallet during shutdown")
			}
		}
		// Clear the subscriptions map
		c.subscriptions = make(map[string]*Subscription)
		c.subscriptionsMu.Unlock()

		// Wait for all goroutines to finish
		c.reconnectWG.Wait()
		c.wg.Wait()

		if c.client != nil {
			c.setState(disconnected)
			c.client.Close()
			c.client = nil
		}

		close(done)
	}()

	// Wait for cleanup with a timeout
	select {
	case <-done:
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("websocket client close timed out after 10 seconds")
	}
}
