# p2p-signal

A minimal WebSocket signaling server for establishing peer-to-peer WebRTC
connections. Translated from the original TypeScript implementation in
`index.ts` into Go, with the following additions:

- **HTTP room creation**: `POST /v1/rooms` returns a random room id (and a
  one-time host token) before any WebSocket is opened.
- **Optional room password**: guests must supply the password to join; the host
  is exempt (authenticated by the host token).
- **Configurable room size**: rooms default to 2 peers but can be created with
  any `max_clients >= 2` to support full-mesh multi-party calls.
- **N-peer mesh signaling**: each peer gets a unique peer id and addresses
  signaling (`offer`/`answer`/`ice`) to a specific peer via a `to` field.

## Layout

| File              | Purpose                                              |
|-------------------|------------------------------------------------------|
| `main.go`         | HTTP server, `POST /v1/rooms` and `GET /v1/ws/<id>` handlers. |
| `hub.go`          | `Hub`/`Room` types: room creation, admission, peer tracking, id generation. |
| `client.go`       | `Client`: WebSocket read/write pumps, relay, teardown. |
| `protocol.go`     | JSON message structs for both directions.            |
| `server_test.go`  | End-to-end tests (basic flow, password, max clients, host ordering, departure). |
| `PROTOCOL.md`     | Full communication protocol spec for clients/agents. |

## Build & run

```bash
go build -o p2p-signal .
./p2p-signal --addr :4000
# or
SIGNALING_ADDR=:4000 ./p2p-signal
```

## Docker

Build a small static image (multi-stage build → `scratch` runtime, ~9 MB):

```bash
docker build -t p2p-signal .
```

Run (exposes port 4000 by default):

```bash
docker run --rm -p 4000:4000 p2p-signal
# or override the listen address:
docker run --rm -p 8080:8080 p2p-signal --addr :8080
# or via the SIGNALING_ADDR env var:
docker run --rm -p 8080:8080 -e SIGNALING_ADDR=:8080 p2p-signal
```

The binary is built with `CGO_ENABLED=0` and `-ldflags="-s -w"`, so the
runtime image contains no OS, shell, or glibc and runs as a non-root user
(UID 65532).

## Test

```bash
go test -race ./...
```

## Protocol

See [PROTOCOL.md](./PROTOCOL.md) for the complete, machine-implementation-ready
specification of the HTTP and WebSocket protocol.
