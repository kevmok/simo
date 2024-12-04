package service

import (
	"context"
	"fmt"
	"simo/internal/config"
	"simo/internal/repository"
	"simo/internal/service/parser"
	"simo/internal/service/websocket"
	"simo/pkg/discord"
	"simo/pkg/solanatracker"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/sourcegraph/conc"
)

type WalletTrackerService interface {
	Close()
	AddWallet(ctx context.Context, address string) error
	RemoveWallet(ctx context.Context, address string) error
	GetWallets(ctx context.Context) ([]repository.Wallet, error)

	handleTransaction(ctx context.Context, walletAddress, signature string) error
}

type WalletTracker struct {
	db         *pgxpool.Pool
	repo       repository.Repository
	discords   map[string]discord.WebhookClient
	wsClient   *websocket.Client
	solTracker *solanatracker.Client
	parser     *parser.TransactionParser
	logger     zerolog.Logger
	closed     bool
	mu         sync.Mutex
}

type Config struct {
	WebhookURL         string
	SolanaRPCURL       string
	SolanaWebSocketURL string
	SolanaTrackerKey   string
}

func NewWalletTracker(ctx context.Context, cfg Config, logger zerolog.Logger) (*WalletTracker, error) {
	// Initialize database connection
	dbConfig := config.NewDatabaseConfig()
	dbPool, err := config.InitDB(dbConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize database: %w", err)
	}
	repo := repository.NewRepository(dbPool)

	wsCfg := websocket.DefaultConfig()
	wsCfg.WSURL = cfg.SolanaWebSocketURL
	wsCfg.RPCURL = cfg.SolanaRPCURL
	wsClient, err := websocket.NewClient(wsCfg, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create WebSocket client: %w", err)
	}

	solTracker, err := solanatracker.NewClient(solanatracker.ClientConfig{
		APIKey: cfg.SolanaTrackerKey,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create SolanaTracker client: %w", err)
	}

	webhooks, err := repo.GetWebhooks(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get webhooks: %w", err)
	}

	discords := make(map[string]discord.WebhookClient)
	for _, webhook := range webhooks {
		webhookID, webhookToken, err := discord.ParseWebhookURL(webhook.WebhookURL)
		if err != nil {
			logger.Error().
				Err(err).
				Str("webhook_name", webhook.Name).
				Msg("Failed to parse webhook URL")
			continue
		}

		client, err := discord.NewWebhookClient(webhookID, webhookToken)
		if err != nil {
			logger.Error().
				Err(err).
				Str("webhook_name", webhook.Name).
				Msg("Failed to create webhook client")
			continue
		}
		discords[webhook.Name] = client
	}

	tracker := &WalletTracker{
		db:         dbPool,
		repo:       repo,
		discords:   discords,
		wsClient:   wsClient,
		solTracker: solTracker,
		logger:     logger,
		parser:     parser.NewTransactionParser(cfg.SolanaRPCURL, logger),
	}
	// Start tracking existing wallets
	if err := tracker.initializeWalletTracking(ctx); err != nil {
		return nil, fmt.Errorf("failed to initialize wallet tracking: %w", err)
	}

	return tracker, nil
}

func (wt *WalletTracker) Close() {
	wt.mu.Lock()
	if wt.closed {
		wt.mu.Unlock()
		return
	}
	wt.closed = true
	wt.mu.Unlock()

	// Create a context with timeout for graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wt.logger.Info().Msg("starting graceful shutdown of wallet tracker")

	// First unsubscribe all wallets
	wallets, err := wt.GetWallets(ctx)
	if err != nil {
		wt.logger.Error().Err(err).Msg("failed to get wallets during shutdown")
	} else {
		for _, wallet := range wallets {
			if err := wt.wsClient.UnsubscribeFromWallet(ctx, wallet.Address); err != nil {
				wt.logger.Error().
					Err(err).
					Str("wallet", wallet.Address).
					Msg("failed to unsubscribe wallet during shutdown")
			}
		}
	}

	// Then close the websocket client
	if wt.wsClient != nil {
		if err := wt.wsClient.Close(); err != nil {
			wt.logger.Error().Err(err).Msg("failed to close websocket client")
		}
	}

	// Finally close the database
	if wt.db != nil {
		wt.db.Close()
	}

	wt.logger.Info().Msg("wallet tracker shutdown complete")
}

func (wt *WalletTracker) AddWallet(ctx context.Context, address string) error {
	if err := wt.repo.AddWallet(ctx, address); err != nil {
		return fmt.Errorf("failed to add wallet to database: %w", err)
	}

	if err := wt.startTrackingWallet(ctx, address); err != nil {
		return fmt.Errorf("failed to start tracking wallet: %w", err)
	}

	return nil
}

func (wt *WalletTracker) RemoveWallet(ctx context.Context, address string) error {
	// First stop tracking the wallet
	if err := wt.wsClient.UnsubscribeFromWallet(ctx, address); err != nil {
		return fmt.Errorf("failed to unsubscribe from wallet: %w", err)
	}

	// Then remove from database
	if err := wt.repo.RemoveWallet(ctx, address); err != nil {
		return fmt.Errorf("failed to remove wallet from database: %w", err)
	}

	return nil
}

func (wt *WalletTracker) GetWallets(ctx context.Context) ([]repository.Wallet, error) {
	return wt.repo.GetWallets(ctx)
}

func (wt *WalletTracker) GetWalletPositions(ctx context.Context, address string) ([]repository.TokenPosition, error) {
	wallet, err := wt.repo.GetWalletByAddress(ctx, address)
	if err != nil {
		return nil, err
	}
	return wt.repo.GetTokenPositions(ctx, wallet.ID)
}

func (wt *WalletTracker) initializeWalletTracking(ctx context.Context) error {
	wallets, err := wt.repo.GetWallets(ctx)
	if err != nil {
		return fmt.Errorf("failed to get wallets: %w", err)
	}

	for _, wallet := range wallets {
		if err := wt.startTrackingWallet(ctx, wallet.Address); err != nil {
			wt.logger.Error().
				Err(err).
				Str("wallet", wallet.Address).
				Msg("Failed to start tracking wallet")
		}
	}

	return nil
}
func (wt *WalletTracker) startTrackingWallet(ctx context.Context, address string) error {
	wt.logger.Info().
		Str("wallet", address).
		Msg("Starting to track wallet")

	return wt.wsClient.SubscribeToWallet(ctx, address, func(signature string) {
		wt.logger.Info().
			Str("wallet", address).
			Str("signature", signature).
			Msg("New transaction detected")

		// Process transaction in a separate goroutine
		go func() {
			if err := wt.handleTransaction(ctx, address, signature); err != nil {
				wt.logger.Error().
					Err(err).
					Str("wallet", address).
					Str("signature", signature).
					Msg("Failed to handle transaction")
			}
		}()
	})
}

func (wt *WalletTracker) handleTransaction(ctx context.Context, walletAddress, signature string) error {
	// Parse the transaction
	tokenTx, err := wt.parser.ParseTransaction(ctx, signature, walletAddress)
	if err != nil {
		return fmt.Errorf("failed to parse transaction: %w", err)
	}

	// If it's not a token transaction we care about, ignore it
	if tokenTx == nil {
		wt.logger.Info().
			Str("wallet", walletAddress).
			Str("signature", signature).
			Interface("tx", tokenTx).
			Msg("Not a token transaction")
		return nil
	}

	// Get wallet ID for database operations
	wallet, err := wt.repo.GetWalletByAddress(ctx, walletAddress)
	if err != nil {
		return fmt.Errorf("failed to get wallet: %w", err)
	}

	tx := &repository.Transaction{
		WalletID:  wallet.ID,
		TokenIn:   tokenTx.TokenIn,
		TokenOut:  tokenTx.TokenOut,
		AmountIn:  tokenTx.AmountIn,
		AmountOut: tokenTx.AmountOut,
		Program:   tokenTx.Program,
		Signature: tokenTx.Signature,
		Timestamp: time.Now(),
	}

	if err := wt.repo.AddTransaction(ctx, tx); err != nil {
		return fmt.Errorf("failed to record transaction: %w", err)
	}

	// Get token information for both tokens
	tokenInInfo, err := wt.solTracker.GetTokenInfo(ctx, tokenTx.TokenIn)
	if err != nil {
		wt.logger.Warn().Err(err).Str("token", tokenTx.TokenIn).Msg("Failed to get token info")
	}

	tokenOutInfo, err := wt.solTracker.GetTokenInfo(ctx, tokenTx.TokenOut)
	if err != nil {
		wt.logger.Warn().Err(err).Str("token", tokenTx.TokenOut).Msg("Failed to get token info")
	}
	// log token in info and out info
	wt.logger.Info().
		Str("tokenIn", tokenInInfo.Token.Name).
		Str("tokenInSymbol", tokenInInfo.Token.Symbol).
		Str("tokenOut", tokenOutInfo.Token.Name).
		Str("tokenOutSymbol", tokenOutInfo.Token.Symbol).
		Msg("Token info retrieved")

	var pnl *solanatracker.ProfitLoss
	if tokenOutInfo.Token.Mint != "So11111111111111111111111111111111111111112" {
		pnl, err = wt.solTracker.GetProfitLoss(ctx, walletAddress, tokenOutInfo.Token.Mint)
		if err != nil {
			wt.logger.Warn().Err(err).Str("token", tokenTx.TokenOut).Msg("Failed to get profit loss")
		}
	}

	message := fmt.Sprintf("# 💱 New Swap Alert\n"+
		"### [View Wallet](https://solana.fm/address/%s) | [View Transaction](https://solscan.io/tx/%s)\n\n"+
		"**From Token**\n"+
		"> %s (%s)\n"+
		"> Amount: `%.6f %s`\n"+
		"> Address: `%s`\n\n"+
		"**To Token**\n"+
		"> %s (%s)\n"+
		"> Amount: `%.6f %s`\n"+
		"> Address: `%s`\n\n"+
		"**Details**\n"+
		"> 🏦 DEX: `%s`\n"+
		"> 👛 Wallet: `%s`",
		wallet.Address, tokenTx.Signature,
		tokenInInfo.Token.Name, tokenInInfo.Token.Symbol,
		tokenTx.AmountIn, tokenInInfo.Token.Symbol,
		tokenTx.TokenIn,
		tokenOutInfo.Token.Name, tokenOutInfo.Token.Symbol,
		tokenTx.AmountOut, tokenOutInfo.Token.Symbol,
		tokenTx.TokenOut,
		tokenTx.Program,
		wallet.Address)

	// Add PNL information only if available
	if pnl != nil {
		message += fmt.Sprintf("\n> 🤑 PNL Realized: `%.6f`\n> 💰 PNL Unrealized: `%.6f`",
			pnl.Realized,
			pnl.Unrealized)
	}

	wt.logger.Info().Msg("sending message to discord")
	return wt.sendMessageToAllWebhooks(message)
}

func (wt *WalletTracker) sendMessageToAllWebhooks(message string) error {
	var wg conc.WaitGroup
	defer wg.Wait()
	var errs []error

	for name, client := range wt.discords {
		name, client := name, client // Create new variables for goroutine
		wg.Go(func() {
			if err := client.SendMessage(message); err != nil {
				wt.logger.Error().
					Err(err).
					Str("webhook_name", name).
					Msg("Failed to send message to webhook")
				errs = append(errs, err)
			}
		})
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to send message to some webhooks: %v", errs)
	}
	return nil
}
