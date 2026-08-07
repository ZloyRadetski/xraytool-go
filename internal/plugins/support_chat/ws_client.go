package support_chat

import (
	"bytes"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	// In production, we should check origin. For plugin, assume host does CORS.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// wsClient is a middleman between the websocket connection and the hub.
type wsClient struct {
	hub *Hub

	// The websocket connection.
	conn *websocket.Conn

	// Buffered channel of outbound messages.
	send chan []byte

	userID string
	role   string // "admin" or "client"
	cfg    pluginConfig
}

func (c *wsClient) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()
	c.conn.SetReadLimit(512)
	c.conn.SetReadDeadline(time.Now().Add(c.cfg.WSPongTimeout))
	c.conn.SetPongHandler(func(string) error { c.conn.SetReadDeadline(time.Now().Add(c.cfg.WSPongTimeout)); return nil })
	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				c.hub.log.Error("error reading from websocket", "error", err)
			}
			break
		}
		// For now we don't expect messages FROM the client via WS, only via REST API.
		// If we did, we'd handle them here.
		message = bytes.TrimSpace(bytes.Replace(message, []byte{'\n'}, []byte{' '}, -1))
		c.hub.log.Debug("ignored websocket message from client", "message", string(message))
	}
}

func (c *wsClient) writePump() {
	ticker := time.NewTicker(c.cfg.WSPingInterval)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				// The hub closed the channel.
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)

			if err := w.Close(); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// serveWs handles websocket requests from the peer.
func (p *Plugin) serveWs(role string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !p.cfg.WebSocketEnabled {
			http.Error(w, "WebSockets are disabled", http.StatusForbidden)
			return
		}

		userID, ok := r.Context().Value("user_id").(string)
		if !ok || userID == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			p.log.Error("Failed to upgrade websocket", "error", err)
			return
		}

		client := &wsClient{
			hub:    p.hub,
			conn:   conn,
			send:   make(chan []byte, 256),
			userID: userID,
			role:   role,
			cfg:    p.cfg,
		}
		client.hub.register <- client

		// Allow collection of memory referenced by the caller by doing all work in
		// new goroutines.
		go client.writePump()
		go client.readPump()
	}
}
