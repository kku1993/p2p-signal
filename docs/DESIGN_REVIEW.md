# Design & Security Review — p2p-signal

Review date: 2026-07-23
Scope: `main.go`, `hub.go`, `client.go`, `store.go`, `protocol.go`, `Dockerfile`, and the
deployment guidance in `README.md` / `AGENTS.md`.

This is a review of the **current** implementation. Findings are grouped by category and
tagged with a rough severity. Each item states the problem, why it matters, and a concrete
recommendation. A prioritized summary is at the end.

---

## 1. Security

### 1.1 Secrets travel in the URL query string — **High**
`GET /v1/ws/<room-id>?token=<host_token>&password=<password>` carries both the host token
and the room password as query parameters (`main.go:131-133`).

Query strings are the *least* safe place for secrets:
- They land in proxy/load-balancer access logs, CDN logs, and the server's own request
  logging if ever enabled.
- They are retained in browser history and can leak via the `Referer` header on any
  subresource the page loads.
- They show up in APM/observability traces by default.

The host token is 256-bit and only the SHA-256 is stored server-side (good), but if the
plaintext leaks from a log it is a full room-host takeover until the room dies.

**Recommendation:** Keep accepting the credential over the WebSocket, but move it out of the
URL. The browser `WebSocket` constructor can't set headers, so the standard pattern is:
- Send the token/password in the **first WebSocket message** (an explicit `auth`/`hello`
  frame) rather than admitting on the upgrade URL; *or*
- Use the `Sec-WebSocket-Protocol` header to smuggle the token (supported by the browser
  `WebSocket` 2nd argument), then echo it back on accept; *or*
- At minimum, make sure no layer logs the query string, and document it.

### 1.2 Room-password comparison is not constant-time — **Medium**
`hub.go:197`: `r.Password != "" && r.Password != password`. Go's `!=` on strings
short-circuits at the first differing byte, so response timing leaks the length of the
matching prefix. The host token already uses `subtle.ConstantTimeCompare` (`hub.go:334-340`);
the password path should too.

**Recommendation:** compare with `subtle.ConstantTimeCompare([]byte(password), []byte(r.Password)) == 1`
(guarding the empty-password case as today). Better still, store a hash of the password
instead of the plaintext (see 1.3).

### 1.3 Room password is stored in plaintext (memory **and** disk) — **Medium**
`Room.Password` is the plaintext value (`hub.go:167`), and with `--store-dir` it is written
verbatim into the per-room JSON (`store.go:22-27`, `RoomRecord.Password`). Files are `0600`
under a `0700` dir (good), but anyone who reads a backup, a snapshot, or the volume sees
every active room password. The host token, by contrast, is only persisted as a digest.

**Recommendation:** store only a hash of the password (same SHA-256 or, if you want
resistance to offline guessing of weak passwords, a slow KDF) and verify against the digest.
This also fixes 1.2 for free.

### 1.4 `CheckOrigin` accepts every origin — **Low (context-dependent)**
`main.go:23` returns `true` for all origins. The inline comment justifies this because
signaling is token/password gated and there are no ambient (cookie) credentials, which is a
reasonable position: classic Cross-Site WebSocket Hijacking needs an ambient credential the
browser attaches automatically, and here the attacker would still need the room id + token,
which are not ambient.

The residual risk: any web page can open connections to your server and enumerate/probe
rooms (see 1.5, 2.1). If you ever add cookie/session auth, this becomes a real CSWSH hole.

**Recommendation:** allow-list known front-end origins via config (with a wildcard escape
hatch for native clients), and re-audit if cookie auth is ever introduced.

### 1.5 Room ids are short and enumerable; existence is an oracle — **Medium**
Room ids are 5 characters over a 30-symbol alphabet ≈ `30^5 ≈ 24.3M` values (`hub.go:17-20`).
`GET /v1/ws/<id>` returns `404` for a missing room and proceeds to a WebSocket upgrade for an
existing one (`main.go:124-128`), so an attacker can enumerate valid room ids by response
code. Rooms created **without a password** can then be joined outright once the host is
present. There is no rate limiting to slow this down (see 2.1).

**Recommendation:** (a) lengthen room ids or decouple the *join secret* from the *routing
id* — e.g. a short human-shareable id plus a longer unguessable join key; (b) rate-limit and
add small random delay to lookups; (c) consider returning the same response whether or not a
passworded room exists until the password is presented, to reduce the enumeration oracle.

### 1.6 WebSocket upgrade happens *before* authentication — **Low/Medium**
In `handleWS`, `upgrader.Upgrade` runs before `room.Admit` checks the token/password
(`main.go:135-146`). Every join attempt — including every brute-force password guess — forces
a full WS handshake and a goroutine allocation before it can be rejected. This makes password
brute-forcing and connection-flooding cheaper for the attacker and more expensive for the
server than an HTTP-level rejection would be.

**Recommendation:** where possible validate cheaply before upgrading. Existence check already
precedes the upgrade; if you keep credentials on the URL, you can also reject obviously bad
tokens/passwords pre-upgrade. Pair with per-IP rate limiting (2.1).

### 1.7 No transport security in the box — **Informational**
The server speaks plain HTTP/WS. That's fine if it's always behind a TLS-terminating proxy,
but nothing enforces or documents "must run behind TLS." Signaling metadata (room ids,
passwords in the URL, peer topology) is sensitive.

**Recommendation:** document a hard requirement to terminate TLS in front, or add optional
`--tls-cert`/`--tls-key` flags.

---

## 2. Denial-of-Service vectors

### 2.1 Unauthenticated, unlimited room creation — **High**
`POST /v1/rooms` has no authentication, no rate limit, and no global cap on room count
(`main.go:85-108`, `hub.CreateRoom`). Each call allocates an in-memory `Room` and, with
persistence, writes a file to disk. An attacker can:
- Exhaust process memory by creating rooms in a loop.
- Exhaust disk/inodes on the persistence volume (files are never cleaned unless a host
  connects and later leaves — see 2.2).

**Recommendation:** add per-IP rate limiting (token bucket) on room creation, a global cap on
total live rooms, and ideally an auth requirement (API key / signed request) for creating
rooms in production.

### 2.2 Rooms with no connected host never expire — **High**
A room created via `POST /v1/rooms` exists indefinitely until a client connects **and later
disconnects**; the teardown-driven cleanup in `client.go:175-209` is the only path that
removes a room. A room whose host never opens a WebSocket is a permanent leak — in memory
without persistence, and in memory *and on disk* with `--store-dir`. Combined with 2.1 this is
a trivial resource-exhaustion primitive; even in normal use, abandoned "created but never
joined" rooms accumulate forever.

**Recommendation:** give every room a TTL / idle timeout:
- Expire rooms that have had no host within N minutes of creation.
- Expire rooms idle (no connected peers) beyond some window, even with persistence.
- Run a background janitor (or lazy expiry on lookup) that evicts from memory and store, and
  persist `CreatedAt`/`LastActive` (the record already has `CreatedAt`, `store.go:27`).

### 2.3 `max_clients` has no upper bound — **Medium**
`CreateRoom` floors `max_clients` at 2 but never caps it (`hub.go:50-53`). A room created with
`max_clients: 1_000_000` lets a single tenant hold a very large peer set on one instance; each
`Broadcast` and admit is O(n) over the room (`hub.go:281-293`). Actual peers are still bounded
by real WS connections, but the missing cap removes a natural safety valve and enables
lopsided resource use on the (single) instance that owns the room.

**Recommendation:** enforce a configurable maximum (e.g. `--max-clients-limit`, default 8–16
for mesh signaling, which is where full-mesh WebRTC stops scaling anyway).

### 2.4 No per-connection message rate limiting — **Medium**
Once admitted, a peer can send `offer`/`answer`/`ice` frames as fast as it likes; `relay`
forwards each to the target with no throttle (`client.go:113-136`). The only backpressure is
the 64-slot per-peer send buffer, and when a target's buffer is full the message is *dropped
silently* while the connection stays open (`client.go:54-58`). A malicious peer can spam a
victim peer to force message loss (degrading their real signaling) and burn CPU on JSON
marshalling.

**Recommendation:** add a per-connection rate limit (messages/sec and/or bytes/sec). Consider
closing (not just dropping for) connections whose send buffer stays saturated, since that
usually indicates a dead or malicious reader.

### 2.5 Request body size is unbounded — **Low**
`json.NewDecoder(r.Body).Decode` on the create-room handler reads the whole body with no
`http.MaxBytesReader` (`main.go:91-98`). A large `password` field (or just a huge body) is
read into memory. `WriteBufferSize`/`ReadBufferSize` bound WS frames, and `maxMessageSize`
caps WS messages, but the HTTP POST body is uncapped.

**Recommendation:** wrap the body with `http.MaxBytesReader` (e.g. 4–16 KiB) and cap password
length explicitly.

### 2.6 No global connection / per-IP limits, no idle-connection accounting — **Low**
Nothing caps total concurrent WebSocket connections or connections per source IP. `pongWait`
(60s) reaps dead connections eventually, but a slow attacker can still hold many upgraded
connections cheaply.

**Recommendation:** cap concurrent connections (global and per-IP) at the app or proxy layer.

---

## 3. Suitability for production / horizontal scaling

### 3.1 State is in-process; the only Store is node-local — **High (for HA)**
The README correctly explains that rooms live in one process and require session affinity by
room id. But the single provided `Store` is a **local** file store (`store.go:60-142`). With
consistent-hash affinity + local disk:
- A room's record exists only on the node that created it.
- If that node dies (or the hash ring changes on scale-up/down), the room is unrecoverable —
  persistence buys crash-recovery of *the same node*, not survival of node loss or
  rebalancing.
- Two peers of the same room that land on different nodes (any affinity miss — ring change,
  cookie loss, direct-to-pod traffic) **cannot signal each other at all**, because relay only
  reaches peers in the same process (`client.go:119`, `room.Client`).

So the design is really *single-node with crash recovery*, not horizontally scalable, despite
the README framing.

**Recommendations (pick per goals):**
- **Shared store:** implement `Store` against Redis / a database so any node can rehydrate any
  room's metadata. The `Store` interface is already clean enough for this (`store.go:45-58`).
- **Cross-node relay:** to truly remove affinity, add a message bus (Redis pub/sub, NATS) so a
  peer on node A can deliver a signaling frame to a peer on node B. Rooms become logical, not
  pinned. This is a larger change but is what "horizontally scaled" usually means.
- If you intend to stay affinity-based, say so explicitly and treat persistence as
  same-node-restart recovery only — and make room ids the affinity key end-to-end (they
  already are, via URL path, which is good for LB routing).

### 3.2 Shutdown is abrupt — bad for rolling deploys — **Medium**
`main.go:81` calls `srv.Close()` on SIGINT/SIGTERM, which immediately drops all active
connections rather than draining. `srv.Shutdown(ctx)` would stop accepting new connections and
let in-flight ones finish. For a WebSocket server this also matters because there's no code
that proactively sends a close frame to peers on shutdown, so clients see an abrupt reset
instead of a clean `peer-left`/close.

**Recommendation:** use `srv.Shutdown(ctx)` with a timeout; optionally broadcast a
close/`server-shutdown` message to connected peers first so clients can reconnect gracefully
(and, with persistence, the host can re-home).

### 3.3 No health / readiness endpoint — **Medium**
There's no `/healthz` or `/readyz`. Load balancers and orchestrators (k8s) need a liveness and
readiness probe; without one they fall back to TCP checks that can't tell "process up but
wedged" from "healthy," and can route to a pod that isn't ready.

**Recommendation:** add lightweight `/healthz` (process alive) and `/readyz` (store reachable,
not shutting down) endpoints.

### 3.4 No observability — **Medium**
There are `log.Printf` lines but no metrics (room count, connected peers, messages relayed,
rejects, dropped-buffer events) and no structured logging. At scale you can't see capacity,
abuse, or the drop-on-full-buffer condition (`client.go:57`) without metrics.

**Recommendation:** expose Prometheus metrics (or OpenMetrics) for rooms, peers, admits,
rejects (by reason), relayed messages, and dropped messages; switch to structured logging with
a request/room id field.

### 3.5 No room-lifecycle admin surface — **Low**
There's no way to list, inspect, or force-close a room (e.g. to evict an abusive room or
reclaim resources). Operationally you're blind and can only restart the process.

**Recommendation:** add an authenticated admin endpoint (list rooms, delete room) or at least
a metric + the janitor from 2.2.

---

## 4. Correctness / robustness (smaller items)

- **Silent message drops are invisible** (`client.go:54-58`): dropping on a full send buffer is
  the right non-blocking choice, but it's logged at most and never surfaced to the sender or
  metrics. A peer whose messages are being dropped has no way to know signaling failed. Consider
  a metric and/or a `backpressure` notice to the sender.
- **`ReadHeaderTimeout` only:** the server sets `ReadHeaderTimeout` (`main.go:67`) but no
  `WriteTimeout`/`IdleTimeout` on the HTTP server. WS connections manage their own deadlines, so
  this is mostly fine, but the plain HTTP `POST /v1/rooms` path has no write timeout.
- **`CreatedAt` is persisted but unused:** the record carries `CreatedAt` (`store.go:27`) yet
  nothing enforces expiry off it — wire it into the TTL work in 2.2.
- **`from` cannot be spoofed (good):** relay sets `From: c.ID` server-side (`client.go:126`), so
  peers can't impersonate each other. Worth keeping as an explicit invariant/test.

---

## 5. Prioritized summary

| # | Finding | Category | Severity | Effort |
|---|---------|----------|----------|--------|
| 2.1 | Unauthenticated, unlimited room creation | DoS | High | Low–Med |
| 2.2 | Rooms with no host never expire (mem + disk leak) | DoS | High | Med |
| 1.1 | Secrets (token/password) in URL query string | Security | High | Med |
| 3.1 | Node-local store + no cross-node relay ≠ horizontally scalable | Scaling | High (HA) | High |
| 1.3 | Room password stored in plaintext (mem + disk) | Security | Medium | Low |
| 1.2 | Password compare not constant-time | Security | Medium | Low |
| 1.5 | Short, enumerable room ids + existence oracle | Security | Medium | Med |
| 2.3 | `max_clients` has no upper bound | DoS | Medium | Low |
| 2.4 | No per-connection message rate limiting | DoS | Medium | Med |
| 3.2 | Abrupt shutdown (`Close` not `Shutdown`) | Scaling | Medium | Low |
| 3.3 | No health/readiness endpoint | Scaling | Medium | Low |
| 3.4 | No metrics/observability | Scaling | Medium | Med |
| 1.6 | Upgrade before auth (cheap brute force) | Security | Low–Med | Low |
| 2.5 | Unbounded POST body | DoS | Low | Low |
| 2.6 | No global/per-IP connection limits | DoS | Low | Med |
| 1.4 | `CheckOrigin` allows all origins | Security | Low | Low |

### Suggested first pass (highest value, lowest effort)
1. Rate-limit + cap room creation, cap `max_clients`, bound the POST body (2.1, 2.3, 2.5).
2. Add a room TTL / idle janitor keyed off `CreatedAt`/last-active (2.2).
3. Hash the room password and compare in constant time (1.3, 1.2).
4. Switch to `srv.Shutdown(ctx)` and add `/healthz` + `/readyz` (3.2, 3.3).
5. Move credentials off the URL, or guarantee no layer logs the query string (1.1).

The larger, roadmap-level item is 3.1: decide whether this service stays single-node
(affinity + same-node crash recovery, which the code already supports well) or becomes truly
horizontal, which needs a shared `Store` plus a cross-node message bus for relay.
