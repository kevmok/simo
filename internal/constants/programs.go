package constants

import "github.com/gagliardetto/solana-go"

// DEX Program IDs
var (
	// Jupiter Aggregator v6
	JupiterV6Program = solana.MustPublicKeyFromBase58("JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4")

	// Raydium
	RaydiumV4Program = solana.MustPublicKeyFromBase58("675kPX9MHTjS2zt1qfr1NYHuzeLXfQM9H24wFSUt1Mp8")

	// Orca Whirlpools
	OrcaWhirlpoolProgram = solana.MustPublicKeyFromBase58("whirLbMiicVdio4qvUfM5KAg6Ct8VwpYzGff3uctyCc")
)

// Token Program
var (
	TokenProgram = solana.MustPublicKeyFromBase58("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA")
)

func IsDEXProgram(program solana.PublicKey) bool {
	dexPrograms := []solana.PublicKey{
		JupiterV6Program,
		RaydiumV4Program,
		OrcaWhirlpoolProgram,
	}

	for _, dex := range dexPrograms {
		if program.Equals(dex) {
			return true
		}
	}
	return false
}
