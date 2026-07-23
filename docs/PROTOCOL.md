# p2p-signal — Signaling Server Communication Protocol

This document specifies the HTTP and WebSocket protocol implemented by the
`p2p-signal` signaling server. It is intended as a stable contract for client
implementations (browsers, native apps, and other agents) that want to
establish peer-to-peer WebRTC connections through this server.

The server is a **relay-only** signaling server. It does not terminate media or
interpret SDP/ICE contents; it only routes signaling frames between peers that
share a room. After the handshake, peers are expected to establish a direct
WebRTC connection and the server relays any subsequent renegotiation
transparently.

## Table of contents

1. [Overview](#overview)
2. [Transport](#transport)
3. [Authentication model](#authentication-model)
4. [HTTP API](#http-api)
   - [POST /v1/rooms — create a room](#post-v1rooms--create-a-room)
   - [GET /v1/ws/&lt;room-id&gt; — open a WebSocket](#get-v1wsroom-id--open-a-websocket)
5. [WebSocket protocol](#websocket-protocol)
   - [Message framing](#message-framing)
   - [Client → Server](#client--server)
   - [Server → Client](#server--client)
   - [Error handling](#error-handling)
6. [End-to-end flows](#end-to-end-flows)
   - [Two-party call](#two-party-call)
   - [Multi-party call (mesh)](#multi-party-call-mesh)
   - [Password-protected room](#password-protected-room)
   - [Peer / host departure](#peer--host-departure)
7. [Close codes](#close-codes)
8. [Configuration & deployment](#configuration--deployment)
9. [Reference client pseudocode](#reference-client-pseudocode)

---

## Overview

A room is a short-lived signaling context identified by a random **room id**.
One participant is the **host** (the peer that created the room); all others are
**guests**. Each participant is assigned a unique **peer id** (scoped to the
room) when it connects. Signaling messages (SDP offers/answers and ICE
candidates) are addressed peer-to-peer using these ids, so the server supports
both 2-party calls and N-party **full-mesh** calls.

Key properties:

- Rooms are created explicitly via `POST /v1/rooms`, which returns a random
  room id and a one-time **host token**.
- A room may optionally be protected by a **password** that guests must supply.
- A room has a **max clients** limit (default `2`, configurable up to any
  `>= 2` value).
- The host must be the first peer to connect. When the host disconnects:
  - **Without persistence** (default): the room is destroyed and all remaining
    peers are notified with `peer-left`.
  - **With persistence**: the room stays alive. The host can reconnect with the
    same `host_token`. See [Peer / host departure](#peer--host-departure).
- By default the server keeps all state in memory for the lifetime of the room.
  When optional persistent storage is enabled (see
  [Configuration & deployment](#configuration--deployment)), room metadata
  (id, password, max_clients, host-token hash) is written to disk so a host can
  reconnect to the same room id with its original host token after a server
  crash, restart, or graceful disconnect.

## Transport

- HTTP/1.1 for room creation.
- WebSocket (RFC 6455) for signaling. Subprotocol negotiation is not used; all
  frames are **JSON text frames** (UTF-8).
- Default listen address is `:4000` (override with `--addr` or the
  `SIGNALING_ADDR` environment variable).
- The server sends WebSocket **ping** frames every ~54s and expects pong frames
  within 60s; clients that do not respond are dropped.

## Authentication model

There are two independent credentials:

| Credential   | Held by | Purpose                                          | Required?            |
|--------------|---------|--------------------------------------------------|----------------------|
| `host_token` | host    | Proves the caller created the room and is host.  | Yes, for the host.   |
| `password`   | guests  | Proves the guest was given the room password.    | Only if the room set one. |

The host is **exempt** from the password check; the host token alone
authenticates it. Guests never need the host token. The room id itself is also a
secret (5 characters from a 30-symbol alphabet ≈ 24 million possibilities), but
clients should treat `host_token` as the authoritative host credential and not
rely on room-id secrecy alone.

`host_token` is single-use in the sense that only one host connection may claim
it; once the host has joined, a second connection presenting the token is
rejected with `"host already connected"`.

## HTTP API

### `POST /v1/rooms` — create a room

Creates a new room and returns its id and host token. The host should then open
a WebSocket to `GET /v1/ws/<room-id>` with the host token.

**Request body** (JSON, all fields optional):

```jsonc
{
  "password": "optional-room-password", // string, default "" (no password)
  "max_clients": 2                       // int >= 2, default 2
}
```

An empty body `{}` or no body is valid and creates a default 2-person,
unprotected room. `max_clients` values `< 2` are coerced up to `2`.

**Response** — `201 Created`:

```json
{
  "room_id": "K7Q2P",
  "host_token": "9f3a1c...64 hex chars"
}
```

**Errors**:

| Status | Cause                                   |
|--------|-----------------------------------------|
| `400`  | Malformed JSON body.                    |
| `405`  | Non-POST method.                        |
| `500`  | Could not allocate a unique room id.    |

The `room_id` is 5 characters from the alphabet `ABCDEFGHJKMNPQRSTUVWXYZ23456789`
(no `0`/`O`/`1`/`I`/`L` to avoid ambiguity when read aloud). The `host_token` is
64 hex characters (32 random bytes, 256 bits of entropy).

The server does **not** store the plaintext `host_token`. It retains only the
hex-encoded SHA-256 digest of the token and verifies presented tokens with a
constant-time comparison, so a process memory dump cannot reveal usable tokens.
The plaintext token is returned exactly once, in this response, and must be
kept by the host.

### `GET /v1/ws/<room-id>` — open a WebSocket

Upgrades to a WebSocket and joins the room. Query parameters:

| Param      | Required                | Description                                  |
|------------|-------------------------|----------------------------------------------|
| `token`    | host only               | The `host_token` from room creation.         |
| `password` | guests, if room has one | The room password.                           |

Examples:

- Host:   `GET /v1/ws/K7Q2P?token=9f3a1c...`
- Guest:  `GET /v1/ws/K7Q2P`  or  `GET /v1/ws/K7Q2P?password=s3cret`

**Pre-upgrade HTTP errors** (the WebSocket is not opened):

| Status | Cause                                            |
|--------|--------------------------------------------------|
| `400`  | Missing room id in the path.                     |
| `404`  | Room does not exist (unknown or already torn down). |
| `405`  | Non-GET method.                                  |

**Post-upgrade WebSocket errors** (the connection is upgraded, then immediately
sent an `error` message and closed with code `1008`; see [Close codes](#close-codes)):

| `error.message`          | Cause                                                  |
|--------------------------|--------------------------------------------------------|
| `invalid host token`     | A `token` was supplied but did not match.              |
| `invalid password`       | Guest supplied the wrong (or no) password.            |
| `host already connected` | A second connection tried to claim the host token.     |
| `host has not joined yet`| A guest connected before the host.                     |
| `room is full`           | The room already has `max_clients` peers.              |

On success, the server immediately sends a `joined` message (see below) and
broadcasts `peer-joined` to all existing peers.

## WebSocket protocol

### Message framing

Every frame is a single JSON object with a `type` field. Unknown types produce
an `error` reply but do not close the connection. All messages are text frames;
binary frames are not used. The server enforces a 64 KiB max message size.

### Client → Server

These are the only message types a client should send.

#### `offer` — SDP offer to a specific peer

```json
{ "type": "offer", "to": "<peer-id>", "sdp": { /* RTCSessionDescriptionInit or string */ } }
```

#### `answer` — SDP answer to a specific peer

```json
{ "type": "answer", "to": "<peer-id>", "sdp": { /* RTCSessionDescriptionInit or string */ } }
```

#### `ice` — ICE candidate for a specific peer

```json
{ "type": "ice", "to": "<peer-id>", "candidate": { /* RTCIceCandidateInit or string */ } }
```

`offer`, `answer`, and `ice` are **relay-only**: the server does not inspect
`SDP` or `candidate` and forwards them verbatim. The `to` field is required; a
missing or unknown `to` produces an `error` and the message is not relayed.

#### `leave` — leave the room and close the connection

```json
{ "type": "leave" }
```

The server acknowledges by closing the WebSocket with code `1000` ("leave").
This is equivalent to simply closing the socket; the same teardown notifications
are sent to remaining peers.

### Server → Client

#### `joined` — sent once, right after a successful WebSocket upgrade

```json
{
  "type": "joined",
  "room": "K7Q2P",
  "peer_id": "V4PFKH",
  "peers": ["33DHAP"]
}
```

`peer_id` is this client's own id within the room. `peers` is the list of peer
ids **already present** in the room. It is always an array (never omitted or
`null`): it is `[]` for the host, since the host is first, and
`["<host-id>"]` for the first guest in a 2-party room. Clients may iterate over
it without a nil/undefined guard.

#### `peer-joined` — a new peer joined the room

```json
{ "type": "peer-joined", "peer_id": "V4PFKH" }
```

Sent to every peer that was already in the room. The newcomer does **not**
receive this for itself (it gets `joined` instead).

#### `peer-left` — a peer left the room

```json
{ "type": "peer-left", "peer_id": "33DHAP" }
```

Sent to all remaining peers when a peer disconnects (gracefully, via `leave`, or
abnormally). If the departing peer was the host, **all** remaining peers receive
`peer-left` for the host and the room is destroyed (see
[Host departure](#host-departure)).

#### `offer` / `answer` / `ice` — relayed signaling

```json
{ "type": "offer",  "from": "V4PFKH", "to": "33DHAP", "sdp": { /* ... */ } }
{ "type": "answer", "from": "33DHAP", "to": "V4PFKH", "sdp": { /* ... */ } }
{ "type": "ice",    "from": "V4PFKH", "to": "33DHAP", "candidate": { /* ... */ } }
```

These are the relayed counterparts of the client→server signaling messages. The
server adds `from` (the originator's peer id) so the recipient knows which
remote peer the signaling belongs to. The `sdp` / `candidate` payloads are
forwarded unchanged.

#### `error` — non-fatal error

```json
{ "type": "error", "message": "Peer not found: V4PFKH" }
```

The connection stays open. Fatal errors at join time (bad token/password/full
room) are delivered as `error` followed by a WebSocket close.

### Error handling

- Malformed JSON → `{"type":"error","message":"Invalid JSON"}`, connection
  stays open.
- Unknown `type` → `{"type":"error","message":"Unknown type: <type>"}`,
  connection stays open.
- Relay with missing/unknown `to` → `error`, connection stays open, message is
  not relayed.
- Join-time failures (auth, full room, host ordering) → `error` then close
  `1008`.

## End-to-end flows

### Two-party call

```
Host                                  Server                                 Guest
 |                                      |                                      |
 |  POST /v1/rooms  ----------->        |                                      |
 |  <-- 201 {room_id, host_token}      |                                      |
 |                                      |                                      |
 |  GET /v1/ws/<id>?token=<token>  ->   |                                      |
 |  <-- WS upgraded                    |                                      |
 |  <-- {type:"joined", peer_id:H, peers:[]}                                  |
 |                                      |                                      |
 |                                      | <----  GET /v1/ws/<id>  ------------- |
 |                                      | -----> WS upgraded                   |
 |                                      | ------ {type:"joined", peer_id:G,    |
 |                                      |         peers:[H]} ----------------> |
 |                                      | ------ {type:"peer-joined",          |
 |                                      |         peer_id:G} ----------------> |
 |                                      |                                      |
 |  {type:"offer", to:G, sdp:...} ->    |                                      |
 |                                      | ------ {type:"offer", from:H,        |
 |                                      |         to:G, sdp:...} -----------> |
 |                                      | <----  {type:"answer", to:H, ...} -- |
 |  <-- {type:"answer", from:G, ...}    |                                      |
 |                                      |                                      |
 |  {type:"ice", to:G, candidate:...}-> |                                      |
 |                                      | ------ {type:"ice", from:H, ...} --> |
 |                                      | <----  {type:"ice", to:H, ...} ----- |
 |  <-- {type:"ice", from:G, ...}       |                                      |
 |                                      |                                      |
 |          (direct WebRTC media/data flows between Host and Guest)             |
```

The host initiates negotiation because it learns about the guest via
`peer-joined`. The guest learns about the host via the `peers` array in its
`joined` message and waits for the offer.

### Multi-party call (mesh)

For a room created with `max_clients: N` (N > 2), every peer must establish a
direct WebRTC connection with every other peer (full mesh). The signaling
protocol is identical; the only difference is that each peer must:

1. On receiving `peer-joined` for a new peer `X`, the existing peer creates a
   new `RTCPeerConnection` for `X` and sends an `offer` addressed `to: X`.
2. On receiving `joined` with a non-empty `peers` list, the newcomer creates one
   `RTCPeerConnection` per listed peer and **waits** for an offer from each of
   them (the existing peers will initiate, per rule 1). Alternatively the
   newcomer may initiate to all listed peers; the only requirement is that for
   each pair exactly one side produces the offer. A simple deterministic rule:
   **the peer that was already in the room initiates the offer to the newcomer.**

ICE candidates are exchanged per-pair using the `to`/`from` addressing.

### Password-protected room

1. Host: `POST /v1/rooms {"password":"s3cret"}` → `{room_id, host_token}`.
2. Host: `GET /v1/ws/<id>?token=<host_token>` (no password needed).
3. Host shares `room_id` **and** `password` with the guest out of band.
4. Guest: `GET /v1/ws/<id>?password=s3cret`.
   - Wrong/missing password → `{"type":"error","message":"invalid password"}`
     then close `1008`.

### Peer / host departure

**Guest leaves.** Remaining peers (including the host) receive
`{"type":"peer-left","peer_id":"<departing-guest>"}`. The room stays alive.

**Host leaves (no persistence).** Every remaining peer receives
`{"type":"peer-left","peer_id":"<host-id>"}` and the room is destroyed. Any
further `GET /v1/ws/<id>` for that room id returns `404`. Clients should treat
host departure as termination of the session.

**Host leaves (persistence enabled).** Remaining peers receive
`{"type":"peer-left","peer_id":"<host-id>"}` but the room is **not** destroyed.
The room metadata stays in the store and the host can reconnect to the same
room id with its original `host_token`. If guests were still connected, the
room stays live in memory and the host can rejoin immediately; guests will
receive a `peer-joined` for the reconnected host. If no guests were connected,
the room is evicted from memory but rehydrated from the store on the next
`GET /v1/ws/<id>`. The room is only permanently destroyed when all peers
(including the host) have left and the room is empty.

## Close codes

| Code  | Meaning                                                                 |
|-------|-------------------------------------------------------------------------|
| `1000`| Normal closure, sent in response to a client `leave` message.           |
| `1008`| Policy violation — join-time rejection (auth/full-room/host-ordering). An `error` message precedes the close. |
| `1006`| Abnormal closure (client dropped without close frame). Treated as leave.|

On any close, the server removes the peer from the room and notifies survivors
with `peer-left`.

## Configuration & deployment

| Setting        | Flag          | Env var                | Default             |
|----------------|---------------|------------------------|---------------------|
| Listen address | `--addr`      | `SIGNALING_ADDR`       | `:4000`             |
| Store directory| `--store-dir` | `SIGNALING_STORE_DIR`  | `""` (in-memory)    |

Build and run:

```bash
go build -o p2p-signal .
./p2p-signal --addr :4000
# or
SIGNALING_ADDR=:4000 ./p2p-signal
```

### Persistent storage

By default the server is stateless: all room state lives in memory and is lost
on crash or restart. To enable crash recovery, point the server at a writable
directory:

```bash
./p2p-signal --store-dir /var/lib/p2p-signal
# or
SIGNALING_STORE_DIR=/var/lib/p2p-signal ./p2p-signal
```

When a store directory is configured:

- Each room is persisted as a JSON file (`<id>.json`) in the directory.
- After a crash or restart, a host can reconnect to the same room id with its
  original `host_token`. The server rehydrates the room from disk on first
  access.
- Guests can then join as usual once the host has reconnected.
- If the host disconnects gracefully (without a crash), the room is **not**
  destroyed. The host can reconnect with the same `host_token`, and any guests
  still connected remain in the room. The room is only permanently destroyed
  when all peers (host + guests) have left and the room is empty.
- Without persistence, host departure destroys the room immediately (the
  original behavior).
- Existing WebRTC connections between peers are unaffected by a signaling
  server restart; persistence only matters for re-admission and new joins.

The store is implemented behind a generic `Store` interface
(`SaveRoom` / `Room` / `DeleteRoom` / `Close`), so alternative backends (e.g.
SQL, Redis, S3) can be added without changing the Hub or HTTP layer.

The server logs to stdout: a `listening on :PORT` line at startup, `read error`
lines on abnormal disconnects, and `dropping message` lines if a slow peer's
send buffer fills (the message is dropped, not the connection).

### Limits & defaults

| Limit                       | Value      |
|-----------------------------|------------|
| Max WebSocket message size  | 64 KiB     |
| Read deadline (pong timeout)| 60 s       |
| Ping interval               | 54 s       |
| Write deadline per frame    | 10 s       |
| Per-peer send buffer        | 64 messages |
| Room id length              | 5 chars    |
| Peer id length              | 6 chars    |
| Host token length           | 64 hex chars (256-bit); stored as SHA-256 digest |

## Reference client pseudocode

```js
// 1. Host creates a room.
const { room_id, host_token } = await fetch("/v1/rooms", {
  method: "POST",
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify({ password: opts.password, max_clients: opts.maxClients }),
}).then(r => r.json());

// 2. Host opens its WebSocket.
const ws = new WebSocket(`ws://srv/v1/ws/${room_id}?token=${host_token}`);
ws.onmessage = (e) => {
  const m = JSON.parse(e.data);
  switch (m.type) {
    case "joined":
      myPeerId = m.peer_id; // m.peers is [] for the host
      break;
    case "peer-joined":
      // Create a new RTCPeerConnection for m.peer_id and send it an offer.
      const pc = new RTCPeerConnection(cfg);
      pcs[m.peer_id] = pc;
      pc.onicecandidate = (e) =>
        ws.send(JSON.stringify({ type: "ice", to: m.peer_id, candidate: e.candidate }));
      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);
      ws.send(JSON.stringify({ type: "offer", to: m.peer_id, sdp: offer }));
      break;
    case "offer":
      // m.from is the peer that wants to connect.
      const pc = ensurePC(m.from);
      await pc.setRemoteDescription(m.sdp);
      const answer = await pc.createAnswer();
      await pc.setLocalDescription(answer);
      ws.send(JSON.stringify({ type: "answer", to: m.from, sdp: answer }));
      break;
    case "answer":
      await pcs[m.from].setRemoteDescription(m.sdp);
      break;
    case "ice":
      pcs[m.from].addIceCandidate(m.candidate);
      break;
    case "peer-left":
      pcs[m.peer_id]?.close();
      delete pcs[m.peer_id];
      break;
    case "error":
      console.error("signaling error:", m.message);
      break;
  }
};

// 3. Guest opens its WebSocket (with password if the room has one).
//    const ws = new WebSocket(`ws://srv/v1/ws/${room_id}?password=${password}`);
//    The guest's "joined" message contains peers:[hostId]; it waits for the
//    host's "offer" rather than initiating.
```
