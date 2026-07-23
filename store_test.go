package main

import (
	"context"
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
func TestHostLeaveDeletesPersistedRoom(t *testing.T) {
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

	// Record exists on disk.
	if _, err := fs.Room(context.Background(), room.ID); err != nil {
		t.Fatalf("room not persisted: %v", err)
	}

	// Simulate host leave: remove the room.
	hub.remove(room.ID)

	// Record gone from store.
	_, err = fs.Room(context.Background(), room.ID)
	if !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("expected ErrRoomNotFound after host leave, got %v", err)
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
