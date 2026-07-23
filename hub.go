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

	// defaultMaxClientsLimit caps the max_clients a room may request. Full-mesh
	// WebRTC signaling stops scaling beyond a handful of peers, so this is also
	// a natural safety valve against lopsided resource use.
	defaultMaxClientsLimit = 16
	// defaultMaxRooms caps the total number of live rooms in one process.
	defaultMaxRooms = 10000
	// defaultRoomCreateRate is the per-IP room-creation refill rate (tokens/sec).
	defaultRoomCreateRate = 0.2 // ~12 rooms/min per IP
	// defaultRoomCreateBurst is the per-IP room-creation burst size.
	defaultRoomCreateBurst = 5
)

// Hub owns the set of active rooms. It is safe for concurrent use.
//
// The optional limits (maxRooms, maxClientsLimit, createLimiter, hostGrace,
// idleTimeout) are zero/unset on a hub created with newHub() and only
// configured by main() from flags. When unset they are not enforced, which
// keeps the unit tests (which construct hubs directly) unaffected.
type Hub struct {
	mu    sync.RWMutex
	rooms map[string]*Room
	store Store // optional durable store; nil = in-memory only

	maxRooms        int           // 0 = unlimited
	maxClientsLimit int           // 0 = unlimited
	createLimiter   *rateLimiter  // nil = no per-IP limit
	hostGrace       time.Duration // 0 = no "host never joined" expiry
	idleTimeout     time.Duration // 0 = no idle expiry
}

func newHub() *Hub {
	return &Hub{rooms: make(map[string]*Room)}
}

// peekRoom returns the in-memory room without consulting the store.
func (h *Hub) peekRoom(id string) *Room {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.rooms[id]
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
//
// The plaintext password is likewise hashed before storage; the room never
// retains the plaintext password.
func (h *Hub) CreateRoom(password string, maxClients int) (*Room, string, error) {
	if h.maxClientsLimit > 0 && maxClients > h.maxClientsLimit {
		maxClients = h.maxClientsLimit
	}
	if maxClients < 2 {
		maxClients = defaultMax
	}
	for attempts := 0; attempts < 32; attempts++ {
		id, err := randID(roomIDLen)
		if err != nil {
			return nil, "", err
		}
		h.mu.Lock()
		if h.maxRooms > 0 && len(h.rooms) >= h.maxRooms {
			h.mu.Unlock()
			return nil, "", errTooManyRooms
		}
		if _, ok := h.rooms[id]; ok {
			h.mu.Unlock()
			continue
		}
		token, err := randToken()
		if err != nil {
			h.mu.Unlock()
			return nil, "", err
		}
		now := time.Now().UTC()
		r := &Room{
			ID:            id,
			PasswordHash:  hashPassword(password),
			MaxClients:    maxClients,
			HostTokenHash: hashToken(token),
			CreatedAt:     now,
			LastActive:    now,
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
				PasswordHash:  r.PasswordHash,
				MaxClients:    r.MaxClients,
				HostTokenHash: r.HostTokenHash,
				CreatedAt:     r.CreatedAt,
				LastActive:    r.LastActive,
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
// can reconnect with its original token after a server restart. Persisted
// records that have exceeded their TTL/idle timeout are lazily evicted here
// rather than admitted.
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
	if h.recordExpired(rec, time.Now()) {
		if err := h.store.DeleteRoom(context.Background(), id); err != nil {
			log.Printf("[signaling] lazy-expire delete room %s: %v", id, err)
		}
		return nil
	}
	lastActive := rec.LastActive
	if lastActive.IsZero() {
		lastActive = rec.CreatedAt
	}
	r = &Room{
		ID:            rec.ID,
		PasswordHash:  rec.PasswordHash,
		MaxClients:    rec.MaxClients,
		HostTokenHash: rec.HostTokenHash,
		CreatedAt:     rec.CreatedAt,
		LastActive:    lastActive,
		HostJoined:    rec.HostJoined,
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
// Before evicting, the current room state (notably LastActive and HostJoined)
// is flushed to the store so the on-disk record reflects when the room last
// had activity and the lazy-expiry logic in Room() works correctly.
func (h *Hub) removeMemOnly(id string) {
	if r := h.peekRoom(id); r != nil {
		h.persistRoom(r)
	}
	h.mu.Lock()
	delete(h.rooms, id)
	h.mu.Unlock()
}

// persistRoom writes the room's durable state to the store (if any). It is
// safe to call concurrently with room operations; it snapshots the fields
// under the room lock and performs the I/O outside any room/hub lock.
func (h *Hub) persistRoom(r *Room) {
	if h.store == nil {
		return
	}
	r.mu.Lock()
	rec := &RoomRecord{
		ID:            r.ID,
		PasswordHash:  r.PasswordHash,
		MaxClients:    r.MaxClients,
		HostTokenHash: r.HostTokenHash,
		CreatedAt:     r.CreatedAt,
		LastActive:    r.LastActive,
		HostJoined:    r.HostJoined,
	}
	r.mu.Unlock()
	if err := h.store.SaveRoom(context.Background(), rec); err != nil {
		log.Printf("[signaling] persist room %s: %v", r.ID, err)
	}
}

// Room is a signaling room hosting up to MaxClients peers.
type Room struct {
	ID            string
	PasswordHash  string // hex(SHA-256(password)); empty = no password; plaintext never stored
	MaxClients    int
	HostTokenHash string // hex(SHA-256(host_token)); plaintext token is not stored
	CreatedAt     time.Time
	LastActive    time.Time
	HostJoined    bool // true once a host has ever successfully admitted

	mu      sync.Mutex
	hostID  string // peer id of the host ("" until host connects)
	clients map[string]*Client
}

// errRoomCollision is returned when unique room id generation fails.
var errRoomCollision = &errVal{"could not allocate unique room id"}

// errTooManyRooms is returned when the global room cap has been reached.
var errTooManyRooms = &errVal{"too many rooms"}

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
	// the host token and is exempt. The comparison is constant-time.
	if !isHost && r.PasswordHash != "" && !verifyPassword(password, r.PasswordHash) {
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
		r.HostJoined = true
	}
	// Admission counts as activity: refresh the idle timer.
	r.LastActive = time.Now().UTC()

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
		// The room just became idle (no host, possibly no guests); record the
		// moment so the idle-timeout janitor measures from here.
		if len(r.clients) == 0 {
			r.LastActive = time.Now().UTC()
		}
		return remaining, true
	}
	if len(r.clients) == 0 {
		// Last guest left and host already gone: room is completely idle.
		r.LastActive = time.Now().UTC()
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

// isExpiredLocked reports whether an in-memory room with no connected clients
// should be evicted. Must be called with r.mu held.
func (r *Room) isExpiredLocked(now time.Time, hostGrace, idleTimeout time.Duration) bool {
	if hostGrace > 0 && !r.HostJoined && now.Sub(r.CreatedAt) > hostGrace {
		return true
	}
	if idleTimeout > 0 && now.Sub(r.LastActive) > idleTimeout {
		return true
	}
	return false
}

// recordExpired reports whether a persisted room record should be evicted on
// lookup. It is the on-disk counterpart of Room.isExpiredLocked.
func (h *Hub) recordExpired(rec *RoomRecord, now time.Time) bool {
	if h.hostGrace == 0 && h.idleTimeout == 0 {
		return false // expiry disabled
	}
	if h.hostGrace > 0 && !rec.HostJoined && now.Sub(rec.CreatedAt) > h.hostGrace {
		return true
	}
	lastActive := rec.LastActive
	if lastActive.IsZero() {
		lastActive = rec.CreatedAt
	}
	if h.idleTimeout > 0 && now.Sub(lastActive) > h.idleTimeout {
		return true
	}
	return false
}

// evictExpired scans the in-memory rooms and removes any that have no
// connected clients and have exceeded their host-grace or idle timeout. Store
// records for evicted rooms are also deleted. Intended to be called
// periodically by the janitor goroutine.
func (h *Hub) evictExpired(now time.Time) {
	h.mu.RLock()
	rooms := make([]*Room, 0, len(h.rooms))
	for _, r := range h.rooms {
		rooms = append(rooms, r)
	}
	h.mu.RUnlock()

	for _, r := range rooms {
		if !h.tryEvict(r, now) {
			continue
		}
		if h.store != nil {
			if err := h.store.DeleteRoom(context.Background(), r.ID); err != nil {
				log.Printf("[signaling] janitor delete room %s: %v", r.ID, err)
			}
		}
		log.Printf("[signaling] janitor evicted expired room %s", r.ID)
	}
}

// tryEvict atomically checks and removes a single expired, empty room from the
// in-memory map. It holds the hub write lock and the room lock together so
// that no Admit can race in between the emptiness check and the deletion. Lock
// ordering is hub -> room, which is consistent with the rest of the codebase
// (no path takes the room lock and then the hub lock).
func (h *Hub) tryEvict(r *Room, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cur := h.rooms[r.ID]; cur != r {
		return false // room was already replaced/removed
	}
	r.mu.Lock()
	expired := len(r.clients) == 0 && r.isExpiredLocked(now, h.hostGrace, h.idleTimeout)
	if expired {
		delete(h.rooms, r.ID)
	}
	r.mu.Unlock()
	return expired
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

// sha256hex returns the lowercase hex SHA-256 digest of s.
func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// hashToken returns the lowercase hex SHA-256 digest of a host token. This is
// what the room stores; the plaintext token is never retained.
func hashToken(token string) string {
	return sha256hex(token)
}

// hashPassword returns the lowercase hex SHA-256 digest of a room password.
// Only this digest is stored (in memory and on disk); the plaintext password
// is hashed at the API boundary and never retained.
func hashPassword(password string) string {
	return sha256hex(password)
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

// verifyPassword reports whether candidate hashes to the stored digest. The
// comparison is constant-time. An empty stored digest means the room has no
// password and any candidate is accepted (the caller is expected to guard the
// empty case, but this is safe to call regardless).
func verifyPassword(candidate, storedHash string) bool {
	if storedHash == "" {
		return true
	}
	got := hashPassword(candidate)
	return subtle.ConstantTimeCompare([]byte(got), []byte(storedHash)) == 1
}
