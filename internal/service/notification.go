package service

import (
	"context"
	"fmt"
	"simo/internal/repository"
	"simo/internal/service/parser"
	"simo/pkg/discord"
	"simo/pkg/solanatracker"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/sourcegraph/conc"
)

type NotificationService interface {
	SendSwapNotification(ctx context.Context, wallet *repository.Wallet, swap *parser.SwapDetails, tokenInInfo, tokenOutInfo *solanatracker.TokenInfo, pnl *solanatracker.ProfitLoss) error
	Shutdown(ctx context.Context) error
}

type DiscordNotifier struct {
	repo      repository.Repository
	discords  map[string]discord.WebhookClient
	logger    zerolog.Logger
	mu        sync.Mutex
	closed    bool
	wg        conc.WaitGroup
	pending   int        // Track pending notifications
	pendingMu sync.Mutex // Mutex for pending counter
}

func NewDiscordNotifier(repo repository.Repository, logger zerolog.Logger) (*DiscordNotifier, error) {
	webhooks, err := repo.GetWebhooks(context.Background())
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

	return &DiscordNotifier{
		repo:     repo,
		discords: discords,
		logger:   logger,
	}, nil
}

func (n *DiscordNotifier) Shutdown(ctx context.Context) error {
	n.mu.Lock()
	n.closed = true
	n.mu.Unlock()

	n.logger.Info().Msg("Starting notification service shutdown")

	// Wait for pending operations with timeout
	done := make(chan struct{})
	go func() {
		n.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		n.logger.Info().Msg("All pending notifications completed")
		return nil
	case <-ctx.Done():
		n.pendingMu.Lock()
		pending := n.pending
		n.pendingMu.Unlock()

		n.logger.Warn().
			Int("pending_notifications", pending).
			Msg("Notification service shutdown timed out")
		return fmt.Errorf("shutdown timed out with %d pending notifications", pending)
	}
}

func (n *DiscordNotifier) SendSwapNotification(ctx context.Context, wallet *repository.Wallet,
	swap *parser.SwapDetails, tokenInInfo, tokenOutInfo *solanatracker.TokenInfo, pnl *solanatracker.ProfitLoss) error {

	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return fmt.Errorf("notification service is shutting down")
	}

	n.pendingMu.Lock()
	n.pending++
	n.pendingMu.Unlock()

	n.wg.Go(func() {
		defer func() {
			n.pendingMu.Lock()
			n.pending--
			n.pendingMu.Unlock()
		}()

		n.sendNotification(ctx, wallet, swap, tokenInInfo, tokenOutInfo, pnl)
	})

	n.mu.Unlock()
	return nil
}

func (n *DiscordNotifier) sendNotification(ctx context.Context, wallet *repository.Wallet,
	swap *parser.SwapDetails, tokenInInfo, tokenOutInfo *solanatracker.TokenInfo, pnl *solanatracker.ProfitLoss) {

	message := n.constructDiscordMessage(ctx, wallet, swap, tokenInInfo, tokenOutInfo, pnl)

	// Use fresh context to prevent cancellation from parent context
	sendCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := n.sendMessageToAllWebhooks(sendCtx, message); err != nil {
		n.logger.Error().Err(err).Msg("Failed to send notification")
	}
}

func (n *DiscordNotifier) sendMessageToAllWebhooks(ctx context.Context, message string) error {
	var errs []error
	var errMu sync.Mutex
	var wg conc.WaitGroup

	for name, client := range n.discords {
		name, client := name, client
		wg.Go(func() {
			if err := client.SendMessage(ctx, message); err != nil {
				n.logger.Warn().
					Err(err).
					Str("webhook", name).
					Msg("Failed to send webhook message")

				errMu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				errMu.Unlock()
			}
		})
	}

	// Wait with timeout
	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait()
	}()

	select {
	case <-done:
	case <-ctx.Done():
		return fmt.Errorf("webhook send operation timed out")
	}

	if len(errs) > 0 {
		return fmt.Errorf("partial failures: %v", errs)
	}
	return nil
}

func (n *DiscordNotifier) constructDiscordMessage(ctx context.Context, wallet *repository.Wallet, swap *parser.SwapDetails, tokenInInfo, tokenOutInfo *solanatracker.TokenInfo, pnl *solanatracker.ProfitLoss) string {
	// Helper function to format token details
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

	// Get wallet aliases
	aliases, err := n.repo.GetWalletAliases(ctx, wallet.ID)
	var aliasStr string
	if err == nil && len(aliases) > 0 {
		// Use the first alias if available
		aliasStr = fmt.Sprintf(" (%s)", aliases[0].Alias)
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
		"> 👛 **Wallet**: `%s%s`\n"+
		"> ⏰ **Time**: `%s`",
		swap.Program,
		wallet.Address,
		aliasStr,
		swap.Timestamp.Format("2006-01-02 15:04:05 MST"))

	// Add PNL information if available and if the input token isn't wrapped SOL
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
