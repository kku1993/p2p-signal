package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestFileStore(t *testing.T) *fileStore {
	t.Helper()
	dir := t.TempDir()
	fs, err := newFileStore(dir)
	if err != nil {
		t.Fatalf("newFileStore: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	return fs
}

func TestFileStoreSaveLoadDelete(t *testing.T) {
	fs := newTestFileStore(t)
	ctx := context.Background()

	rec := &RoomRecord{
		ID:            "K7Q2P",
		Password:      "s3cret",
		MaxClients:    3,
		HostTokenHash: hashToken("abc"),
		CreatedAt:     time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := fs.SaveRoom(ctx, rec); err != nil {
		t.Fatalf("SaveRoom: %v", err)
	}

	got, err := fs.Room(ctx, "K7Q2P")
	if err != nil {
		t.Fatalf("Room: %v", err)
	}
	if got.ID != rec.ID || got.Password != rec.Password ||
		got.MaxClients != rec.MaxClients || got.HostTokenHash != rec.HostTokenHash {
		t.Fatalf("roundtrip mismatch:\n got  %+v\n want %+v", got, rec)
	}
	if !got.CreatedAt.Equal(rec.CreatedAt) {
		t.Fatalf("CreatedAt: got %v, want %v", got.CreatedAt, rec.CreatedAt)
	}

	// Overwrite.
	rec.Password = "newpass"
	if err := fs.SaveRoom(ctx, rec); err != nil {
		t.Fatalf("SaveRoom overwrite: %v", err)
	}
	got, _ = fs.Room(ctx, "K7Q2P")
	if got.Password != "newpass" {
		t.Fatalf("overwrite failed: %v", got.Password)
	}

	// Delete.
	if err := fs.DeleteRoom(ctx, "K7Q2P"); err != nil {
		t.Fatalf("DeleteRoom: %v", err)
	}
	_, err = fs.Room(ctx, "K7Q2P")
	if !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("expected ErrRoomNotFound, got %v", err)
	}

	// Delete missing is not an error.
	if err := fs.DeleteRoom(ctx, "K7Q2P"); err != nil {
		t.Fatalf("DeleteRoom missing: %v", err)
	}
}

func TestFileStoreRoomNotFound(t *testing.T) {
	fs := newTestFileStore(t)
	_, err := fs.Room(context.Background(), "NOPEX")
	if !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("expected ErrRoomNotFound, got %v", err)
	}
}

func TestFileStoreRejectsBadID(t *testing.T) {
	fs := newTestFileStore(t)
	ctx := context.Background()

	for _, bad := range []string{"", ".", "..", "a/b", "a\\b"} {
		if err := fs.SaveRoom(ctx, &RoomRecord{ID: bad}); err == nil {
			t.Errorf("SaveRoom(%q) should fail", bad)
		}
		if _, err := fs.Room(ctx, bad); err == nil {
			t.Errorf("Room(%q) should fail", bad)
		}
		if err := fs.DeleteRoom(ctx, bad); err == nil {
			t.Errorf("DeleteRoom(%q) should fail", bad)
		}
	}
}

func TestFileStoreAtomicWrite(t *testing.T) {
	fs := newTestFileStore(t)
	dir := fs.dir

	// A successful SaveRoom must not leave a .tmp file behind.
	rec := &RoomRecord{ID: "ABC23", MaxClients: 2, HostTokenHash: hashToken("x")}
	if err := fs.SaveRoom(context.Background(), rec); err != nil {
		t.Fatalf("SaveRoom: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ABC23.tmp")); !os.IsNotExist(err) {
		t.Fatalf("leftover .tmp file: %v", err)
	}
	// The committed file must exist.
	if _, err := os.Stat(filepath.Join(dir, "ABC23.json")); err != nil {
		t.Fatalf("committed file missing: %v", err)
	}
}

// TestRoomPersistsAcrossRestart: a room created with a file store survives a
// simulated restart (new Hub backed by the same store directory), and the host
// can reconnect with its original token.
func TestRoomPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()

	// --- "first run" ---
	fs1, err := newFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	hub1 := newHubWithStore(fs1)
	room, token, err := hub1.CreateRoom("s3cret", 3)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	roomID := room.ID
	_ = fs1.Close()

	// --- "restart": new hub, same store dir ---
	fs2, err := newFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs2.Close()
	hub2 := newHubWithStore(fs2)

	// The room is not in memory yet.
	if r := hub2.peekRoom(roomID); r != nil {
		t.Fatal("room should not be in memory after restart")
	}
	// Room() rehydrates it from the store.
	r := hub2.Room(roomID)
	if r == nil {
		t.Fatal("room not rehydrated from store")
	}
	if r.Password != "s3cret" || r.MaxClients != 3 {
		t.Fatalf("rehydrated room mismatch: password=%q max=%d", r.Password, r.MaxClients)
	}
	// The original host token must still verify.
	if !verifyHostToken(token, r.HostTokenHash) {
		t.Fatal("original host token does not verify after restart")
	}
}

// TestHostLeaveDeletesPersistedRoom: when the host leaves, the room record is
// removed from the store so it cannot be rehydrated after a restart.
// TestHubRemoveDeletesPersistedRoom: hub.remove (full destroy) deletes from
// both memory and store. This is the "intentional permanent destroy" path.
func TestHubRemoveDeletesPersistedRoom(t *testing.T) {
	dir := t.TempDir()
	fs, err := newFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	hub := newHubWithStore(fs)

	room, _, err := hub.CreateRoom("", 2)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if _, err := fs.Room(context.Background(), room.ID); err != nil {
		t.Fatalf("room not persisted: %v", err)
	}

	hub.remove(room.ID)

	_, err = fs.Room(context.Background(), room.ID)
	if !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("expected ErrRoomNotFound after hub.remove, got %v", err)
	}
}

// TestHostLeaveWithPersistenceKeepsRoom: when the host disconnects and
// persistence is enabled, the room record stays in the store so the host can
// reconnect with the same token. The room is evicted from memory (no guests
// were connected).
func TestHostLeaveWithPersistenceKeepsRoom(t *testing.T) {
	dir := t.TempDir()
	fs, err := newFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	hub := newHubWithStore(fs)

	room, token, err := hub.CreateRoom("", 2)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	// Simulate host teardown (host leaves, no guests).
	host := &Client{ID: "HOST01", IsHost: true, Room: room, hub: hub,
		sendCh: make(chan []byte, 1)}
	room.hostID = "HOST01"
	room.clients["HOST01"] = host
	host.teardown()

	// Room evicted from memory.
	if r := hub.peekRoom(room.ID); r != nil {
		t.Fatal("room should be evicted from memory after host leave")
	}
	// Room record still in store.
	rec, err := fs.Room(context.Background(), room.ID)
	if err != nil {
		t.Fatalf("room record should survive host leave: %v", err)
	}
	// Host token still valid.
	if !verifyHostToken(token, rec.HostTokenHash) {
		t.Fatal("host token should still verify after host leave")
	}
}

// TestHostReconnectAfterLeave: after the host leaves (with persistence), the
// host can reconnect to the same room id with the same token, and the room is
// rehydrated from the store.
func TestHostReconnectAfterLeave(t *testing.T) {
	dir := t.TempDir()
	fs, err := newFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	hub := newHubWithStore(fs)

	room, token, err := hub.CreateRoom("s3cret", 3)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	// Host joins then leaves.
	host := &Client{ID: "HOST01", IsHost: true, Room: room, hub: hub,
		sendCh: make(chan []byte, 1)}
	room.hostID = "HOST01"
	room.clients["HOST01"] = host
	host.teardown()

	// Room is gone from memory but in store. Re-lookup rehydrates it.
	r := hub.Room(room.ID)
	if r == nil {
		t.Fatal("room should be rehydrated from store")
	}
	if r.Password != "s3cret" || r.MaxClients != 3 {
		t.Fatalf("rehydrated room mismatch: password=%q max=%d", r.Password, r.MaxClients)
	}
	if r.hostID != "" {
		t.Fatalf("hostID should be empty after rehydration, got %q", r.hostID)
	}

	// Host can re-admit with the original token.
	host2 := &Client{hub: hub, sendCh: make(chan []byte, 1)}
	peerID, _, err := r.Admit(host2, token, "")
	if err != nil {
		t.Fatalf("host re-admit failed: %v", err)
	}
	if peerID == "" {
		t.Fatal("host re-admit returned empty peer id")
	}
	if r.hostID != peerID {
		t.Fatalf("hostID should be %q after re-admit, got %q", peerID, r.hostID)
	}
}

// TestHostLeaveWithPersistenceGuestsStay: when the host disconnects with
// persistence enabled and guests are still connected, the room stays alive in
// memory, guests receive peer-left, and the host can reconnect.
func TestHostLeaveWithPersistenceGuestsStay(t *testing.T) {
	dir := t.TempDir()
	fs, err := newFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	hub := newHubWithStore(fs)

	room, token, err := hub.CreateRoom("", 3)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	// Set up host + guest in the room.
	host := &Client{ID: "HOST01", IsHost: true, Room: room, hub: hub,
		sendCh: make(chan []byte, 1)}
	guest := &Client{ID: "GUEST1", Room: room, hub: hub,
		sendCh: make(chan []byte, 1)}
	room.hostID = "HOST01"
	room.clients["HOST01"] = host
	room.clients["GUEST1"] = guest

	// Host leaves.
	host.teardown()

	// Guest should have received peer-left for the host.
	select {
	case raw := <-guest.sendCh:
		var msg ServerOut
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if msg.Type != "peer-left" || msg.PeerID != "HOST01" {
			t.Fatalf("expected peer-left HOST01, got %+v", msg)
		}
	default:
		t.Fatal("guest should have received peer-left")
	}

	// Room should still be in memory (guest is still connected).
	if r := hub.peekRoom(room.ID); r == nil {
		t.Fatal("room should stay in memory when guests are connected")
	}
	// hostID should be cleared.
	if room.hostID != "" {
		t.Fatalf("hostID should be cleared, got %q", room.hostID)
	}
	// Guest should still be in the room.
	if _, ok := room.clients["GUEST1"]; !ok {
		t.Fatal("guest should still be in room")
	}

	// Host reconnects with the same token.
	host2 := &Client{hub: hub, sendCh: make(chan []byte, 1)}
	peerID, existing, err := room.Admit(host2, token, "")
	if err != nil {
		t.Fatalf("host reconnect failed: %v", err)
	}
	if peerID == "" {
		t.Fatal("reconnect returned empty peer id")
	}
	// Host should see the guest in existing peers.
	if len(existing) != 1 || existing[0] != "GUEST1" {
		t.Fatalf("host should see [GUEST1] as existing peers, got %v", existing)
	}
}

// TestLastGuestLeavesAfterHostDestroysRoom: when the host has already left
// (with persistence) and the last guest leaves, the room is fully destroyed
// (memory + store).
func TestLastGuestLeavesAfterHostDestroysRoom(t *testing.T) {
	dir := t.TempDir()
	fs, err := newFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	hub := newHubWithStore(fs)

	room, _, err := hub.CreateRoom("", 3)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	// Host + guest in room.
	host := &Client{ID: "HOST01", IsHost: true, Room: room, hub: hub,
		sendCh: make(chan []byte, 1)}
	guest := &Client{ID: "GUEST1", Room: room, hub: hub,
		sendCh: make(chan []byte, 1)}
	room.hostID = "HOST01"
	room.clients["HOST01"] = host
	room.clients["GUEST1"] = guest

	// Host leaves (room stays alive because guest is connected).
	host.teardown()
	// Drain guest's peer-left notification.
	<-guest.sendCh

	// Last guest leaves.
	guest.teardown()

	// Room should be gone from memory.
	if r := hub.peekRoom(room.ID); r != nil {
		t.Fatal("room should be evicted from memory")
	}
	// Room should be gone from store.
	_, err = fs.Room(context.Background(), room.ID)
	if !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("expected ErrRoomNotFound after last guest left, got %v", err)
	}
}

// TestCreateRoomRollbackOnPersistFailure: if the store fails, the in-memory
// room is rolled back and CreateRoom returns an error.
func TestCreateRoomRollbackOnPersistFailure(t *testing.T) {
	store := &failingStore{}
	hub := newHubWithStore(store)

	_, _, err := hub.CreateRoom("", 2)
	if err == nil {
		t.Fatal("expected error from failing store")
	}
	// The in-memory map should be empty (rolled back).
	hub.mu.RLock()
	n := len(hub.rooms)
	hub.mu.RUnlock()
	if n != 0 {
		t.Fatalf("expected 0 rooms after rollback, got %d", n)
	}
}

// failingStore is a Store whose SaveRoom always fails.
type failingStore struct{}

func (failingStore) SaveRoom(context.Context, *RoomRecord) error { return errStoreFail }
func (failingStore) Room(context.Context, string) (*RoomRecord, error) {
	return nil, ErrRoomNotFound
}
func (failingStore) DeleteRoom(context.Context, string) error { return nil }
func (failingStore) Close() error                             { return nil }

var errStoreFail = errors.New("simulated store failure")

// peekRoom returns the in-memory room without consulting the store.
func (h *Hub) peekRoom(id string) *Room {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.rooms[id]
}
