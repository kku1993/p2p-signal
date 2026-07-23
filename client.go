package main

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 64 * 1024
	sendBufSize    = 64
)

// Client is a single WebSocket connection bound to a Room.
type Client struct {
	ID     string // peer id within the room; assigned on admit
	IsHost bool
	Room   *Room
	hub    *Hub
	conn   *websocket.Conn

	sendMu sync.Mutex
	closed bool
	sendCh chan []byte
}

func newClient(hub *Hub, conn *websocket.Conn) *Client {
	return &Client{
		hub:    hub,
		conn:   conn,
		sendCh: make(chan []byte, sendBufSize),
	}
}

// sendCh queues a message for writing. It is safe to call concurrently with
// teardown: once teardown has marked the client closed, further sends are
// dropped silently rather than panicking on a closed channel.
func (c *Client) send(msg *ServerOut) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.closed {
		return
	}
	select {
	case c.sendCh <- data:
	default:
		log.Printf("[signaling] dropping message for peer %s (sendCh buffer full)", c.ID)
	}
}

// readPump reads messages from the WebSocket and dispatches them.
func (c *Client) readPump() {
	defer func() {
		c.teardown()
		c.conn.Close()
	}()
	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseNormalClosure,
				websocket.CloseGoingAway) {
				log.Printf("[signaling] read error from peer %s: %v", c.ID, err)
			}
			return
		}
		var in ClientIn
		if err := json.Unmarshal(raw, &in); err != nil {
			c.send(&ServerOut{Type: "error", Message: "Invalid JSON"})
			continue
		}
		if c.handle(in) {
			return // explicit leave: close the connection
		}
	}
}

// handle dispatches one inbound message. Returns true if the connection should
// be closed (i.e. on "leave").
func (c *Client) handle(in ClientIn) bool {
	switch in.Type {
	case "offer", "answer", "ice":
		c.relay(in)
	case "leave":
		c.conn.WriteMessage(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "leave"),
		)
		return true
	default:
		c.send(&ServerOut{Type: "error", Message: "Unknown type: " + in.Type})
	}
	return false
}

// relay forwards a signaling message to the targeted peer.
func (c *Client) relay(in ClientIn) {
	if in.To == "" {
		c.send(&ServerOut{Type: "error", Message: "Missing 'to' peer id"})
		return
	}
	peer := c.Room.Client(in.To)
	if peer == nil {
		c.send(&ServerOut{Type: "error", Message: "Peer not found: " + in.To})
		return
	}
	out := &ServerOut{
		Type: in.Type,
		From: c.ID,
		To:   in.To,
	}
	switch in.Type {
	case "offer", "answer":
		out.SDP = in.SDP
	case "ice":
		out.Candidate = in.Candidate
	}
	peer.send(out)
}

// writePump flushes the sendCh channel to the WebSocket and issues pings.
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case data, ok := <-c.sendCh:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// teardown removes the client from its room, notifies survivors, and shuts down
// the sendCh channel. Safe to call once (from readPump's defer).
//
// Room destruction policy:
//   - No persistence + host leaves: destroy the room entirely (memory + store).
//   - Persistence + host leaves + no guests: evict from memory, keep the store
//     record so the host can reconnect with the same token.
//   - Persistence + host leaves + guests remain: keep the room alive in memory
//     and store; guests stay connected and the host can reconnect.
//   - Last guest leaves and host already gone: destroy the room entirely.
func (c *Client) teardown() {
	if c.Room == nil {
		// Never admitted (e.g. bad room/password). Just close the sendCh channel.
		c.sendMu.Lock()
		c.closed = true
		close(c.sendCh)
		c.sendMu.Unlock()
		return
	}
	remaining, hostLeft := c.Room.Remove(c)
	for _, id := range remaining {
		if peer := c.Room.Client(id); peer != nil {
			peer.send(&ServerOut{Type: "peer-left", PeerID: c.ID})
		}
	}
	switch {
	case hostLeft && c.hub.store == nil:
		// No persistence: host departure destroys the room.
		c.hub.remove(c.Room.ID)
	case hostLeft && len(remaining) == 0:
		// Persistence enabled, no guests: evict from memory but keep the
		// store record so the host can reconnect.
		c.hub.removeMemOnly(c.Room.ID)
	case !hostLeft && len(remaining) == 0:
		// Last guest left and host already gone: room is completely dead.
		c.hub.remove(c.Room.ID)
	}
	// else: host left with guests (persisted) — keep room alive for reconnection;
	//   or guest left with peers remaining — room stays as-is.
	c.sendMu.Lock()
	c.closed = true
	close(c.sendCh)
	c.sendMu.Unlock()
	c.Room = nil
}
