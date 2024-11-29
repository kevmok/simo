package main

import (
	"context"
	"os"
	"os/signal"
	"simo/internal/handler"
	"simo/internal/server"
	"simo/internal/service"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func main() {
	// Load environment variables
	if _, err := os.Stat(".env"); err == nil {
		err := godotenv.Load()
		if err != nil {
			log.Fatal().Msg("Error loading .env file")
		}
		log.Info().Msg("Loaded .env file")
	} else if os.IsNotExist(err) {
		log.Info().Msg("No .env file found, continuing with default environment")
	} else {
		log.Info().Msgf("Error checking .env file: %v", err)
	}

	// Initialize logger
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	config := service.Config{
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

	// Initialize handler and server
	h := handler.NewHandler(tracker, logger)
	srv := server.NewServer(server.Config{
		Port:            "8080",
		ShutdownTimeout: 30 * time.Second,
	}, h, logger)

	// Set up signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start the server in a goroutine
	go func() {
		if err := srv.Start(); err != nil {
			logger.Error().Err(err).Msg("Server error")
			cancel() // Cancel main context if server fails
		}
	}()

	// Wait for either a signal or context cancellation
	select {
	case sig := <-sigChan:
		logger.Info().
			Str("signal", sig.String()).
			Msg("Received shutdown signal")
			// Shutdown with timeout
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error().Err(err).Msg("Server shutdown failed")
		}
	case <-ctx.Done():
		logger.Info().Msg("Context canceled")
	}

	// Perform graceful shutdown
	logger.Info().Msg("Shutting down gracefully...")
}
