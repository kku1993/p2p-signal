package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log"
	"sync"
	"time"
)

// Alphabet without ambiguous characters (no 0/O/1/I/L), matching the original
// TypeScript implementation.
const roomAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

const (
	roomIDLen    = 5
	peerIDLen    = 6
	defaultMax   = 2
	tokenRandLen = 32 // bytes -> 64 hex chars (256-bit host token)
)

// Hub owns the set of active rooms. It is safe for concurrent use.
type Hub struct {
	mu    sync.RWMutex
	rooms map[string]*Room
	store Store // optional durable store; nil = in-memory only
}

func newHub() *Hub {
	return &Hub{rooms: make(map[string]*Room)}
}

// newHubWithStore returns a Hub backed by the given durable store. Rooms
// created through this hub are persisted, and rooms that existed before a
// restart are rehydrated on demand when looked up by id.
func newHubWithStore(s Store) *Hub {
	h := newHub()
	h.store = s
	return h
}

// CreateRoom allocates a new room with a unique id and host token. It returns
// the room and the plaintext host token (returned once to the caller so it can
// be sent to the host); only the SHA-256 hash of the token is retained by the
// room, so a memory dump does not reveal usable tokens.
func (h *Hub) CreateRoom(password string, maxClients int) (*Room, string, error) {
	if maxClients < 2 {
		maxClients = defaultMax
	}
	for attempts := 0; attempts < 32; attempts++ {
		id, err := randID(roomIDLen)
		if err != nil {
			return nil, "", err
		}
		h.mu.Lock()
		if _, ok := h.rooms[id]; ok {
			h.mu.Unlock()
			continue
		}
		token, err := randToken()
		if err != nil {
			h.mu.Unlock()
			return nil, "", err
		}
		r := &Room{
			ID:            id,
			Password:      password,
			MaxClients:    maxClients,
			HostTokenHash: hashToken(token),
			clients:       make(map[string]*Client),
		}
		h.rooms[id] = r
		h.mu.Unlock()

		// Persist before returning success so a crash after the host receives
		// the room id + token does not lose the room. On failure, roll back the
		// in-memory room.
		if h.store != nil {
			rec := &RoomRecord{
				ID:            r.ID,
				Password:      r.Password,
				MaxClients:    r.MaxClients,
				HostTokenHash: r.HostTokenHash,
				CreatedAt:     time.Now().UTC(),
			}
			if err := h.store.SaveRoom(context.Background(), rec); err != nil {
				h.mu.Lock()
				delete(h.rooms, id)
				h.mu.Unlock()
				return nil, "", fmt.Errorf("persist room %s: %w", id, err)
			}
		}
		return r, token, nil
	}
	return nil, "", errRoomCollision
}

// Room returns the room with the given id, or nil if it does not exist.
//
// If the room is not in memory but a Store is configured, the store is
// consulted and the room is rehydrated (with no connected clients) so a host
// can reconnect with its original token after a server restart.
func (h *Hub) Room(id string) *Room {
	h.mu.RLock()
	r := h.rooms[id]
	h.mu.RUnlock()
	if r != nil {
		return r
	}
	if h.store == nil {
		return nil
	}
	rec, err := h.store.Room(context.Background(), id)
	if err != nil || rec == nil {
		return nil
	}
	r = &Room{
		ID:            rec.ID,
		Password:      rec.Password,
		MaxClients:    rec.MaxClients,
		HostTokenHash: rec.HostTokenHash,
		clients:       make(map[string]*Client),
	}
	h.mu.Lock()
	// Re-check under write lock: another goroutine may have rehydrated the
	// same room concurrently.
	if existing, ok := h.rooms[id]; ok {
		h.mu.Unlock()
		return existing
	}
	h.rooms[id] = r
	h.mu.Unlock()
	return r
}

// remove deletes a room from the hub and, if a store is configured, from
// durable storage. Called with the room's own lock held is fine because we only
// touch the hub map here. Store I/O is done after releasing the hub lock to
// avoid blocking other room operations.
func (h *Hub) remove(id string) {
	h.mu.Lock()
	delete(h.rooms, id)
	h.mu.Unlock()
	if h.store != nil {
		if err := h.store.DeleteRoom(context.Background(), id); err != nil {
			log.Printf("[signaling] delete persisted room %s: %v", id, err)
		}
	}
}

// removeMemOnly evicts a room from the in-memory map but keeps the durable
// store record intact, so the room can be rehydrated when the host reconnects.
func (h *Hub) removeMemOnly(id string) {
	h.mu.Lock()
	delete(h.rooms, id)
	h.mu.Unlock()
}

// Room is a signaling room hosting up to MaxClients peers.
type Room struct {
	ID            string
	Password      string
	MaxClients    int
	HostTokenHash string // hex(SHA-256(host_token)); plaintext token is not stored

	mu      sync.Mutex
	hostID  string // peer id of the host ("" until host connects)
	clients map[string]*Client
}

// errRoomCollision is returned when unique room id generation fails.
var errRoomCollision = &errVal{"could not allocate unique room id"}

type errVal struct{ s string }

func (e *errVal) Error() string { return e.s }

// Admit adds a client to the room. hostToken non-empty claims the host slot.
// It returns the assigned peer id and the list of existing peer ids.
//
// Errors: bad token (guest claiming host), wrong password, room full, host
// slot already taken.
func (r *Room) Admit(c *Client, hostToken, password string) (peerID string, existing []string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	isHost := hostToken != "" && verifyHostToken(hostToken, r.HostTokenHash)
	if hostToken != "" && !isHost {
		return "", nil, &errVal{"invalid host token"}
	}
	// Only guests must satisfy the room password; the host is authenticated by
	// the host token and is exempt.
	if !isHost && r.Password != "" && r.Password != password {
		return "", nil, &errVal{"invalid password"}
	}
	if isHost {
		if r.hostID != "" {
			return "", nil, &errVal{"host already connected"}
		}
	} else {
		if r.hostID == "" {
			return "", nil, &errVal{"host has not joined yet"}
		}
		if len(r.clients) >= r.MaxClients {
			return "", nil, &errVal{"room is full"}
		}
	}

	// Allocate a unique peer id within this room.
	for attempts := 0; attempts < 32; attempts++ {
		id, err := randID(peerIDLen)
		if err != nil {
			return "", nil, err
		}
		if _, ok := r.clients[id]; ok {
			continue
		}
		peerID = id
		break
	}
	if peerID == "" {
		return "", nil, errRoomCollision
	}

	c.ID = peerID
	c.IsHost = isHost
	c.Room = r
	r.clients[peerID] = c
	if isHost {
		r.hostID = peerID
	}

	existing = make([]string, 0, len(r.clients)-1)
	for id := range r.clients {
		if id != peerID {
			existing = append(existing, id)
		}
	}
	return peerID, existing, nil
}

// Remove drops a client from the room. If the host leaves, hostID is cleared
// so the host can reconnect with the same token when persistence is enabled.
// Returns the remaining peer ids (so the caller can notify them) and whether
// the departing client was the host.
func (r *Room) Remove(c *Client) (remaining []string, hostLeft bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.clients, c.ID)
	if c.ID == r.hostID {
		r.hostID = "" // allow host reconnection with the same token
		remaining = make([]string, 0, len(r.clients))
		for id := range r.clients {
			remaining = append(remaining, id)
		}
		return remaining, true
	}
	if len(r.clients) == 0 {
		return nil, false
	}
	remaining = make([]string, 0, len(r.clients))
	for id := range r.clients {
		remaining = append(remaining, id)
	}
	return remaining, false
}

// Client returns the peer with the given id, or nil.
func (r *Room) Client(id string) *Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.clients[id]
}

// Broadcast sends a message to every peer in the room except skip (which may be
// nil to broadcast to everyone).
func (r *Room) Broadcast(msg *ServerOut, skip *Client) {
	r.mu.Lock()
	targets := make([]*Client, 0, len(r.clients))
	for _, c := range r.clients {
		if c != skip {
			targets = append(targets, c)
		}
	}
	r.mu.Unlock()
	for _, c := range targets {
		c.send(msg)
	}
}

// randID returns a random id of the given length over roomAlphabet.
func randID(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = roomAlphabet[int(b)%len(roomAlphabet)]
	}
	return string(out), nil
}

// randToken returns an opaque host token (plaintext). It is returned to the
// caller exactly once via the HTTP response; only its SHA-256 hash is stored.
func randToken() (string, error) {
	buf := make([]byte, tokenRandLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	const hex = "0123456789abcdef"
	out := make([]byte, tokenRandLen*2)
	for i, b := range buf {
		out[i*2] = hex[b>>4]
		out[i*2+1] = hex[b&0x0f]
	}
	return string(out), nil
}

// hashToken returns the lowercase hex SHA-256 digest of a host token. This is
// what the room stores; the plaintext token is never retained.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// verifyHostToken reports whether candidate hashes to the stored digest. The
// comparison is constant-time to avoid leaking information about the digest via
// timing. An empty stored digest matches nothing.
func verifyHostToken(candidate, storedHash string) bool {
	if storedHash == "" {
		return false
	}
	got := hashToken(candidate)
	return subtle.ConstantTimeCompare([]byte(got), []byte(storedHash)) == 1
}
