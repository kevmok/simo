package main

import (
	"context"
	"os"
	"simo/internal/service"

	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func main() {
	// Load environment variables
	if err := godotenv.Load(); err != nil {
		log.Fatal().Err(err).Msg("Error loading .env file")
	}

	// Initialize logger
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	config := service.Config{
		WebhookURL:   os.Getenv("DISCORD_WEBHOOK_URL"),
		SolanaRPCURL: os.Getenv("SOLANA_RPC_URL"),
	}

	ctx := context.Background()
	tracker, err := service.NewWalletTracker(ctx, config, logger)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize wallet tracker")
	}
	defer tracker.Close()

	// TODO: Add your main application logic here
}
