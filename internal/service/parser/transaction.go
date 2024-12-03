package parser

import (
	"context"
	"fmt"
	"math"
	"simo/internal/constants"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/rs/zerolog"
)

type TransactionParser struct {
	client *rpc.Client
	logger zerolog.Logger
}

type TokenTransaction struct {
	WalletAddress string
	TokenIn       string // Token address being swapped from
	TokenOut      string // Token address being swapped to
	AmountIn      float64
	AmountOut     float64
	Signature     string
	Program       string // Which DEX program was used
}

type SwapDetails struct {
	TokenIn    solana.PublicKey
	TokenOut   solana.PublicKey
	AmountIn   uint64
	AmountOut  uint64
	DEXProgram string
}

const (
	WrappedSOLMint = "So11111111111111111111111111111111111111112"
)

func NewTransactionParser(rpcURL string, logger zerolog.Logger) *TransactionParser {
	return &TransactionParser{
		client: rpc.New(rpcURL),
		logger: logger.With().Str("component", "transaction_parser").Logger(),
	}
}

func (p *TransactionParser) ParseTransaction(ctx context.Context, signature string, walletAddress string) (*TokenTransaction, error) {
	p.logger.Info().
		Str("signature", signature).
		Str("wallet", walletAddress).
		Msg("Starting transaction parsing")

	tx, err := p.getTransactionWithRetry(ctx, signature)
	if err != nil || tx == nil || tx.Meta == nil || tx.Meta.Err != nil {
		return nil, fmt.Errorf("failed to get or invalid transaction: %w", err)
	}

	parsed_tx, err := tx.Transaction.GetTransaction()
	if err != nil {
		return nil, fmt.Errorf("failed to parse transaction: %w", err)
	}

	dexProgram := findDEXProgram(parsed_tx)
	if dexProgram.IsZero() {
		p.logger.Debug().Str("signature", signature).Msg("Not a DEX transaction")
		return nil, nil
	}

	// Track all balance changes
	changes := p.calculateBalanceChanges(tx, walletAddress)

	// Special handling for SOL (both native and wrapped)
	p.handleSOLChanges(tx, walletAddress, changes)

	p.logger.Debug().
		Interface("changes", changes).
		Msg("Token balance changes calculated")

	tokenIn, tokenOut, amountIn, amountOut := p.identifyTokens(changes)
	if tokenIn == "" || tokenOut == "" {
		p.logger.Debug().
			Str("signature", signature).
			Interface("changes", changes).
			Msg("Insufficient token changes detected")
		return nil, nil
	}

	return &TokenTransaction{
		WalletAddress: walletAddress,
		TokenIn:       tokenIn,
		TokenOut:      tokenOut,
		AmountIn:      math.Abs(amountIn),  // Ensure positive
		AmountOut:     math.Abs(amountOut), // Ensure positive
		Signature:     signature,
		Program:       getProgramName(dexProgram),
	}, nil
}

func (p *TransactionParser) getTransactionWithRetry(ctx context.Context, signature string) (*rpc.GetTransactionResult, error) {
	var tx *rpc.GetTransactionResult
	var err error

	opts := &rpc.GetTransactionOpts{
		MaxSupportedTransactionVersion: new(uint64),
		Commitment:                     rpc.CommitmentConfirmed,
	}

	maxRetries := 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			backoffDuration := time.Duration(math.Pow(2, float64(attempt))) * time.Second
			time.Sleep(backoffDuration)
		}

		tx, err = p.client.GetTransaction(ctx, solana.MustSignatureFromBase58(signature), opts)
		if err == nil && tx != nil {
			// Check both Meta.Err and Status.Err
			if tx.Meta != nil && tx.Meta.Err == nil {
				break
			}
		}

		// Special handling for "not found"
		if err != nil && strings.Contains(err.Error(), "not found") {
			time.Sleep(time.Second)
			continue
		}

		if attempt == maxRetries-1 {
			return nil, fmt.Errorf("failed to get transaction after %d attempts: %w", maxRetries, err)
		}
	}

	return tx, err
}

func (p *TransactionParser) calculateBalanceChanges(tx *rpc.GetTransactionResult, walletAddress string) map[string]float64 {
	changes := make(map[string]float64)
	walletPubkey := solana.MustPublicKeyFromBase58(walletAddress)

	// Create a map of pre-balances for quick lookup
	preBalances := make(map[string]float64)
	for _, pre := range tx.Meta.PreTokenBalances {
		if pre.UiTokenAmount.UiAmount != nil {
			key := fmt.Sprintf("%d", pre.AccountIndex)
			preBalances[key] = *pre.UiTokenAmount.UiAmount
		}
	}

	// Get decoded transaction
	decoded, err := tx.Transaction.GetTransaction()
	if err != nil {
		p.logger.Error().Err(err).Msg("Failed to decode transaction")
		return changes
	}

	// Process post balances and calculate changes
	for _, post := range tx.Meta.PostTokenBalances {
		isRelevantAccount := false

		// Check if direct ownership
		if post.Owner.String() == walletAddress {
			isRelevantAccount = true
		} else {
			// Check if it's an ATA
			ata, _, _ := solana.FindAssociatedTokenAddress(walletPubkey, post.Mint)
			accountKey := decoded.Message.AccountKeys[post.AccountIndex]
			if ata.Equals(accountKey) {
				isRelevantAccount = true
			}
		}

		p.logger.Debug().
			Str("mint", post.Mint.String()).
			Str("owner", post.Owner.String()).
			Bool("isRelevant", isRelevantAccount).
			Msg("Checking account relevance")

		if !isRelevantAccount {
			continue
		}

		if post.UiTokenAmount.UiAmount == nil {
			continue
		}

		postAmount := *post.UiTokenAmount.UiAmount
		key := fmt.Sprintf("%d", post.AccountIndex)
		preAmount := preBalances[key]

		diff := postAmount - preAmount
		if diff != 0 {
			changes[post.Mint.String()] = diff
		}
	}

	return changes
}

func (p *TransactionParser) handleSOLChanges(tx *rpc.GetTransactionResult, walletAddress string, changes map[string]float64) {
	// Handle native SOL balance changes
	if len(tx.Meta.PreBalances) > 0 && len(tx.Meta.PostBalances) > 0 {
		walletIndex := -1
		// Get decoded transaction
		decoded, err := tx.Transaction.GetTransaction()
		if err != nil {
			p.logger.Error().Err(err).Msg("Failed to decode transaction for SOL balance check")
			return
		}

		for i, key := range decoded.Message.AccountKeys {
			if key.String() == walletAddress {
				walletIndex = i
				break
			}
		}

		if walletIndex >= 0 && walletIndex < len(tx.Meta.PreBalances) {
			preBalance := float64(tx.Meta.PreBalances[walletIndex]) / 1e9 // Convert lamports to SOL
			postBalance := float64(tx.Meta.PostBalances[walletIndex]) / 1e9

			solChange := postBalance - preBalance
			if math.Abs(solChange) > 0.00001 { // Filter out dust
				// Add to wrapped SOL changes or create new entry
				if existing, ok := changes[WrappedSOLMint]; ok {
					changes[WrappedSOLMint] = existing + solChange
				} else {
					changes[WrappedSOLMint] = solChange
				}
			}
		}
	}
}

func (p *TransactionParser) identifyTokens(changes map[string]float64) (string, string, float64, float64) {
	var tokenIn, tokenOut string
	var amountIn, amountOut float64

	// Sort changes by absolute value to prioritize larger changes
	for token, change := range changes {
		p.logger.Debug().
			Str("token", token).
			Float64("change", change).
			Msg("Processing token change")

		if change < 0 {
			if math.Abs(change) > math.Abs(amountIn) {
				tokenIn = token
				amountIn = change
			}
		} else {
			if change > amountOut {
				tokenOut = token
				amountOut = change
			}
		}
	}

	p.logger.Debug().
		Str("tokenIn", tokenIn).
		Str("tokenOut", tokenOut).
		Float64("amountIn", amountIn).
		Float64("amountOut", amountOut).
		Msg("Token identification complete")

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
