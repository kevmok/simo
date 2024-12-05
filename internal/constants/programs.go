package constants

import "github.com/gagliardetto/solana-go"

// DEX Program IDs
var (
	JupiterV6Program     = solana.MustPublicKeyFromBase58("JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4")
	RaydiumV4Program     = solana.MustPublicKeyFromBase58("675kPX9MHTjS2zt1qfr1NYHuzeLXfQM9H24wFSUt1Mp8")
	OrcaWhirlpoolProgram = solana.MustPublicKeyFromBase58("whirLbMiicVdio4qvUfM5KAg6Ct8VwpYzGff3uctyCc")
	PumpfunProgram       = solana.MustPublicKeyFromBase58("6EF8rrecthR5Dkzon8Nwu78hRvfCKubJ14M5uBEwF6P")
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
