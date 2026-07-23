package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
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

const (
	// maxCreateBodySize caps the POST /v1/rooms request body so an attacker
	// cannot pin memory with a huge body.
	maxCreateBodySize = 16 * 1024
	// maxPasswordLen caps the room password length accepted at creation time.
	maxPasswordLen = 1024
	// shutdownTimeout is the upper bound on draining in-flight connections
	// during a graceful shutdown.
	shutdownTimeout = 10 * time.Second
	// janitorInterval is how often the background janitor scans for expired
	// rooms and reaps stale rate-limiter buckets.
	janitorInterval = time.Minute
)

func main() {
	addr := flag.String("addr", ":4000", "listen address")
	storeDir := flag.String("store-dir", "", "directory for persistent room storage (empty = in-memory only)")
	maxRooms := flag.Int("max-rooms", defaultMaxRooms, "maximum number of concurrent live rooms (0 = unlimited)")
	maxClientsLimit := flag.Int("max-clients-limit", defaultMaxClientsLimit, "upper bound on a room's max_clients (0 = unlimited)")
	roomCreateRate := flag.Float64("room-create-rate", defaultRoomCreateRate, "per-IP room-creation refill rate, tokens/sec (0 = no limit)")
	roomCreateBurst := flag.Int("room-create-burst", defaultRoomCreateBurst, "per-IP room-creation burst size")
	roomHostGrace := flag.Duration("room-host-grace", 15*time.Minute, "evict rooms whose host never joined after this duration (0 = disabled)")
	roomIdleTimeout := flag.Duration("room-idle-timeout", 4*time.Hour, "evict rooms with no connected peers after this idle duration (0 = disabled)")
	showVersion := flag.Bool("version", false, "print server version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if env := os.Getenv("SIGNALING_ADDR"); env != "" {
		*addr = env
	}
	if env := os.Getenv("SIGNALING_STORE_DIR"); env != "" {
		*storeDir = env
	}
	if env := os.Getenv("SIGNALING_MAX_ROOMS"); env != "" {
		if v, err := parseIntEnv(env); err == nil {
			*maxRooms = v
		}
	}
	if env := os.Getenv("SIGNALING_MAX_CLIENTS_LIMIT"); env != "" {
		if v, err := parseIntEnv(env); err == nil {
			*maxClientsLimit = v
		}
	}

	var hub *Hub
	if *storeDir != "" {
		fs, err := newFileStore(*storeDir)
		if err != nil {
			log.Fatalf("[signaling] open store: %v", err)
		}
		defer fs.Close()
		hub = newHubWithStore(fs)
		log.Printf("[signaling] persisting rooms to %s", *storeDir)
	} else {
		hub = newHub()
	}
	hub.maxRooms = *maxRooms
	hub.maxClientsLimit = *maxClientsLimit
	hub.hostGrace = *roomHostGrace
	hub.idleTimeout = *roomIdleTimeout
	if *roomCreateRate > 0 {
		hub.createLimiter = newRateLimiter(*roomCreateRate, *roomCreateBurst)
	}

	var ready atomic.Bool
	ready.Store(true)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/rooms", handleCreateRoom(hub))
	mux.HandleFunc("/v1/ws/", handleWS(hub))
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/readyz", handleReadyz(&ready, hub))

	// Wrap the mux so every response carries the server version header.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-P2P-Signal-Server-Version", version)
		mux.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Background janitor: evicts expired rooms and reaps stale rate-limiter
	// buckets. Stops when janitorCtx is cancelled during shutdown.
	janitorCtx, janitorCancel := context.WithCancel(context.Background())
	defer janitorCancel()
	go runJanitor(janitorCtx, hub, janitorInterval)

	go func() {
		log.Printf("[signaling] listening on %s", *addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("[signaling] server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Printf("[signaling] shutting down")
	ready.Store(false)
	janitorCancel()
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("[signaling] shutdown error: %v", err)
	}
}

// runJanitor periodically evicts expired rooms and reaps stale rate-limiter
// buckets until ctx is cancelled.
func runJanitor(ctx context.Context, hub *Hub, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			hub.evictExpired(now)
			if hub.createLimiter != nil {
				hub.createLimiter.sweep(now)
			}
		}
	}
}

// handleCreateRoom returns an http.HandlerFunc for POST /v1/rooms.
func handleCreateRoom(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Per-IP rate limit on room creation to blunt room-flooding attacks.
		if hub.createLimiter != nil {
			if !hub.createLimiter.allow(clientIP(r)) {
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}
		}
		// Bound the request body so an attacker cannot pin memory with a huge
		// POST (e.g. an oversized password field).
		r.Body = http.MaxBytesReader(w, r.Body, maxCreateBodySize)
		var req CreateRoomRequest
		if r.Body != nil {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil &&
				err != io.EOF {
				http.Error(w, "invalid request body", http.StatusBadRequest)
				return
			}
		}
		if len(req.Password) > maxPasswordLen {
			http.Error(w, "password too long", http.StatusBadRequest)
			return
		}
		room, token, err := hub.CreateRoom(req.Password, req.MaxClients)
		if err != nil {
			switch err {
			case errTooManyRooms:
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
			default:
				http.Error(w, "could not create room", http.StatusInternalServerError)
			}
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

		// Persist updated room state (HostJoined/LastActive) when a host
		// (re)connects, so the on-disk record reflects that the host has
		// claimed the room and the janitor does not evict it as "never joined".
		if client.IsHost {
			hub.persistRoom(room)
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

// handleHealthz is a liveness probe: the process is up and serving.
func handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleReadyz returns a readiness probe: 200 when the server is accepting
// traffic, 503 once shutdown has begun. The store (if any) is not pinged here
// because the provided fileStore has no health check; a future shared backend
// could extend this.
func handleReadyz(ready *atomic.Bool, hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !ready.Load() {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}

// clientIP extracts the remote peer's IP from the request, stripping the port
// from RemoteAddr. It deliberately does not trust X-Forwarded-For, which is
// spoofable by the client; operators behind a trusted proxy should extend this
// to consult configured proxy headers.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func parseIntEnv(s string) (int, error) {
	var v int
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
		return 0, err
	}
	return v, nil
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
