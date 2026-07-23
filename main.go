package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// Allow all origins: signaling is token/password gated, and clients are
	// typically browsers or native apps from any origin.
	CheckOrigin: func(r *http.Request) bool { return true },
}

func main() {
	addr := flag.String("addr", ":4000", "listen address")
	flag.Parse()
	if env := os.Getenv("SIGNALING_ADDR"); env != "" {
		*addr = env
	}

	hub := newHub()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/rooms", handleCreateRoom(hub))
	mux.HandleFunc("/v1/ws/", handleWS(hub))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("[signaling] listening on %s", *addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[signaling] server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Printf("[signaling] shutting down")
	_ = srv.Close()
}

// handleCreateRoom returns an http.HandlerFunc for POST /v1/rooms.
func handleCreateRoom(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req CreateRoomRequest
		if r.Body != nil {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil &&
				err != io.EOF {
				http.Error(w, "invalid request body", http.StatusBadRequest)
				return
			}
		}
		room, token, err := hub.CreateRoom(req.Password, req.MaxClients)
		if err != nil {
			http.Error(w, "could not create room", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, CreateRoomResponse{
			RoomID:    room.ID,
			HostToken: token,
		})
	}
}

// handleWS returns an http.HandlerFunc for GET /v1/ws/<room-id>.
func handleWS(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Path: /v1/ws/<room-id>
		roomID := r.URL.Path[len("/v1/ws/"):]
		if roomID == "" || roomID == "/" {
			http.Error(w, "missing room id", http.StatusBadRequest)
			return
		}
		room := hub.Room(roomID)
		if room == nil {
			http.Error(w, "room not found", http.StatusNotFound)
			return
		}

		// Query params: token (host claim), password (room password).
		q := r.URL.Query()
		hostToken := q.Get("token")
		password := q.Get("password")

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			// Upgrade already wrote an HTTP error response.
			return
		}

		client := newClient(hub, conn)
		peerID, existing, err := room.Admit(client, hostToken, password)
		if err != nil {
			client.reject(err.Error())
			return
		}

		// Tell the newcomer its own id and the peers already present.
		client.send(&ServerOut{
			Type:   "joined",
			Room:   room.ID,
			PeerID: peerID,
			Peers:  existing,
		})
		// Tell everyone else about the newcomer.
		room.Broadcast(&ServerOut{Type: "peer-joined", PeerID: peerID}, client)

		go client.writePump()
		client.readPump() // blocks until disconnect
	}
}

// reject sends an error to a freshly-upgraded client and closes the connection.
// writePump is not running yet at this point, so the error is written directly.
func (c *Client) reject(reason string) {
	data, _ := json.Marshal(&ServerOut{Type: "error", Message: reason})
	c.conn.SetWriteDeadline(time.Now().Add(writeWait))
	_ = c.conn.WriteMessage(websocket.TextMessage, data)
	_ = c.conn.WriteMessage(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.ClosePolicyViolation, reason),
	)
	c.sendMu.Lock()
	c.closed = true
	close(c.sendCh)
	c.sendMu.Unlock()
	c.conn.Close()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
