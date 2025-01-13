package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"simo/internal/service"
	"simo/internal/service/websocket"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
)

type Handler struct {
	tracker service.WalletTrackerService
	wallet  service.WalletService
	logger  zerolog.Logger
}

func NewHandler(tracker service.WalletTrackerService, wallet service.WalletService, logger zerolog.Logger) *Handler {
	return &Handler{
		tracker: tracker,
		wallet:  wallet,
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

type WalletAliasResponse struct {
	ID       int    `json:"id"`
	WalletID int    `json:"wallet_id"`
	Alias    string `json:"alias"`
}

type WalletAliasesResponse struct {
	Aliases []WalletAliasResponse `json:"aliases"`
}

// Status response structs
type WalletStatusResponse struct {
	Address string                        `json:"address"`
	Status  *websocket.SubscriptionStatus `json:"status"`
}

type WebSocketStatusResponse struct {
	Websocket websocket.ConnectionStatus               `json:"websocket"`
	Wallets   map[string]*websocket.SubscriptionStatus `json:"wallets"`
}

// Request structs
type AddWalletRequest struct {
	Address string `json:"address"`
}

type AddWalletAliasRequest struct {
	Alias string `json:"alias"`
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

// Wallet alias handlers
func (h *Handler) AddWalletAlias(w http.ResponseWriter, r *http.Request) {
	address := chi.URLParam(r, "address")
	if address == "" {
		h.respondWithError(w, http.StatusBadRequest, errors.New("address is required"))
		return
	}

	var req AddWalletAliasRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.respondWithError(w, http.StatusBadRequest, errors.New("invalid request body"))
		return
	}

	if req.Alias == "" {
		h.respondWithError(w, http.StatusBadRequest, errors.New("alias is required"))
		return
	}

	alias, err := h.wallet.AddWalletAlias(r.Context(), address, req.Alias)
	if err != nil {
		h.respondWithError(w, http.StatusInternalServerError, err)
		return
	}

	h.respondWithJSON(w, http.StatusCreated, WalletAliasResponse{
		ID:       alias.ID,
		WalletID: alias.WalletID,
		Alias:    alias.Alias,
	})
}

func (h *Handler) RemoveWalletAlias(w http.ResponseWriter, r *http.Request) {
	address := chi.URLParam(r, "address")
	if address == "" {
		h.respondWithError(w, http.StatusBadRequest, errors.New("address is required"))
		return
	}

	alias := chi.URLParam(r, "alias")
	if alias == "" {
		h.respondWithError(w, http.StatusBadRequest, errors.New("alias is required"))
		return
	}

	if err := h.wallet.RemoveWalletAlias(r.Context(), address, alias); err != nil {
		h.respondWithError(w, http.StatusInternalServerError, err)
		return
	}

	h.respondWithJSON(w, http.StatusOK, struct{}{})
}

func (h *Handler) ListWalletAliases(w http.ResponseWriter, r *http.Request) {
	address := chi.URLParam(r, "address")
	if address == "" {
		h.respondWithError(w, http.StatusBadRequest, errors.New("address is required"))
		return
	}

	aliases, err := h.wallet.GetWalletAliases(r.Context(), address)
	if err != nil {
		h.respondWithError(w, http.StatusInternalServerError, err)
		return
	}

	response := WalletAliasesResponse{
		Aliases: make([]WalletAliasResponse, len(aliases)),
	}
	for i, alias := range aliases {
		response.Aliases[i] = WalletAliasResponse{
			ID:       alias.ID,
			WalletID: alias.WalletID,
			Alias:    alias.Alias,
		}
	}

	h.respondWithJSON(w, http.StatusOK, response)
}

func (h *Handler) GetWalletByAlias(w http.ResponseWriter, r *http.Request) {
	alias := chi.URLParam(r, "alias")
	if alias == "" {
		h.respondWithError(w, http.StatusBadRequest, errors.New("alias is required"))
		return
	}

	wallet, err := h.wallet.GetWalletByAlias(r.Context(), alias)
	if err != nil {
		h.respondWithError(w, http.StatusInternalServerError, err)
		return
	}

	h.respondWithJSON(w, http.StatusOK, WalletResponse{Address: wallet.Address})
}

// GetStatus returns the overall status of the WebSocket connection and all wallet subscriptions
func (h *Handler) GetStatus(w http.ResponseWriter, r *http.Request) {
	status := WebSocketStatusResponse{
		Websocket: h.tracker.GetConnectionStatus(),
		Wallets:   h.tracker.GetAllSubscriptionStatuses(),
	}
	h.respondWithJSON(w, http.StatusOK, status)
}

// GetWalletStatus returns the status of a specific wallet subscription
func (h *Handler) GetWalletStatus(w http.ResponseWriter, r *http.Request) {
	address := chi.URLParam(r, "address")
	if address == "" {
		h.respondWithError(w, http.StatusBadRequest, errors.New("address is required"))
		return
	}

	status, err := h.tracker.GetSubscriptionStatus(address)
	if err != nil {
		h.respondWithError(w, http.StatusNotFound, err)
		return
	}

	h.respondWithJSON(w, http.StatusOK, WalletStatusResponse{
		Address: address,
		Status:  status,
	})
}
