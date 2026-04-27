// Package api provides the WebSocket and REST API for the MITM proxy.
package api

import (
	"encoding/json"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 1024
	maxHubClients  = 512
)

// Hub manages WebSocket connections and broadcasts records to all clients.
type Hub struct {
	mu      sync.RWMutex
	clients map[*Client]bool

	// Channel for broadcasting records
	broadcast chan []byte

	// Register/unregister channels
	register   chan *Client
	unregister chan *Client
}

// Client represents a WebSocket client connection.
type Client struct {
	hub        *Hub
	conn       *websocket.Conn
	send       chan []byte
	remoteHost string
	clientID   string
}

// NewHub creates a new Hub instance.
func NewHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte, 256),
		register:   make(chan *Client),
		unregister: make(chan *Client),
	}
}

// Run starts the hub's main loop.
func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.removeClientsForClientIDLocked(client.clientID)
			for len(h.clients) >= maxHubClients {
				for existing := range h.clients {
					h.removeClientLocked(existing)
					break
				}
			}
			h.clients[client] = true
			h.mu.Unlock()

		case client := <-h.unregister:
			h.mu.Lock()
			h.removeClientLocked(client)
			h.mu.Unlock()

		case message := <-h.broadcast:
			h.mu.Lock()
			for client := range h.clients {
				select {
				case client.send <- message:
				default:
					// Client buffer full; drop it so future broadcasts remain healthy.
					h.removeClientLocked(client)
				}
			}
			h.mu.Unlock()
		}
	}
}

func (h *Hub) removeClientLocked(client *Client) {
	if _, ok := h.clients[client]; !ok {
		return
	}
	delete(h.clients, client)
	close(client.send)
	_ = client.conn.Close()
}

func (h *Hub) removeClientsForClientIDLocked(clientID string) {
	if clientID == "" {
		return
	}
	for client := range h.clients {
		if client.clientID == clientID {
			h.removeClientLocked(client)
		}
	}
}

// Broadcast sends a record to all connected clients.
func (h *Hub) Broadcast(record interface{}) {
	data, err := json.Marshal(record)
	if err != nil {
		return
	}

	select {
	case h.broadcast <- data:
	default:
		// Broadcast channel full, skip
	}
}

// ClientCount returns the number of connected clients.
func (h *Hub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	seen := make(map[string]bool, len(h.clients))
	for client := range h.clients {
		key := client.clientID
		if key == "" {
			key = "anon:" + client.remoteHost
		}
		seen[key] = true
	}
	return len(seen)
}

// Register adds a new client to the hub.
func (h *Hub) Register(client *Client) {
	h.register <- client
}

// Unregister removes a client from the hub.
func (h *Hub) Unregister(client *Client) {
	h.unregister <- client
}

// NewClient creates a new WebSocket client.
func NewClient(hub *Hub, conn *websocket.Conn, remoteAddr string, clientID string) *Client {
	remoteHost, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		remoteHost = remoteAddr
	}
	return &Client{
		hub:        hub,
		conn:       conn,
		send:       make(chan []byte, 256),
		remoteHost: remoteHost,
		clientID:   clientID,
	}
}

// WritePump pumps messages from the hub to the websocket connection.
func (c *Client) WritePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// ReadPump pumps messages from the websocket connection to the hub.
// Currently just handles connection close.
func (c *Client) ReadPump() {
	defer func() {
		c.hub.Unregister(c)
		c.conn.Close()
	}()

	c.conn.SetReadLimit(maxMessageSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			break
		}
	}
}
