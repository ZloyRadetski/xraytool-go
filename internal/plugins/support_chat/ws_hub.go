package support_chat

import (
	"sync"
	"xraytool/internal/pluginapi"
)

// Hub manages active WebSocket connections and broadcasts messages.
type Hub struct {
	// Registered clients. Key is the wsClient pointer.
	clients map[*wsClient]bool

	// Inbound messages to broadcast.
	broadcast chan broadcastMsg

	// Register requests from the clients.
	register chan *wsClient

	// Unregister requests from clients.
	unregister chan *wsClient

	log pluginapi.Logger
	mu  sync.RWMutex
}

// broadcastMsg encapsulates a message and its target audience.
type broadcastMsg struct {
	targetUserID string // if empty, broadcast to admins. if "*", broadcast to all (or specific logic)
	targetRole   string // "admin", "client", or empty
	data         []byte
}

func newHub(log pluginapi.Logger) *Hub {
	return &Hub{
		broadcast:  make(chan broadcastMsg, 256),
		register:   make(chan *wsClient, 64),
		unregister: make(chan *wsClient, 64),
		clients:    make(map[*wsClient]bool),
		log:        log,
	}
}

func (h *Hub) run(stopCh <-chan struct{}) {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()
			h.log.Debug("WebSocket client registered", "role", client.role, "user_id", client.userID)

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
				h.log.Debug("WebSocket client unregistered", "role", client.role, "user_id", client.userID)
			}
			h.mu.Unlock()

		case msg := <-h.broadcast:
			// A full client send buffer requires removing that client. Hold the
			// write lock for the whole iteration so map mutation is never made
			// under RLock (or concurrently with another map operation).
			h.mu.Lock()
			for client := range h.clients {
				// Broadcast logic:
				// - If msg.targetRole == "admin", send only to admins
				// - If msg.targetUserID != "", send only to that specific client (and maybe admins)

				shouldSend := false
				if msg.targetRole == "admin" && client.role == "admin" {
					shouldSend = true
				} else if msg.targetUserID != "" && client.userID == msg.targetUserID && client.role == "client" {
					shouldSend = true
				} else if msg.targetRole == "" && msg.targetUserID == "" {
					shouldSend = true // broadcast to all
				}

				if shouldSend {
					select {
					case client.send <- msg.data:
					default:
						// Send buffer is full, drop the client
						delete(h.clients, client)
						close(client.send)
					}
				}
			}
			h.mu.Unlock()

		case <-stopCh:
			h.mu.Lock()
			for client := range h.clients {
				close(client.send)
				delete(h.clients, client)
			}
			h.mu.Unlock()
			return
		}
	}
}

// BroadcastToAdmins sends a message to all connected admins.
func (h *Hub) BroadcastToAdmins(data []byte) {
	h.enqueueBroadcast(broadcastMsg{
		targetRole: "admin",
		data:       data,
	})
}

// BroadcastToClient sends a message to a specific client.
func (h *Hub) BroadcastToClient(userID string, data []byte) {
	h.enqueueBroadcast(broadcastMsg{
		targetUserID: userID,
		data:         data,
	})
}

// enqueueBroadcast deliberately drops a message when the bounded hub queue is
// full. A slow or disconnected WebSocket consumer must not be able to block
// an HTTP request goroutine indefinitely.
func (h *Hub) enqueueBroadcast(msg broadcastMsg) {
	select {
	case h.broadcast <- msg:
	default:
		if h.log != nil {
			h.log.Warn("WebSocket broadcast queue is full; dropping message")
		}
	}
}
