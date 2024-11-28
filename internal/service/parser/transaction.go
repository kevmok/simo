package parser

import (
	"context"
	"fmt"
	"math"
	"simo/internal/constants"
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
	// Add retry logic with exponential backoff
	var tx *rpc.GetTransactionResult
	var err error

	// Create uint64 pointer for MaxSupportedTransactionVersion
	var maxVersion uint64 = 0

	opts := &rpc.GetTransactionOpts{
		MaxSupportedTransactionVersion: &maxVersion,
		Commitment:                     rpc.CommitmentConfirmed,
	}

	maxRetries := 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff: 1s, 2s, 4s
			backoffDuration := time.Duration(math.Pow(2, float64(attempt))) * time.Second
			p.logger.Debug().
				Str("signature", signature).
				Int("attempt", attempt+1).
				Str("backoff", backoffDuration.String()).
				Msg("Retrying transaction fetch")
			time.Sleep(backoffDuration)
		}

		// Single attempt first for debugging
		tx, err = p.client.GetTransaction(
			ctx,
			solana.MustSignatureFromBase58(signature),
			opts,
		)

		if err == nil {
			p.logger.Debug().
				Str("signature", signature).
				Msg("Successfully fetched transaction")
			break
		}

		// If it's the last attempt, return the error
		if attempt == maxRetries-1 {
			return nil, fmt.Errorf("failed to get transaction after %d attempts: %w", maxRetries, err)
		}

		// Log the retry attempt
		p.logger.Debug().
			Err(err).
			Str("signature", signature).
			Int("attempt", attempt+1).
			Msg("Transaction fetch failed, will retry")
	}

	if tx == nil {
		p.logger.Warn().
			Str("signature", signature).
			Msg("Transaction not found")
		return nil, nil
	}

	// Skip if transaction failed
	if tx.Meta.Err != nil {
		p.logger.Debug().
			Str("signature", signature).
			Interface("error", tx.Meta.Err).
			Msg("Skipping failed transaction")
		return nil, nil
	}

	parsed_tx, err := tx.Transaction.GetTransaction()

	// First check for DEX program
	var dexProgram solana.PublicKey
	for _, inst := range parsed_tx.Message.Instructions {
		programID := parsed_tx.Message.AccountKeys[inst.ProgramIDIndex]
		if constants.IsDEXProgram(programID) {
			dexProgram = programID
			p.logger.Debug().
				Str("signature", signature).
				Str("program", programID.String()).
				Msg("Found DEX program")
			break
		}
	}

	if dexProgram.IsZero() {
		p.logger.Debug().
			Str("signature", signature).
			Msg("Not a DEX transaction")
		return nil, nil
	}

	p.logger.Debug().
		Str("signature", signature).
		Int("postBalancesCount", len(tx.Meta.PostTokenBalances)).
		Int("preBalancesCount", len(tx.Meta.PreTokenBalances)).
		Msg("Starting token balance analysis")

		// Get token mint addresses and amounts directly from the transaction
	var tokenIn, tokenOut string
	var amountIn, amountOut float64

	// First pass: Find the token being spent (input token)
	for _, mint := range tx.Meta.PostTokenBalances {
		p.logger.Debug().
			Str("signature", signature).
			Str("mintOwner", mint.Owner.String()).
			Str("expectedWallet", walletAddress).
			Str("mintAddress", mint.Mint.String()).
			Interface("uiTokenAmount", mint.UiTokenAmount).
			Msg("Analyzing post balance")

		// For the input token, we can be strict about ownership
		if mint.Owner.String() == walletAddress {
			for _, pre := range tx.Meta.PreTokenBalances {
				if pre.AccountIndex == mint.AccountIndex {
					if pre.UiTokenAmount.UiAmount == nil || mint.UiTokenAmount.UiAmount == nil {
						continue
					}
					preAmount := *pre.UiTokenAmount.UiAmount
					postAmount := *mint.UiTokenAmount.UiAmount
					if postAmount < preAmount {
						tokenIn = mint.Mint.String()
						amountIn = preAmount - postAmount
						p.logger.Debug().
							Str("signature", signature).
							Str("tokenIn", tokenIn).
							Float64("amountIn", amountIn).
							Msg("Identified input token")
					}
				}
			}
		}
	}

	// Second pass: Find the token being received (output token)
	// For this pass, we'll look for any increase in token balance
	for _, mint := range tx.Meta.PostTokenBalances {
		for _, pre := range tx.Meta.PreTokenBalances {
			if pre.AccountIndex == mint.AccountIndex {
				if pre.UiTokenAmount.UiAmount == nil || mint.UiTokenAmount.UiAmount == nil {
					continue
				}
				preAmount := *pre.UiTokenAmount.UiAmount
				postAmount := *mint.UiTokenAmount.UiAmount

				// Check for balance increase
				if postAmount > preAmount {
					// Special case for SOL token
					if mint.Mint.String() == "So11111111111111111111111111111111111111112" {
						tokenOut = mint.Mint.String()
						amountOut = postAmount - preAmount
						p.logger.Debug().
							Str("signature", signature).
							Str("tokenOut", tokenOut).
							Float64("amountOut", amountOut).
							Msg("Identified output token (SOL)")
					} else if mint.Owner.String() == walletAddress {
						// For other tokens, verify ownership
						tokenOut = mint.Mint.String()
						amountOut = postAmount - preAmount
						p.logger.Debug().
							Str("signature", signature).
							Str("tokenOut", tokenOut).
							Float64("amountOut", amountOut).
							Msg("Identified output token")
					}
				}
			}
		}
	}

	// Log before the token check
	p.logger.Debug().
		Str("signature", signature).
		Str("tokenIn", tokenIn).
		Str("tokenOut", tokenOut).
		Bool("hasTokenIn", tokenIn != "").
		Bool("hasTokenOut", tokenOut != "").
		Msg("Token identification complete")

	// If we couldn't identify both tokens, skip this transaction
	if tokenIn == "" || tokenOut == "" {
		p.logger.Debug().
			Str("signature", signature).
			Str("tokenIn", tokenIn).
			Str("tokenOut", tokenOut).
			Msg("Could not identify both tokens, skipping transaction")
		return nil, nil
	}

	p.logger.Debug().
		Str("signature", signature).
		Str("tokenIn", tokenIn).
		Str("tokenOut", tokenOut).
		Float64("amountIn", amountIn).
		Float64("amountOut", amountOut).
		Str("program", getProgramName(dexProgram)).
		Msg("Parsed swap transaction")

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

func abs64(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
