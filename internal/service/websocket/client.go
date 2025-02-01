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
	"golang.org/x/sync/errgroup"
)

type Config struct {
	WSURL                string
	ReconnectInterval    time.Duration
	MaxReconnectAttempts int
}

type SubscriptionStatus struct {
	IsActive       bool      `json:"is_active"`
	LastActivity   time.Time `json:"last_activity"`
	ErrorCount     int       `json:"error_count"`
	IsReconnecting bool      `json:"is_reconnecting"`
}

type ConnectionStatus struct {
	State               string    `json:"state"`
	LastConnected       time.Time `json:"last_connected"`
	ReconnectCount      int       `json:"reconnect_count"`
	ActiveSubscriptions int       `json:"active_subscriptions"`
}

type WSClient struct {
	config        Config
	logger        zerolog.Logger
	client        *ws.Client
	clientMu      sync.RWMutex
	subscriptions map[string]*Subscription
	subsMu        sync.RWMutex
	status        map[string]*SubscriptionStatus
	statusMu      sync.RWMutex
	connStatus    ConnectionStatus
	connStatusMu  sync.RWMutex
	ctx           context.Context
	cancel        context.CancelFunc
	group         *errgroup.Group
}

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
	ctx        context.Context
	cancel     context.CancelFunc
}

func DefaultConfig() Config {
	return Config{
		ReconnectInterval:    5 * time.Second,
		MaxReconnectAttempts: 5,
	}
}

func NewClient(config Config, logger zerolog.Logger) (*WSClient, error) {
	if config.WSURL == "" {
		return nil, fmt.Errorf("WSURL is required")
	}

	ctx, cancel := context.WithCancel(context.Background())
	eg, ctx := errgroup.WithContext(ctx)

	return &WSClient{
		config:        config,
		logger:        logger,
		subscriptions: make(map[string]*Subscription),
		status:        make(map[string]*SubscriptionStatus),
		ctx:           ctx,
		cancel:        cancel,
		group:         eg,
	}, nil
}

func (c *WSClient) updateConnectionState(state string) {
	c.connStatusMu.Lock()
	defer c.connStatusMu.Unlock()

	c.connStatus.State = state
	if state == "connected" {
		c.connStatus.LastConnected = time.Now()
		c.connStatus.ReconnectCount = 0
	}
}

func (c *WSClient) GetConnectionStatus() ConnectionStatus {
	c.connStatusMu.RLock()
	defer c.connStatusMu.RUnlock()

	status := c.connStatus
	status.ActiveSubscriptions = len(c.status)
	return status
}

func (c *WSClient) updateSubscriptionStatus(addr string, update func(*SubscriptionStatus)) {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()

	status, exists := c.status[addr]
	if !exists {
		status = &SubscriptionStatus{IsActive: true}
		c.status[addr] = status
	}

	update(status)
	status.LastActivity = time.Now()
}

func (c *WSClient) GetSubscriptionStatus(addr string) (*SubscriptionStatus, error) {
	c.statusMu.RLock()
	defer c.statusMu.RUnlock()

	status, exists := c.status[addr]
	if !exists {
		return nil, fmt.Errorf("subscription not found")
	}
	return &SubscriptionStatus{
		IsActive:       status.IsActive,
		LastActivity:   status.LastActivity,
		ErrorCount:     status.ErrorCount,
		IsReconnecting: status.IsReconnecting,
	}, nil
}

func (c *WSClient) Connect() error {
	c.clientMu.Lock()
	defer c.clientMu.Unlock()

	if c.client != nil {
		return nil
	}

	c.updateConnectionState("connecting")
	client, err := ws.Connect(c.ctx, c.config.WSURL)
	if err != nil {
		c.updateConnectionState("disconnected")
		return fmt.Errorf("connection failed: %w", err)
	}

	c.client = client
	c.updateConnectionState("connected")
	return nil
}

func (c *WSClient) SubscribeToWallet(addr string, callback func(TransactionInfo)) error {
	if err := c.Connect(); err != nil {
		return err
	}

	pubKey, err := solana.PublicKeyFromBase58(addr)
	if err != nil {
		return fmt.Errorf("invalid address: %w", err)
	}

	// Create subscription context with cancellation
	subCtx, cancel := context.WithCancel(c.ctx)

	sub, err := c.client.LogsSubscribeMentions(pubKey, rpc.CommitmentConfirmed)
	if err != nil {
		cancel()
		return fmt.Errorf("subscription failed: %w", err)
	}

	c.subsMu.Lock()
	c.subscriptions[addr] = &Subscription{
		callback:   callback,
		sub:        sub,
		commitment: rpc.CommitmentConfirmed,
		ctx:        subCtx,
		cancel:     cancel,
	}
	c.subsMu.Unlock()

	c.updateSubscriptionStatus(addr, func(s *SubscriptionStatus) {
		s.IsActive = true
	})

	c.group.Go(func() error {
		return c.handleSubscription(subCtx, addr, sub)
	})

	return nil
}

func (c *WSClient) handleSubscription(ctx context.Context, addr string, sub *ws.LogSubscription) error {
	defer func() {
		c.subsMu.Lock()
		delete(c.subscriptions, addr)
		c.subsMu.Unlock()

		c.updateSubscriptionStatus(addr, func(s *SubscriptionStatus) {
			s.IsActive = false
		})
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			notif, err := sub.Recv(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				c.handleSubscriptionError(addr, err)
				return err
			}
			c.processNotification(addr, notif)
		}
	}
}

func (c *WSClient) handleSubscriptionError(addr string, err error) {
	c.logger.Error().Err(err).Str("address", addr).Msg("Subscription error")

	c.updateSubscriptionStatus(addr, func(s *SubscriptionStatus) {
		s.ErrorCount++
		s.IsReconnecting = true
	})

	c.triggerReconnect()
}

func (c *WSClient) GetAllSubscriptionStatuses() map[string]*SubscriptionStatus {
	c.statusMu.RLock()
	defer c.statusMu.RUnlock()

	statuses := make(map[string]*SubscriptionStatus)
	for addr, status := range c.status {
		// Create a copy for each status
		statusCopy := &SubscriptionStatus{
			IsActive:       status.IsActive,
			LastActivity:   status.LastActivity,
			ErrorCount:     status.ErrorCount,
			IsReconnecting: status.IsReconnecting,
		}
		statuses[addr] = statusCopy
	}
	return statuses
}

func (c *WSClient) processNotification(addr string, notification *ws.LogResult) {
	// Status update
	c.updateSubscriptionStatus(addr, func(s *SubscriptionStatus) {
		s.LastActivity = time.Now()
		s.IsReconnecting = false
	})

	// Get subscription
	c.subsMu.RLock()
	sub, exists := c.subscriptions[addr]
	c.subsMu.RUnlock()

	if exists && sub.callback != nil {
		// Error handling
		var txError error
		if notification.Value.Err != nil {
			txError = fmt.Errorf("%v", notification.Value.Err)
		}

		// Create transaction info
		txInfo := TransactionInfo{
			Signature: notification.Value.Signature.String(),
			Slot:      notification.Context.Slot,
			Logs:      notification.Value.Logs,
			Error:     txError,
			Timestamp: time.Now(),
		}

		// Safe callback execution
		defer func() {
			if r := recover(); r != nil {
				c.logger.Error().
					Str("address", addr).
					Interface("panic", r).
					Msg("Callback panic recovered")
			}
		}()
		sub.callback(txInfo)
	}
}

func (c *WSClient) triggerReconnect() {
	c.clientMu.Lock()
	defer c.clientMu.Unlock()

	if c.client == nil {
		return
	}

	c.updateConnectionState("reconnecting")
	c.client.Close()
	c.client = nil

	go func() {
		backoff := c.config.ReconnectInterval
		attempts := 0
		for attempts < c.config.MaxReconnectAttempts {
			select {
			case <-c.ctx.Done():
				return
			case <-time.After(backoff):
				if err := c.Connect(); err == nil {
					if err := c.resubscribeAll(); err == nil {
						c.logger.Info().Msg("Reconnected successfully")
						c.connStatusMu.Lock()
						c.connStatus.ReconnectCount = 0
						c.connStatusMu.Unlock()
						return
					}
				}
				backoff = time.Duration(float64(backoff) * 1.5)
				attempts++
			}
		}
		c.logger.Error().Msg("Failed to reconnect after maximum attempts")
	}()
}

func (c *WSClient) resubscribeAll() error {
	c.subsMu.RLock()
	defer c.subsMu.RUnlock()

	for addr, sub := range c.subscriptions {
		sub.cancel()

		subCtx, cancel := context.WithCancel(c.ctx)

		newSub, err := c.client.LogsSubscribeMentions(
			solana.MustPublicKeyFromBase58(addr),
			sub.commitment,
		)
		if err != nil {
			cancel()
			return fmt.Errorf("failed to resubscribe %s: %w", addr, err)
		}

		sub.sub = newSub
		sub.ctx = subCtx
		sub.cancel = cancel

		c.group.Go(func() error {
			return c.handleSubscription(subCtx, addr, newSub)
		})

		c.updateSubscriptionStatus(addr, func(s *SubscriptionStatus) {
			s.IsActive = true
			s.IsReconnecting = false
		})
	}
	return nil
}

func (c *WSClient) UnsubscribeFromWallet(addr string) error {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()

	sub, exists := c.subscriptions[addr]
	if !exists {
		return fmt.Errorf("subscription not found")
	}

	// Cancel the subscription context first
	if sub.cancel != nil {
		sub.cancel()
	}

	if sub.sub != nil {
		sub.sub.Unsubscribe()
	}
	delete(c.subscriptions, addr)

	c.statusMu.Lock()
	delete(c.status, addr)
	c.statusMu.Unlock()

	c.logger.Debug().
		Str("address", addr).
		Msg("Successfully unsubscribed wallet")

	return nil
}

func (c *WSClient) Close() error {
	c.cancel()

	c.subsMu.Lock()
	for _, sub := range c.subscriptions {
		if sub.sub != nil {
			sub.sub.Unsubscribe()
		}
	}
	c.subsMu.Unlock()

	c.clientMu.Lock()
	if c.client != nil {
		c.client.Close()
		c.client = nil
	}
	c.clientMu.Unlock()

	return c.group.Wait()
}
