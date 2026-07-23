package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- Helpers ---------------------------------------------------------------

func newTestServer(t *testing.T, hub *Hub) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/rooms", handleCreateRoom(hub))
	mux.HandleFunc("/v1/ws/", handleWS(hub))
	mux.HandleFunc("/healthz", handleHealthz)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func postRooms(t *testing.T, srvURL string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, srvURL+"/v1/rooms", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post rooms: %v", err)
	}
	return resp
}

// --- Item 1: rate limit, room cap, max_clients cap, body bounding ----------

// TestRoomCreateRateLimit: a per-IP rate limiter rejects room creation beyond
// the burst, then recovers as the bucket refills.
func TestRoomCreateRateLimit(t *testing.T) {
	hub := newHub()
	hub.createLimiter = newRateLimiter(0, 2) // 0 refill: pure burst of 2
	srv := newTestServer(t, hub)

	for i := 0; i < 2; i++ {
		resp := postRooms(t, srv.URL, CreateRoomRequest{})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("room %d: expected 201, got %d", i, resp.StatusCode)
		}
		resp.Body.Close()
	}
	// Third is over the burst -> 429.
	resp := postRooms(t, srv.URL, CreateRoomRequest{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", resp.StatusCode)
	}
}

// TestMaxRoomsCap: CreateRoom refuses once the global cap is reached.
func TestMaxRoomsCap(t *testing.T) {
	hub := newHub()
	hub.maxRooms = 2
	for i := 0; i < 2; i++ {
		if _, _, err := hub.CreateRoom("", 2); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	_, _, err := hub.CreateRoom("", 2)
	if err != errTooManyRooms {
		t.Fatalf("expected errTooManyRooms, got %v", err)
	}
}

// TestMaxClientsLimitClamp: a requested max_clients above the limit is clamped
// down to the limit.
func TestMaxClientsLimitClamp(t *testing.T) {
	hub := newHub()
	hub.maxClientsLimit = 4
	r, _, err := hub.CreateRoom("", 1_000_000)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if r.MaxClients != 4 {
		t.Fatalf("expected max_clients clamped to 4, got %d", r.MaxClients)
	}
}

// TestMaxClientsLimitStillFloorsAtTwo: clamping does not push below the floor.
func TestMaxClientsLimitStillFloorsAtTwo(t *testing.T) {
	hub := newHub()
	hub.maxClientsLimit = 4
	r, _, err := hub.CreateRoom("", 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if r.MaxClients != 2 {
		t.Fatalf("expected floor of 2, got %d", r.MaxClients)
	}
}

// TestCreateBodySizeCap: an oversized POST body is rejected.
func TestCreateBodySizeCap(t *testing.T) {
	hub := newHub()
	srv := newTestServer(t, hub)

	// Build a body larger than maxCreateBodySize.
	big := strings.Repeat("x", maxCreateBodySize+1024)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/rooms",
		bytes.NewReader([]byte(`{"password":"`+big+`"}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for oversized body, got %d", resp.StatusCode)
	}
}

// TestPasswordLengthCap: a password longer than maxPasswordLen is rejected
// even when the overall body fits.
func TestPasswordLengthCap(t *testing.T) {
	hub := newHub()
	srv := newTestServer(t, hub)
	resp := postRooms(t, srv.URL, map[string]any{
		"password": strings.Repeat("x", maxPasswordLen+1),
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for long password, got %d", resp.StatusCode)
	}
}

// --- Item 3: password hashing ----------------------------------------------

// TestPasswordHashedNotPlaintext: CreateRoom stores a hash, not the plaintext.
func TestPasswordHashedNotPlaintext(t *testing.T) {
	hub := newHub()
	r, _, err := hub.CreateRoom("s3cret", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if r.PasswordHash == "" {
		t.Fatal("password hash should be set for a passworded room")
	}
	if r.PasswordHash == "s3cret" {
		t.Fatal("room stored plaintext password instead of a hash")
	}
	if r.PasswordHash != hashPassword("s3cret") {
		t.Fatalf("password hash mismatch: got %s", r.PasswordHash)
	}
}

// TestPasswordVerifyConstantTime: verifyPassword accepts the right password
// and rejects wrong ones; empty stored hash means "no password".
func TestPasswordVerifyConstantTime(t *testing.T) {
	h := hashPassword("hunter2")
	if !verifyPassword("hunter2", h) {
		t.Fatal("correct password rejected")
	}
	if verifyPassword("hunter3", h) {
		t.Fatal("wrong password accepted")
	}
	if !verifyPassword("anything", "") {
		t.Fatal("empty stored hash should accept any candidate")
	}
}

// TestPasswordRoundtripViaStore: the hashed password survives persistence.
func TestPasswordRoundtripViaStore(t *testing.T) {
	dir := t.TempDir()
	fs, err := newFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	hub := newHubWithStore(fs)

	room, token, err := hub.CreateRoom("s3cret", 3)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Simulate host join + leave so the record is flushed with HostJoined=true.
	host := &Client{ID: "H1", IsHost: true, Room: room, hub: hub,
		sendCh: make(chan []byte, 1)}
	room.hostID = "H1"
	room.clients["H1"] = host
	room.HostJoined = true
	host.teardown()

	rec, err := fs.Room(context.Background(), room.ID)
	if err != nil {
		t.Fatalf("room record: %v", err)
	}
	if rec.PasswordHash != hashPassword("s3cret") {
		t.Fatalf("persisted password hash mismatch: %s", rec.PasswordHash)
	}
	if !rec.HostJoined {
		t.Fatal("HostJoined should be persisted as true")
	}
	// Original host token still verifies against the persisted hash.
	if !verifyHostToken(token, rec.HostTokenHash) {
		t.Fatal("host token should verify against persisted record")
	}
}

// --- Item 2: room TTL / idle janitor ---------------------------------------

// TestRoomExpiredNoHost: a room whose host never joined and whose CreatedAt is
// older than hostGrace is evicted by the janitor.
func TestRoomExpiredNoHost(t *testing.T) {
	hub := newHub()
	hub.hostGrace = 10 * time.Minute
	r, _, err := hub.CreateRoom("", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Backdate creation so it is past the grace window.
	r.mu.Lock()
	r.CreatedAt = time.Now().Add(-20 * time.Minute)
	r.LastActive = r.CreatedAt
	r.mu.Unlock()

	hub.evictExpired(time.Now())
	if hub.peekRoom(r.ID) != nil {
		t.Fatal("room should have been evicted by janitor (no host, past grace)")
	}
}

// TestRoomNotExpiredWithinGrace: a room within the grace window is kept.
func TestRoomNotExpiredWithinGrace(t *testing.T) {
	hub := newHub()
	hub.hostGrace = 10 * time.Minute
	r, _, err := hub.CreateRoom("", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	r.mu.Lock()
	r.CreatedAt = time.Now().Add(-5 * time.Minute)
	r.LastActive = r.CreatedAt
	r.mu.Unlock()

	hub.evictExpired(time.Now())
	if hub.peekRoom(r.ID) == nil {
		t.Fatal("room within grace should not be evicted")
	}
}

// TestRoomExpiredHostJoinedNotSubjectToGrace: once the host has joined, the
// host-grace rule no longer applies; only the idle timeout matters.
func TestRoomExpiredHostJoinedNotSubjectToGrace(t *testing.T) {
	hub := newHub()
	hub.hostGrace = 1 * time.Minute
	hub.idleTimeout = 1 * time.Hour
	r, _, err := hub.CreateRoom("", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	r.mu.Lock()
	r.CreatedAt = time.Now().Add(-2 * time.Hour)     // past grace
	r.LastActive = time.Now().Add(-30 * time.Minute) // within idle timeout
	r.HostJoined = true
	r.mu.Unlock()

	hub.evictExpired(time.Now())
	if hub.peekRoom(r.ID) == nil {
		t.Fatal("host-joined room within idle timeout should not be evicted")
	}
}

// TestRoomExpiredIdle: a room idle longer than idleTimeout is evicted.
func TestRoomExpiredIdle(t *testing.T) {
	hub := newHub()
	hub.idleTimeout = 1 * time.Hour
	r, _, err := hub.CreateRoom("", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	r.mu.Lock()
	r.HostJoined = true
	r.LastActive = time.Now().Add(-2 * time.Hour)
	r.mu.Unlock()

	hub.evictExpired(time.Now())
	if hub.peekRoom(r.ID) != nil {
		t.Fatal("idle room past idleTimeout should be evicted")
	}
}

// TestRoomNotExpiredWithClients: a room with connected clients is never
// evicted, regardless of age.
func TestRoomNotExpiredWithClients(t *testing.T) {
	hub := newHub()
	hub.hostGrace = 1 * time.Minute
	hub.idleTimeout = 1 * time.Minute
	r, _, err := hub.CreateRoom("", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	c := &Client{ID: "P1", Room: r, hub: hub, sendCh: make(chan []byte, 1)}
	r.mu.Lock()
	r.CreatedAt = time.Now().Add(-1 * time.Hour)
	r.LastActive = time.Now().Add(-1 * time.Hour)
	r.clients["P1"] = c
	r.mu.Unlock()

	hub.evictExpired(time.Now())
	if hub.peekRoom(r.ID) == nil {
		t.Fatal("room with a connected client should not be evicted")
	}
}

// TestJanitorEvictsFromStore: with persistence, an expired in-memory room is
// removed from both memory and the store.
func TestJanitorEvictsFromStore(t *testing.T) {
	dir := t.TempDir()
	fs, err := newFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	hub := newHubWithStore(fs)
	hub.hostGrace = 1 * time.Minute

	r, _, err := hub.CreateRoom("", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	r.mu.Lock()
	r.CreatedAt = time.Now().Add(-10 * time.Minute)
	r.LastActive = r.CreatedAt
	r.mu.Unlock()

	hub.evictExpired(time.Now())
	if hub.peekRoom(r.ID) != nil {
		t.Fatal("room should be evicted from memory")
	}
	_, err = fs.Room(context.Background(), r.ID)
	if !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("room should be evicted from store, got %v", err)
	}
}

// TestLazyExpiryOnLookup: a persisted-but-expired record is evicted on lookup
// and Room() returns nil.
func TestLazyExpiryOnLookup(t *testing.T) {
	dir := t.TempDir()
	fs, err := newFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	hub := newHubWithStore(fs)
	hub.hostGrace = 1 * time.Minute

	// Write an expired record directly to the store.
	rec := &RoomRecord{
		ID:            "EXPIRD",
		PasswordHash:  hashPassword("x"),
		MaxClients:    2,
		HostTokenHash: hashToken("tok"),
		CreatedAt:     time.Now().Add(-1 * time.Hour),
		LastActive:    time.Now().Add(-1 * time.Hour),
	}
	if err := fs.SaveRoom(context.Background(), rec); err != nil {
		t.Fatalf("save: %v", err)
	}

	if r := hub.Room("EXPIRD"); r != nil {
		t.Fatalf("expired room should not be rehydrated, got %+v", r)
	}
	_, err = fs.Room(context.Background(), "EXPIRD")
	if !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("expired record should be deleted on lookup, got %v", err)
	}
}

// TestRateLimiterSweepReapsStaleBuckets: stale buckets are dropped by sweep.
func TestRateLimiterSweepReapsStaleBuckets(t *testing.T) {
	rl := newRateLimiter(1, 1)
	rl.sweepAge = 50 * time.Millisecond
	if !rl.allow("1.2.3.4") {
		t.Fatal("first allow should succeed")
	}
	// Bucket now has 0 tokens and last=now. Wait past sweepAge.
	time.Sleep(60 * time.Millisecond)
	rl.sweep(time.Now())
	rl.mu.Lock()
	n := len(rl.buckets)
	rl.mu.Unlock()
	if n != 0 {
		t.Fatalf("expected stale bucket reaped, got %d buckets", n)
	}
}

// TestRateLimiterConcurrent: the limiter is safe under concurrent access
// (exercised with -race).
func TestRateLimiterConcurrent(t *testing.T) {
	rl := newRateLimiter(100, 100)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				rl.allow(k)
			}
		}(string(rune('A' + i)))
	}
	wg.Wait()
}

// --- Item 4: healthz / readyz ----------------------------------------------

func TestHealthz(t *testing.T) {
	hub := newHub()
	srv := newTestServer(t, hub)
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: expected 200, got %d", resp.StatusCode)
	}
}

// TestReadyzFlipsOnShutdown: readyz reports 503 once the ready flag is cleared.
func TestReadyzFlipsOnShutdown(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	hub := newHub()
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", handleReadyz(&ready, hub))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if resp, err := http.Get(srv.URL + "/readyz"); err != nil {
		t.Fatal(err)
	} else {
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("readyz before shutdown: expected 200, got %d", resp.StatusCode)
		}
		resp.Body.Close()
	}

	ready.Store(false)
	if resp, err := http.Get(srv.URL + "/readyz"); err != nil {
		t.Fatal(err)
	} else {
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("readyz after shutdown: expected 503, got %d", resp.StatusCode)
		}
		resp.Body.Close()
	}
}
