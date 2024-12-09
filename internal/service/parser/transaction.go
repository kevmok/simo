package parser

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"simo/internal/constants"

	"github.com/cenkalti/backoff/v4"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

// TokenAmount represents a token amount with precise decimal handling
type TokenAmount struct {
	Amount   float64
	Decimals uint8
	Raw      uint64
}

// TokenInfo contains essential token metadata
type TokenInfo struct {
	Address  solana.PublicKey
	Symbol   string
	Decimals uint8
}

type TokenBalance struct {
	AccountIndex  uint16
	Mint          solana.PublicKey
	Owner         solana.PublicKey
	UiTokenAmount rpc.UiTokenAmount
}

// SwapDetails contains comprehensive swap transaction information
type SwapDetails struct {
	TokenIn   TokenInfo
	TokenOut  TokenInfo
	AmountIn  TokenAmount
	AmountOut TokenAmount
	Signature string
	Program   string // DEX program name (e.g., "Jupiter", "Raydium")
	Timestamp time.Time
}

// ParserMetrics holds prometheus metrics for monitoring
type ParserMetrics struct {
	ParseDuration    *prometheus.HistogramVec
	RetryCount       *prometheus.CounterVec
	FailedParses     *prometheus.CounterVec
	SuccessfulParses *prometheus.CounterVec
}

// TransactionParser handles the parsing of Solana transactions
type TransactionParser struct {
	client  *rpc.Client
	logger  zerolog.Logger
	metrics *ParserMetrics

	// Caches
	ataCache     sync.Map // Cache for Associated Token Accounts
	programCache map[solana.PublicKey]string
}

const (
	WrappedSOL = "So11111111111111111111111111111111111111112"
)

func NewTransactionParser(rpcURL string, logger zerolog.Logger) *TransactionParser {
	return &TransactionParser{
		client:  rpc.New(rpcURL),
		logger:  logger.With().Str("component", "transaction_parser").Logger(),
		metrics: initializeMetrics(),
		programCache: map[solana.PublicKey]string{
			constants.JupiterV6Program:     "Jupiter",
			constants.RaydiumV4Program:     "Raydium",
			constants.OrcaWhirlpoolProgram: "Orca",
			constants.PumpfunProgram:       "Pumpfun",
		},
	}
}

func (p *TransactionParser) ParseTransaction(ctx context.Context, signature string, walletAddress string) (*SwapDetails, error) {
	start := time.Now()
	defer func() {
		p.metrics.ParseDuration.WithLabelValues("parse_duration").Observe(time.Since(start).Seconds())
	}()

	p.logger.Debug().
		Str("signature", signature).
		Str("wallet", walletAddress).
		Msg("Starting transaction fetch")

	tx, err := p.getTransactionWithRetry(ctx, signature)
	if err != nil {
		p.metrics.FailedParses.WithLabelValues("get_transaction").Inc()
		return nil, fmt.Errorf("failed to get transaction: %w", err)
	}

	p.logger.Debug().
		Str("signature", signature).
		Msg("Transaction fetched successfully")

	parsed_tx, err := tx.Transaction.GetTransaction()
	if err != nil {
		return nil, fmt.Errorf("failed to parse transaction: %w", err)
	}

	// Check if this is a DEX transaction
	dexProgram := findDEXProgram(parsed_tx)
	if dexProgram.IsZero() {
		// Log all program IDs for debugging
		var programIDs []string
		for _, inst := range parsed_tx.Message.Instructions {
			programID := parsed_tx.Message.AccountKeys[inst.ProgramIDIndex]
			programIDs = append(programIDs, programID.String())
		}

		p.logger.Debug().
			Strs("program_ids", programIDs).
			Msg("Not a DEX transaction - no matching DEX program found")
		return nil, nil
	}

	// Calculate balance changes
	changes := p.calculateBalanceChanges(tx, walletAddress)

	// Log all changes for debugging
	for token, change := range changes {
		p.logger.Debug().
			Str("token", token).
			Float64("change", change).
			Msg("Token balance change detected")
	}

	p.handleSOLChanges(tx, walletAddress, changes)

	// Identify tokens and amounts
	tokenIn, tokenOut, amountIn, amountOut := p.identifyTokens(changes)

	p.logger.Debug().
		Str("token_in", tokenIn).
		Str("token_out", tokenOut).
		Float64("amount_in", amountIn).
		Float64("amount_out", amountOut).
		Int("total_changes", len(changes)).
		Msg("Token identification results")

	if tokenIn == "" || tokenOut == "" {
		return nil, nil
	}

	p.metrics.SuccessfulParses.WithLabelValues(p.programCache[dexProgram]).Inc()

	return &SwapDetails{
		TokenIn: TokenInfo{
			Address: solana.MustPublicKeyFromBase58(tokenIn),
		},
		TokenOut: TokenInfo{
			Address: solana.MustPublicKeyFromBase58(tokenOut),
		},
		AmountIn: TokenAmount{
			Amount: math.Abs(amountIn),
		},
		AmountOut: TokenAmount{
			Amount: math.Abs(amountOut),
		},
		Signature: signature,
		Program:   p.programCache[dexProgram],
		Timestamp: time.Unix(int64(*tx.BlockTime), 0),
	}, nil
}

func (p *TransactionParser) getTransactionWithRetry(ctx context.Context, signature string) (*rpc.GetTransactionResult, error) {
	var tx *rpc.GetTransactionResult

	policy := backoff.NewExponentialBackOff()
	policy.MaxElapsedTime = 30 * time.Second
	policy.InitialInterval = 1 * time.Second
	policy.MaxInterval = 5 * time.Second
	policy.Multiplier = 1.5

	operation := func() error {
		// Add context timeout for each attempt
		attemptCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		opts := &rpc.GetTransactionOpts{
			MaxSupportedTransactionVersion: new(uint64),
			Commitment:                     rpc.CommitmentConfirmed,
		}

		var err error
		tx, err = p.client.GetTransaction(attemptCtx, solana.MustSignatureFromBase58(signature), opts)
		if err != nil {
			// More detailed error logging
			p.logger.Debug().
				Err(err).
				Str("signature", signature).
				Msg("Transaction fetch attempt failed")

			if strings.Contains(err.Error(), "not found") {
				return backoff.Permanent(fmt.Errorf("transaction not found: %w", err))
			}

			// Check for context cancellation
			if attemptCtx.Err() != nil {
				return backoff.Permanent(fmt.Errorf("context canceled during fetch: %w", attemptCtx.Err()))
			}

			return fmt.Errorf("transaction fetch failed: %w", err)
		}

		if tx == nil || tx.Meta == nil {
			return fmt.Errorf("received empty transaction")
		}

		if tx.Meta.Err != nil {
			return fmt.Errorf("transaction contains error: %v", tx.Meta.Err)
		}

		return nil
	}

	err := backoff.Retry(operation, backoff.WithContext(policy, ctx))
	if err != nil {
		return nil, fmt.Errorf("failed to get transaction after retries: %w", err)
	}

	return tx, nil
}

func (p *TransactionParser) calculateBalanceChanges(tx *rpc.GetTransactionResult, walletAddress string) map[string]float64 {
	changes := make(map[string]float64)
	walletPubkey := solana.MustPublicKeyFromBase58(walletAddress)

	// Process pre-balances
	preBalances := make(map[string]float64)
	for _, pre := range tx.Meta.PreTokenBalances {
		if pre.UiTokenAmount.UiAmount != nil {
			preBalances[fmt.Sprintf("%d", pre.AccountIndex)] = *pre.UiTokenAmount.UiAmount
		}
	}

	// Process post-balances and calculate changes
	for _, post := range tx.Meta.PostTokenBalances {
		if !p.isRelevantAccount(post, walletPubkey, tx) {
			continue
		}

		if post.UiTokenAmount.UiAmount == nil {
			continue
		}

		postAmount := *post.UiTokenAmount.UiAmount
		preAmount := preBalances[fmt.Sprintf("%d", post.AccountIndex)]
		diff := postAmount - preAmount

		if diff != 0 {
			changes[post.Mint.String()] = diff
		}
	}

	return changes
}

func (p *TransactionParser) isRelevantAccount(post rpc.TokenBalance, walletPubkey solana.PublicKey, tx *rpc.GetTransactionResult) bool {
	if post.Owner != nil && post.Owner.String() == walletPubkey.String() {
		return true
	}

	// Check ATA
	ataKey := fmt.Sprintf("%s-%s", walletPubkey.String(), post.Mint.String())
	if ata, ok := p.ataCache.Load(ataKey); ok {
		return ata.(solana.PublicKey).Equals(*post.Owner)
	}

	ata, _, _ := solana.FindAssociatedTokenAddress(walletPubkey, post.Mint)
	p.ataCache.Store(ataKey, ata)

	return ata.Equals(*post.Owner)
}

func (p *TransactionParser) handleSOLChanges(tx *rpc.GetTransactionResult, walletAddress string, changes map[string]float64) {
	if len(tx.Meta.PreBalances) == 0 || len(tx.Meta.PostBalances) == 0 {
		return
	}

	decoded, err := tx.Transaction.GetTransaction()
	if err != nil {
		p.logger.Error().Err(err).Msg("Failed to decode transaction for SOL balance check")
		return
	}

	for i, key := range decoded.Message.AccountKeys {
		if key.String() == walletAddress {
			preBalance := float64(tx.Meta.PreBalances[i]) / 1e9
			postBalance := float64(tx.Meta.PostBalances[i]) / 1e9
			solChange := postBalance - preBalance

			if math.Abs(solChange) > 0.00001 {
				if existing, ok := changes[WrappedSOL]; ok {
					changes[WrappedSOL] = existing + solChange
				} else {
					changes[WrappedSOL] = solChange
				}
			}
			break
		}
	}
}

func (p *TransactionParser) identifyTokens(changes map[string]float64) (string, string, float64, float64) {
	var tokenIn, tokenOut string
	var amountIn, amountOut float64

	// Track all negative and positive changes for better analysis
	var negativeChanges, positiveChanges []struct {
		token  string
		amount float64
	}

	// First pass: separate changes into negative and positive
	for token, change := range changes {
		if change < 0 {
			negativeChanges = append(negativeChanges, struct {
				token  string
				amount float64
			}{token, change})
		} else if change > 0 {
			positiveChanges = append(positiveChanges, struct {
				token  string
				amount float64
			}{token, change})
		}
	}

	// Log summary of changes
	p.logger.Debug().
		Int("negative_changes", len(negativeChanges)).
		Int("positive_changes", len(positiveChanges)).
		Msg("Analyzing token changes")

	// Find largest negative change (token in)
	for _, nc := range negativeChanges {
		if math.Abs(nc.amount) > math.Abs(amountIn) {
			tokenIn = nc.token
			amountIn = nc.amount
		}
	}

	// Find largest positive change (token out)
	for _, pc := range positiveChanges {
		if pc.amount > amountOut {
			tokenOut = pc.token
			amountOut = pc.amount
		}
	}

	// Validate the swap makes sense
	if tokenIn != "" && tokenOut != "" {
		p.logger.Debug().
			Str("token_in", tokenIn).
			Float64("amount_in", amountIn).
			Str("token_out", tokenOut).
			Float64("amount_out", amountOut).
			Msg("Valid swap pair identified")
	} else {
		p.logger.Debug().
			Interface("negative_changes", negativeChanges).
			Interface("positive_changes", positiveChanges).
			Msg("Failed to identify valid swap pair")
	}

	return tokenIn, tokenOut, amountIn, amountOut
}

func findDEXProgram(tx *solana.Transaction) solana.PublicKey {
	for _, inst := range tx.Message.Instructions {
		programID := tx.Message.AccountKeys[inst.ProgramIDIndex]
		if constants.IsDEXProgram(programID) {
			return programID
		}
	}
	return solana.PublicKey{}
}

// Initialize metrics
func initializeMetrics() *ParserMetrics {
	return &ParserMetrics{
		ParseDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "transaction_parse_duration_seconds",
				Help: "Time spent parsing transactions",
			},
			[]string{"type"},
		),
		RetryCount: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "transaction_retry_total",
				Help: "Number of transaction fetch retries",
			},
			[]string{"reason"},
		),
		FailedParses: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "transaction_parse_failures_total",
				Help: "Number of failed transaction parses",
			},
			[]string{"reason"},
		),
		SuccessfulParses: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "transaction_parse_success_total",
				Help: "Number of successful transaction parses",
			},
			[]string{"dex"},
		),
	}
}

func getProgramName(programID solana.PublicKey) string {
	switch {
	case programID.Equals(constants.JupiterV6Program):
		return "Jupiter"
	case programID.Equals(constants.RaydiumV4Program):
		return "Raydium"
	case programID.Equals(constants.OrcaWhirlpoolProgram):
		return "Orca"
	default:
		return "Unknown"
	}
}
