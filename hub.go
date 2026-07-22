package main

import (
	"crypto/rand"
	"sync"
)

// Alphabet without ambiguous characters (no 0/O/1/I/L), matching the original
// TypeScript implementation.
const roomAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

const (
	roomIDLen    = 5
	peerIDLen    = 6
	defaultMax   = 2
	tokenRandLen = 24 // bytes -> 32 base64-ish chars
)

// Hub owns the set of active rooms. It is safe for concurrent use.
type Hub struct {
	mu    sync.RWMutex
	rooms map[string]*Room
}

func newHub() *Hub {
	return &Hub{rooms: make(map[string]*Room)}
}

// CreateRoom allocates a new room with a unique id and host token.
func (h *Hub) CreateRoom(password string, maxClients int) (*Room, error) {
	if maxClients < 2 {
		maxClients = defaultMax
	}
	for attempts := 0; attempts < 32; attempts++ {
		id, err := randID(roomIDLen)
		if err != nil {
			return nil, err
		}
		h.mu.Lock()
		if _, ok := h.rooms[id]; ok {
			h.mu.Unlock()
			continue
		}
		token, err := randToken()
		if err != nil {
			h.mu.Unlock()
			return nil, err
		}
		r := &Room{
			ID:         id,
			Password:   password,
			MaxClients: maxClients,
			HostToken:  token,
			clients:    make(map[string]*Client),
		}
		h.rooms[id] = r
		h.mu.Unlock()
		return r, nil
	}
	return nil, errRoomCollision
}

// Room returns the room with the given id, or nil if it does not exist.
func (h *Hub) Room(id string) *Room {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.rooms[id]
}

// remove deletes a room from the hub if it has no clients. Called with the
// room's own lock held is fine because we only touch the hub map here.
func (h *Hub) remove(id string) {
	h.mu.Lock()
	delete(h.rooms, id)
	h.mu.Unlock()
}

// Room is a signaling room hosting up to MaxClients peers.
type Room struct {
	ID         string
	Password   string
	MaxClients int
	HostToken  string

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

	isHost := hostToken != "" && hostToken == r.HostToken
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

// Remove drops a client from the room. If the host leaves, the room is marked
// for teardown and the remaining peer ids are returned so the caller can notify
// them. Returns whether the room should be destroyed (host left or empty).
func (r *Room) Remove(c *Client) (remaining []string, destroy bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.clients, c.ID)
	if c.ID == r.hostID {
		// Host leaving tears down the whole room.
		remaining = make([]string, 0, len(r.clients))
		for id := range r.clients {
			remaining = append(remaining, id)
		}
		return remaining, true
	}
	if len(r.clients) == 0 {
		return nil, true
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

// randToken returns a opaque host token.
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
