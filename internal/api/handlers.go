package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gorilla/websocket"
)

// RecordStore interface for accessing records.
type RecordStore interface {
	GetRecentRecords(limit int) []interface{}
	ClearRecords() error
}

// Handler provides HTTP handlers for the API.
type Handler struct {
	hub   *Hub
	store RecordStore
}

// NewHandler creates a new API handler.
func NewHandler(hub *Hub, store RecordStore) *Handler {
	return &Handler{
		hub:   hub,
		store: store,
	}
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for development
	},
}

// HandleWebSocket handles WebSocket connections for real-time record streaming.
func (h *Handler) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		http.Error(w, "Failed to upgrade connection", http.StatusInternalServerError)
		return
	}

	client := NewClient(h.hub, conn, r.RemoteAddr, r.URL.Query().Get("client_id"))
	h.hub.Register(client)

	// Start pumps
	go client.WritePump()
	client.ReadPump()
}

// HandleGetRecords handles GET /api/records - returns recent records for initial load
func (h *Handler) HandleGetRecords(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 && l <= 1000 {
			limit = l
		}
	}

	records := h.store.GetRecentRecords(limit)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(records)
}

// HandleClearRecords handles DELETE /api/records - clears live traffic records.
func (h *Handler) HandleClearRecords(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if err := h.store.ClearRecords(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// HandleCORS handles CORS preflight requests.
func (h *Handler) HandleCORS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.WriteHeader(http.StatusOK)
}

// RegisterRoutes registers all API routes on the given mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// WebSocket endpoint for real-time streaming
	mux.HandleFunc("/ws/records", h.HandleWebSocket)

	// GET /api/records - returns recent records for initial load
	mux.HandleFunc("/api/records", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			h.HandleCORS(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			h.HandleGetRecords(w, r)
		case http.MethodDelete:
			h.HandleClearRecords(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}
