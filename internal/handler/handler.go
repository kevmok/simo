package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"simo/internal/service"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
)

type Handler struct {
	tracker service.WalletTrackerService
	logger  zerolog.Logger
}

func NewHandler(tracker service.WalletTrackerService, logger zerolog.Logger) *Handler {
	return &Handler{
		tracker: tracker,
		logger:  logger,
	}
}

// Response structs
type ErrorResponse struct {
	Error string `json:"error"`
}

type WalletResponse struct {
	Address string `json:"address"`
}

type WalletsResponse struct {
	Wallets []WalletResponse `json:"wallets"`
}

// Request structs
type AddWalletRequest struct {
	Address string `json:"address"`
}

// Utility methods
func (h *Handler) respondWithError(w http.ResponseWriter, code int, err error) {
	h.respondWithJSON(w, code, ErrorResponse{Error: err.Error()})
}

func (h *Handler) respondWithJSON(w http.ResponseWriter, code int, payload interface{}) {
	response, err := json.Marshal(payload)
	if err != nil {
		h.logger.Error().Err(err).Msg("Failed to marshal JSON response")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(response)
}

// Handler methods
func (h *Handler) AddWallet(w http.ResponseWriter, r *http.Request) {
	var req AddWalletRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.respondWithError(w, http.StatusBadRequest, errors.New("invalid request body"))
		return
	}

	if err := h.tracker.AddWallet(r.Context(), req.Address); err != nil {
		h.respondWithError(w, http.StatusInternalServerError, err)
		return
	}

	h.respondWithJSON(w, http.StatusCreated, WalletResponse{Address: req.Address})
}

func (h *Handler) ListWallets(w http.ResponseWriter, r *http.Request) {
	wallets, err := h.tracker.GetWallets(r.Context())
	if err != nil {
		h.respondWithError(w, http.StatusInternalServerError, err)
		return
	}

	response := WalletsResponse{
		Wallets: make([]WalletResponse, len(wallets)),
	}
	for i, wallet := range wallets {
		response.Wallets[i] = WalletResponse{Address: wallet.Address}
	}

	h.respondWithJSON(w, http.StatusOK, response)
}

func (h *Handler) RemoveWallet(w http.ResponseWriter, r *http.Request) {
	address := chi.URLParam(r, "address")
	if address == "" {
		h.respondWithError(w, http.StatusBadRequest, errors.New("address is required"))
		return
	}

	if err := h.tracker.RemoveWallet(r.Context(), address); err != nil {
		h.respondWithError(w, http.StatusInternalServerError, err)
		return
	}

	h.respondWithJSON(w, http.StatusOK, WalletResponse{Address: address})
}
