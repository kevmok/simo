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

func NewTransactionParser(rpcURL string, logger zerolog.Logger) *TransactionParser {
	return &TransactionParser{
		client: rpc.New(rpcURL),
		logger: logger,
	}
}

func (p *TransactionParser) ParseTransaction(ctx context.Context, signature string, walletAddress string) (*TokenTransaction, error) {
	p.logger.Info().
		Str("signature", signature).
		Str("wallet", walletAddress).
		Msg("Starting transaction parsing")

	tx, err := p.getTransactionWithRetry(ctx, signature)
	if err != nil || tx == nil || tx.Meta.Err != nil {
		return nil, err
	}

	parsed_tx, _ := tx.Transaction.GetTransaction()
	dexProgram := findDEXProgram(parsed_tx)
	if dexProgram.IsZero() {
		p.logger.Debug().Str("signature", signature).Msg("Not a DEX transaction")
		return nil, nil
	}

	changes := make(map[string]float64)
	walletPubkey := solana.MustPublicKeyFromBase58(walletAddress)

	for _, post := range tx.Meta.PostTokenBalances {
		isRelevantAccount := post.Owner.String() == walletAddress
		if !isRelevantAccount {
			expected, _, _ := solana.FindAssociatedTokenAddress(
				walletPubkey, // Note: order switched to match solana.FindAssociatedTokenAddress
				post.Mint,
			)
			isRelevantAccount = expected.String() == post.Owner.String() // Compare strings to avoid type mismatch
		}

		p.logger.Debug().
			Str("signature", signature).
			Str("mintOwner", post.Owner.String()).
			Str("expectedWallet", walletAddress).
			Str("mintAddress", post.Mint.String()).
			Bool("isRelevantAccount", isRelevantAccount).
			Interface("uiTokenAmount", post.UiTokenAmount).
			Msg("Analyzing post balance")

		if !isRelevantAccount {
			continue
		}

		for _, pre := range tx.Meta.PreTokenBalances {
			if pre.AccountIndex != post.AccountIndex {
				continue
			}

			if pre.UiTokenAmount.UiAmount == nil || post.UiTokenAmount.UiAmount == nil {
				continue
			}

			diff := *post.UiTokenAmount.UiAmount - *pre.UiTokenAmount.UiAmount
			if diff != 0 {
				changes[post.Mint.String()] = diff
				p.logger.Debug().
					Str("signature", signature).
					Str("mint", post.Mint.String()).
					Float64("difference", diff).
					Msg("Found balance change")
			}
		}
	}
	p.logger.Log().Interface("changes", changes).Msg("Identifiying tokens...")
	tokenIn, tokenOut, amountIn, amountOut := p.identifyTokens(changes)
	if tokenIn == "" || tokenOut == "" {
		p.logger.Log().
			Str("signature", signature).
			Str("wallet", walletAddress).
			Msg("No token changes detected")
		return nil, nil
	}

	return &TokenTransaction{
		WalletAddress: walletAddress,
		TokenIn:       tokenIn,
		TokenOut:      tokenOut,
		AmountIn:      amountIn,
		AmountOut:     amountOut,
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

func (p *TransactionParser) calculateBalanceChanges(tx *rpc.GetTransactionResult, post *rpc.TokenBalance, changes map[string]float64) map[string]float64 {
	for _, pre := range tx.Meta.PreTokenBalances {
		if pre.AccountIndex != post.AccountIndex {
			continue
		}

		if pre.UiTokenAmount.UiAmount == nil || post.UiTokenAmount.UiAmount == nil {
			continue
		}

		diff := *post.UiTokenAmount.UiAmount - *pre.UiTokenAmount.UiAmount
		if diff != 0 {
			changes[post.Mint.String()] = diff
		}
	}
	return changes
}

func (p *TransactionParser) identifyTokens(changes map[string]float64) (string, string, float64, float64) {
	var tokenIn, tokenOut string
	var amountIn, amountOut float64

	for token, change := range changes {
		if change < 0 {
			tokenIn = token
			amountIn = -change
		} else {
			tokenOut = token
			amountOut = change
		}
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
