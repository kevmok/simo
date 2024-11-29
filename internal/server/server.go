package server

import (
	"context"
	"fmt"
	"net/http"
	"simo/internal/handler"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
)

type Server struct {
	httpServer *http.Server
	logger     zerolog.Logger
}

type Config struct {
	Port            string
	ShutdownTimeout time.Duration
}

func NewServer(cfg Config, h *handler.Handler, logger zerolog.Logger) *Server {
	logger.Info().Str("port", cfg.Port).Msg("Server will start on port")
	r := chi.NewRouter()

	// Middleware
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))

	// Routes
	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/wallets", h.AddWallet)
		r.Get("/wallets", h.ListWallets)
		r.Delete("/wallets/{address}", h.RemoveWallet)
	})

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%s", cfg.Port),
		Handler: r,
	}

	return &Server{
		httpServer: srv,
		logger:     logger,
	}
}

func (s *Server) Start() error {
	s.logger.Info().Str("addr", s.httpServer.Addr).Msg("Starting HTTP server")
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server failed to start: %w", err)
	}
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info().Msg("Shutting down server...")
	return s.httpServer.Shutdown(ctx)
}
