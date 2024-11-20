package service

import (
	"context"
	"fmt"
	"simo/internal/config"
	"simo/pkg/discord"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

type WalletTrackerService interface {
	Close()
	// Add other methods here as we implement them
	NotifyTransaction(tx *Transaction) error
}

type WalletTracker struct {
	db      *pgxpool.Pool
	discord discord.WebhookClient
	logger  zerolog.Logger
}

type Config struct {
	WebhookURL   string
	SolanaRPCURL string
}

func NewWalletTracker(ctx context.Context, cfg Config, logger zerolog.Logger) (*WalletTracker, error) {
	// Initialize database connection
	dbConfig := config.NewDatabaseConfig()
	dbPool, err := config.InitDB(dbConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize database: %w", err)
	}
	// Parse and initialize Discord webhook
	webhookID, webhookToken, err := discord.ParseWebhookURL(cfg.WebhookURL)
	if err != nil {
		return nil, fmt.Errorf("error parsing webhook URL: %w", err)
	}

	webhookClient, err := discord.NewWebhookClient(webhookID, webhookToken)
	if err != nil {
		return nil, fmt.Errorf("error creating webhook client: %w", err)
	}

	return &WalletTracker{
		db:      dbPool,
		discord: webhookClient,
		logger:  logger,
	}, nil
}

func (wt *WalletTracker) Close() {
	if wt.db != nil {
		wt.db.Close()
	}
}

// Example method to send notification
func (wt *WalletTracker) NotifyTransaction(tx *Transaction) error {
	message := fmt.Sprintf("New transaction detected!\nWallet: %s\nAmount: %f\nType: %s",
		tx.WalletAddress, tx.Amount, tx.Type)

	return wt.discord.SendMessage(message)
}
