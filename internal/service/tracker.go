package service

import (
	"context"
	"errors"
	"fmt"
	"simo/internal/config"
	"simo/internal/repository"
	"simo/internal/service/parser"
	"simo/internal/service/websocket"
	"simo/pkg/discord"
	"simo/pkg/solanatracker"
	"strings"
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
	wsClient   *websocket.WSClient
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
	// wsCfg.RPCURL = cfg.SolanaRPCURL
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

	return wt.wsClient.SubscribeToWallet(ctx, address, func(txInfo websocket.TransactionInfo) {
		// Create a new, independent context for transaction processing
		// This ensures transaction processing isn't affected by the websocket context
		processCtx := context.Background()

		// Add a timeout specific to transaction processing
		processCtx, cancel := context.WithTimeout(processCtx, 60*time.Second)

		go func() {
			defer cancel() // Clean up the context when done

			// Create a logger specific to this transaction
			txLogger := wt.logger.With().
				Str("wallet", address).
				Str("signature", txInfo.Signature).
				Str("operation", "transaction_processing").
				Logger()

			txLogger.Info().Msg("Processing new transaction")

			if err := wt.handleTransaction(processCtx, address, txInfo.Signature); err != nil {
				// Log error with transaction-specific context
				txLogger.Error().
					Err(err).
					Msg("Failed to handle transaction")
			}
		}()
	})
}

// In the tracker (handleTransaction), we'll create a proper context hierarchy:
func (wt *WalletTracker) handleTransaction(ctx context.Context, walletAddress, signature string) error {
	// Create a root context for the entire operation with a reasonable timeout
	rootCtx, rootCancel := context.WithTimeout(ctx, 90*time.Second)
	defer rootCancel()

	// Add transaction metadata to context for better tracing
	rootCtx = context.WithValue(rootCtx, "wallet_address", walletAddress)
	rootCtx = context.WithValue(rootCtx, "signature", signature)

	// Create a logger with transaction context
	txLogger := wt.logger.With().
		Str("wallet", walletAddress).
		Str("signature", signature).
		Str("operation", "transaction_handling").
		Logger()

	// Let's break down the operation into phases with their own contexts
	// Phase 1: Transaction Parsing
	parseCtx, parseCancel := context.WithTimeout(rootCtx, 45*time.Second)
	defer parseCancel()

	txLogger.Debug().Msg("Starting transaction parsing phase")
	swapDetails, err := wt.parser.ParseTransaction(parseCtx, signature, walletAddress)

	if err != nil {
		// Check for specific context errors
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			txLogger.Error().Msg("Transaction parsing timed out")
			return fmt.Errorf("parsing timeout: %w", err)
		case errors.Is(err, context.Canceled):
			txLogger.Error().Msg("Transaction parsing was canceled")
			return fmt.Errorf("parsing canceled: %w", err)
		default:
			txLogger.Error().Err(err).Msg("Transaction parsing failed")
			return fmt.Errorf("parsing error: %w", err)
		}
	}

	// Early return if not a swap transaction
	if swapDetails == nil {
		txLogger.Info().Msg("Not a swap transaction")
		return nil
	}

	// Phase 2: Database Operations
	dbCtx, dbCancel := context.WithTimeout(rootCtx, 20*time.Second)
	defer dbCancel()

	txLogger.Debug().Msg("Starting database operations phase")
	wallet, err := wt.repo.GetWalletByAddress(dbCtx, walletAddress)
	if err != nil {
		return fmt.Errorf("database error: %w", err)
	}

	// Phase 3: Token Information Retrieval
	tokenCtx, tokenCancel := context.WithTimeout(rootCtx, 30*time.Second)
	defer tokenCancel()

	// Create wait group for parallel token info fetching
	var tokenWg sync.WaitGroup
	var tokenInInfo, tokenOutInfo *solanatracker.TokenInfo
	var tokenInErr, tokenOutErr error

	txLogger.Debug().Msg("Starting token information retrieval phase")
	tokenWg.Add(2)
	go func() {
		defer tokenWg.Done()
		tokenInInfo, tokenInErr = wt.solTracker.GetTokenInfo(tokenCtx, swapDetails.TokenIn.Address.String())
	}()
	go func() {
		defer tokenWg.Done()
		tokenOutInfo, tokenOutErr = wt.solTracker.GetTokenInfo(tokenCtx, swapDetails.TokenOut.Address.String())
	}()

	// Wait for token info with timeout
	done := make(chan struct{})
	go func() {
		tokenWg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Handle any errors from token info retrieval
		if tokenInErr != nil {
			txLogger.Warn().Err(tokenInErr).Msg("Failed to get token in info")
		}
		if tokenOutErr != nil {
			txLogger.Warn().Err(tokenOutErr).Msg("Failed to get token out info")
		}
	case <-tokenCtx.Done():
		txLogger.Warn().Msg("Token info retrieval timed out")
		return fmt.Errorf("token info timeout: %w", tokenCtx.Err())
	}

	// Phase 4: Message Construction and Delivery
	msgCtx, msgCancel := context.WithTimeout(rootCtx, 20*time.Second)
	defer msgCancel()

	txLogger.Debug().Msg("Starting message construction and delivery phase")

	var pnl *solanatracker.ProfitLoss

	// Add safety checks before attempting to get PNL
	if tokenInInfo != nil && tokenInInfo.Token.Mint != "So11111111111111111111111111111111111111112" {
		// Create a separate context for PNL retrieval with appropriate timeout
		pnlCtx, pnlCancel := context.WithTimeout(ctx, 15*time.Second)
		defer pnlCancel()

		pnl, err = wt.solTracker.GetProfitLoss(pnlCtx, walletAddress, tokenInInfo.Token.Mint)
		if err != nil {
			txLogger.Warn().
				Err(err).
				Str("token", tokenInInfo.Token.Mint).
				Msg("Failed to get profit loss")
			// We continue execution since PNL is optional
		}
	}

	message := constructDiscordMessage(wallet, swapDetails, tokenInInfo, tokenOutInfo, pnl)

	if err := wt.sendMessageToAllWebhooks(msgCtx, message); err != nil {
		txLogger.Error().Err(err).Msg("Failed to send Discord message")
		return fmt.Errorf("message delivery error: %w", err)
	}

	txLogger.Info().Msg("Transaction handling completed successfully")
	return nil
}

func (wt *WalletTracker) sendMessageToAllWebhooks(ctx context.Context, message string) error {
	var wg conc.WaitGroup
	defer wg.Wait()
	var errs []error
	var errMu sync.Mutex // Mutex to safely collect errors from goroutines

	for name, client := range wt.discords {
		name, client := name, client // Create new variables for goroutine
		wg.Go(func() {
			// Create a context with timeout for each webhook call
			webhookCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()

			// Send message with context
			if err := client.SendMessage(webhookCtx, message); err != nil {
				wt.logger.Error().
					Err(err).
					Str("webhook_name", name).
					Msg("Failed to send message to webhook")

				errMu.Lock()
				errs = append(errs, fmt.Errorf("webhook %s: %w", name, err))
				errMu.Unlock()
			}
		})
	}

	// Wait for all goroutines to complete
	wg.Wait()

	if len(errs) > 0 {
		return fmt.Errorf("failed to send message to some webhooks: %v", errs)
	}
	return nil
}

// constructDiscordMessage creates a formatted Discord message for swap transactions.
// It handles cases where token information might be missing and includes optional PNL data.
func constructDiscordMessage(
	wallet *repository.Wallet,
	swap *parser.SwapDetails,
	tokenInInfo *solanatracker.TokenInfo,
	tokenOutInfo *solanatracker.TokenInfo,
	pnl *solanatracker.ProfitLoss,
) string {
	// Helper function to format token details, providing consistent formatting
	// even when token information is unavailable
	formatTokenInfo := func(mint string, amount float64, info *solanatracker.TokenInfo) string {
		if info == nil {
			return fmt.Sprintf("> Unknown Token\n"+
				"> **Amount**: `%.6f`\n"+
				"> **Address**: `%s`", amount, mint)
		}

		return fmt.Sprintf("> %s (%s)\n"+
			"> **Amount**: `%.6f %s`\n"+
			"> **Address**: [%s](https://ape.pro/solana/%s)\n",
			info.Token.Name,
			info.Token.Symbol,
			amount,
			info.Token.Symbol,
			mint, mint)
	}

	addMarketCapAndRisks := func(info *solanatracker.TokenInfo) string {
		if info == nil {
			return ""
		}

		if info.Token.Mint == "So11111111111111111111111111111111111111112" {
			return ""
		}

		// check that there's a risk score
		var risks string
		if len(info.Risk.Risks) != 0 {
			risks = "> **Risks**:\n"
			for _, risk := range info.Risk.Risks {
				risks += fmt.Sprintf("> %s: %s\n", risk.Name, risk.Description)
			}
		}

		return fmt.Sprintf("> **Market Cap**: `%.6f usd`\n"+
			"%s\n\n",
			info.Pools[0].MarketCap.USD,
			risks)
	}

	// Construct the message by building each section
	// Start with the header containing links to blockchain explorers
	header := fmt.Sprintf("# 💱 New Swap Alert\n"+
		"### [View Wallet](https://solana.fm/address/%s) | [View Transaction](https://solscan.io/tx/%s)\n\n",
		wallet.Address, swap.Signature)

	// Add the token input section with detailed information about the source token
	tokenInSection := fmt.Sprintf("**From Token**\n%s",
		formatTokenInfo(
			swap.TokenIn.Address.String(),
			swap.AmountIn.Amount,
			tokenInInfo,
		))

	tokenInSection += addMarketCapAndRisks(tokenInInfo)

	// Add the token output section with information about the destination token
	tokenOutSection := fmt.Sprintf("**To Token**\n%s",
		formatTokenInfo(
			swap.TokenOut.Address.String(),
			swap.AmountOut.Amount,
			tokenOutInfo,
		))

	tokenOutSection += addMarketCapAndRisks(tokenOutInfo)

	// Add transaction details including DEX information and timing
	detailsSection := fmt.Sprintf("**Details**\n"+
		"> 🏦 **DEX**: `%s`\n"+
		"> 👛 **Wallet**: `%s`\n"+
		"> ⏰ **Time**: `%s`",
		swap.Program,
		wallet.Address,
		swap.Timestamp.Format("2006-01-02 15:04:05 MST"))

	// Add PNL information if available and if the input token isn't wrapped SOL
	// We check both the PNL data and the token type to ensure relevant PNL reporting
	if pnl != nil && tokenInInfo != nil &&
		tokenInInfo.Token.Mint != "So11111111111111111111111111111111111111112" {
		detailsSection += fmt.Sprintf("\n> 🤑 PNL Realized: `%.6f`\n"+
			"> 💰 PNL Unrealized: `%.6f`",
			pnl.Realized,
			pnl.Unrealized)
	}

	// Combine all sections into the final message
	message := strings.Join([]string{
		header,
		tokenInSection,
		tokenOutSection,
		detailsSection,
	}, "")

	return message
}
