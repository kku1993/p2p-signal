# AGENTS.md

## Build
```bash
go build ./...
# binary with version stamped from VERSION file:
go build -ldflags "-X main.version=$(cat VERSION | tr -d '[:space:]')" -o p2p-signal .
```

## Test
```bash
go test -race ./...
```

## Run
```bash
./p2p-signal --addr :4000
# or: SIGNALING_ADDR=:4000 ./p2p-signal

# with persistent room storage (crash recovery):
./p2p-signal --addr :4000 --store-dir /var/lib/p2p-signal
# or: SIGNALING_STORE_DIR=/var/lib/p2p-signal ./p2p-signal
```

## Verify before committing
```bash
gofmt -l . && go vet ./... && go test -race ./...
```
`gofmt -l .` must print nothing (all files formatted).

## Architecture
- `main.go` — HTTP routes: `POST /v1/rooms` (create room), `GET /v1/ws/<room-id>` (WebSocket).
- `hub.go` — `Hub` (room registry) and `Room` (peer set, password, max_clients, host token). All concurrency-safe.
- `client.go` — per-connection read/write pumps; relay of offer/answer/ice by peer id; teardown with peer-left broadcast.
- `protocol.go` — JSON message structs.
- `store.go` — generic `Store` interface for durable room metadata, with a `fileStore` implementation (one JSON file per room). Pluggable: alternative backends just implement the interface.
- `docs/PROTOCOL.md` — the wire protocol contract; update it whenever the protocol changes.

## Key invariants
- Host must connect first. Host is authenticated by `host_token` (from `POST /v1/rooms`) and is exempt from the room password.
- Guests authenticate with the room `password` (only if the room set one).
- Host departure behavior depends on persistence:
  - Without persistence (default): room is destroyed, all peers get `peer-left`.
  - With persistence (`--store-dir`): room stays alive, host can reconnect with the same `host_token`. Room is only destroyed when all peers have left.
- Signaling messages are addressed peer-to-peer via `to` (client→server) and `from` (server→client); the server never inspects SDP/ICE contents.
