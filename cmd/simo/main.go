package main

import (
	"context"
	"os"
	"os/signal"
	"simo/internal/service"
	"syscall"

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
		WebhookURL:         os.Getenv("DISCORD_WEBHOOK_URL"),
		SolanaRPCURL:       os.Getenv("SOLANA_RPC_URL"),
		SolanaWebSocketURL: os.Getenv("SOLANA_WEBSOCKET_URL"),
		SolanaTrackerKey:   os.Getenv("SOLANA_TRACKER_API_KEY"),
	}

	if config.SolanaTrackerKey == "" {
		log.Fatal().Msg("SOLANA_TRACKER_API_KEY is required")
	}

	// Create a context that will be canceled on signal
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tracker, err := service.NewWalletTracker(ctx, config, logger)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize wallet tracker")
	}
	defer tracker.Close()

	// Set up signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Wait for either a signal or context cancellation
	select {
	case sig := <-sigChan:
		logger.Info().
			Str("signal", sig.String()).
			Msg("Received shutdown signal")
	case <-ctx.Done():
		logger.Info().Msg("Context canceled")
	}

	// Perform graceful shutdown
	logger.Info().Msg("Shutting down gracefully...")
}
