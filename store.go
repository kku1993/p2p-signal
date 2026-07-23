package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrRoomNotFound is returned by Store.Room when no room with the given id
// exists.
var ErrRoomNotFound = errors.New("room not found")

// RoomRecord is the durable representation of a room: enough information to
// re-admit a host after a server restart. It does not include transient state
// (connected clients, current host peer id) — only the facts that must survive
// a crash.
type RoomRecord struct {
	ID            string    `json:"id"`
	Password      string    `json:"password,omitempty"`
	MaxClients    int       `json:"max_clients"`
	HostTokenHash string    `json:"host_token_hash"`
	CreatedAt     time.Time `json:"created_at"`
}

// Store is a durable registry of room metadata. Implementations must be safe
// for concurrent use.
//
// The store holds only the information needed to re-create a room's admission
// policy after a process restart (id, password, max_clients, host-token hash).
// It does not hold live connection state; that remains in memory in the Hub.
//
// When a Store is attached to a Hub:
//
//   - CreateRoom persists the new room record before returning success, so a
//     crash after the host receives the room id + token does not lose the room.
//   - Room falls back to the store when the in-memory map misses, rehydrating a
//     Room with no connected clients so the host can reconnect with its token.
//   - remove (called on host departure or when the room empties) deletes the
//     record so intentional teardown is durable too.
type Store interface {
	// SaveRoom persists a room record, overwriting any existing record for the
	// same id.
	SaveRoom(ctx context.Context, r *RoomRecord) error
	// Room returns the record for the given id, or ErrRoomNotFound if no such
	// room exists.
	Room(ctx context.Context, id string) (*RoomRecord, error)
	// DeleteRoom removes the record for the given id. A missing record is not
	// an error.
	DeleteRoom(ctx context.Context, id string) error
	// Close releases any resources held by the store. Called once during
	// shutdown.
	Close() error
}

// fileStore implements Store using one JSON file per room in a base directory.
// Writes are atomic (temp file + rename) so a crash mid-write cannot corrupt
// an existing record.
type fileStore struct {
	dir string
}

// newFileStore creates a file-based store rooted at dir. The directory is
// created with 0700 permissions if it does not exist.
func newFileStore(dir string) (*fileStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("store dir is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create store dir %q: %w", dir, err)
	}
	return &fileStore{dir: dir}, nil
}

func (s *fileStore) roomPath(id string) (string, error) {
	if !safeFilename(id) {
		return "", fmt.Errorf("invalid room id for storage: %q", id)
	}
	return filepath.Join(s.dir, id+".json"), nil
}

func (s *fileStore) SaveRoom(_ context.Context, r *RoomRecord) error {
	if r == nil || r.ID == "" {
		return fmt.Errorf("empty room record")
	}
	path, err := s.roomPath(r.ID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal room %s: %w", r.ID, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write room %s: %w", r.ID, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit room %s: %w", r.ID, err)
	}
	return nil
}

func (s *fileStore) Room(_ context.Context, id string) (*RoomRecord, error) {
	path, err := s.roomPath(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrRoomNotFound
		}
		return nil, fmt.Errorf("read room %s: %w", id, err)
	}
	var r RoomRecord
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("unmarshal room %s: %w", id, err)
	}
	return &r, nil
}

func (s *fileStore) DeleteRoom(_ context.Context, id string) error {
	path, err := s.roomPath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("delete room %s: %w", id, err)
	}
	return nil
}

func (s *fileStore) Close() error { return nil }

// safeFilename reports whether id is safe to use as a filename component: it
// must be non-empty, not "." or "..", and contain no path separators.
func safeFilename(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	return !strings.ContainsAny(id, `/\`)
}
