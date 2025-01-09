package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"simo/internal/handler"
	"simo/internal/server"
	"simo/internal/service"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
)

func main() {
	// Set up error channel to handle fatal errors from goroutines
	errChan := make(chan error, 1)

	// Create a base context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize logger first - this ensures we have logging before anything else
	logger := zerolog.New(os.Stdout).
		With().
		Timestamp().
		Caller(). // Added caller info for better debugging
		Logger()

	// Load environment with better error handling
	if err := loadEnvironment(&logger); err != nil {
		logger.Fatal().Err(err).Msg("Failed to load environment")
	}

	// Validate configuration
	config := service.Config{
		SolanaRPCURL:       os.Getenv("SOLANA_RPC_URL"),
		SolanaWebSocketURL: os.Getenv("SOLANA_WEBSOCKET_URL"),
		SolanaTrackerKey:   os.Getenv("SOLANA_TRACKER_API_KEY"),
	}

	if err := validateConfig(config); err != nil {
		logger.Fatal().Err(err).Msg("Invalid configuration")
	}

	// Initialize components with proper shutdown order
	components, err := initializeComponents(ctx, config, &logger)
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to initialize components")
	}
	defer components.cleanup()

	// Set up signal handling with buffered channel
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	// Start server in goroutine with error reporting
	go func() {
		if err := components.server.Start(); err != nil {
			if !errors.Is(err, http.ErrServerClosed) {
				errChan <- fmt.Errorf("server error: %w", err)
			}
		}
	}()

	// Main event loop
	select {
	case sig := <-sigChan:
		logger.Info().
			Str("signal", sig.String()).
			Msg("Received shutdown signal")

		// Initiate graceful shutdown
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()

		if err := gracefulShutdown(shutdownCtx, components, &logger); err != nil {
			logger.Error().Err(err).Msg("Graceful shutdown failed")
			os.Exit(1)
		}

	case err := <-errChan:
		logger.Error().Err(err).Msg("Received fatal error")
		// Trigger shutdown on fatal error
		cancel()

	case <-ctx.Done():
		logger.Info().Msg("Context canceled")
	}

	logger.Info().Msg("Shutdown complete")
}

// Components holds all service components for coordinated shutdown
type Components struct {
	tracker *service.WalletTracker
	server  *server.Server
}

func (c *Components) cleanup() {
	if c.tracker != nil {
		c.tracker.Close()
	}
}

func loadEnvironment(logger *zerolog.Logger) error {
	if _, err := os.Stat(".env"); err == nil {
		if err := godotenv.Load(); err != nil {
			return fmt.Errorf("failed to load .env: %w", err)
		}
		logger.Info().Msg("Loaded .env file")
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("error checking .env file: %w", err)
	}
	logger.Info().Msg("No .env file found, using environment variables")
	return nil
}

func validateConfig(config service.Config) error {
	if config.SolanaTrackerKey == "" {
		return errors.New("SOLANA_TRACKER_API_KEY is required")
	}
	if config.SolanaRPCURL == "" {
		return errors.New("SOLANA_RPC_URL is required")
	}
	if config.SolanaWebSocketURL == "" {
		return errors.New("SOLANA_WEBSOCKET_URL is required")
	}
	return nil
}

func initializeComponents(ctx context.Context, config service.Config, logger *zerolog.Logger) (*Components, error) {
	// Initialize tracker with timeout
	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	tracker, err := service.NewWalletTracker(initCtx, config, *logger)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize wallet tracker: %w", err)
	}

	walletManager := service.NewWalletManager(tracker.GetRepository(), *logger)

	// Initialize handler and server
	handler := handler.NewHandler(tracker, walletManager, *logger)
	srv := server.NewServer(server.Config{
		Port:            "8080",
		ShutdownTimeout: 30 * time.Second,
	}, handler, *logger)

	return &Components{
		tracker: tracker,
		server:  srv,
	}, nil
}

func gracefulShutdown(ctx context.Context, components *Components, logger *zerolog.Logger) error {
	// Stop accepting new connections first
	if err := components.server.Shutdown(ctx); err != nil {
		logger.Error().Err(err).Msg("Failed to shutdown server")
	}

	// Then close the tracker (websocket connections)
	components.tracker.Close()

	return nil
}
