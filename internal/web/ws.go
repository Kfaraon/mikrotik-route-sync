package web

import (
	"log/slog"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true }, // LAN-only, см. middleware
}

type Hub struct {
	mu      sync.RWMutex
	clients map[*websocket.Conn]struct{}
}

func NewHub() *Hub {
	return &Hub{clients: map[*websocket.Conn]struct{}{}}
}

func (h *Hub) HandleWS(w http.ResponseWriter, r *http.Request) {
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("ws upgrade failed", "err", err)
		return
	}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()

	go func() {
		defer func() {
			h.mu.Lock()
			delete(h.clients, c)
			h.mu.Unlock()
			_ = c.Close()
		}()
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

// Broadcast рассылает JSON всем подключённым клиентам.
func (h *Hub) Broadcast(v any) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		if err := c.WriteJSON(v); err != nil {
			_ = c.Close()
			delete(h.clients, c)
		}
	}
}
