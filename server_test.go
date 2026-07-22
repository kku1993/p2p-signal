package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func dial(t *testing.T, srvURL, roomID, token, password string) *websocket.Conn {
	t.Helper()
	u, err := url.Parse(srvURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Scheme = "ws"
	u.Path = "/v1/ws/" + roomID
	q := u.Query()
	if token != "" {
		q.Set("token", token)
	}
	if password != "" {
		q.Set("password", password)
	}
	u.RawQuery = q.Encode()
	c, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}

func readMsg(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, raw, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal %q: %v", raw, err)
	}
	return m
}

func sendMsg(t *testing.T, c *websocket.Conn, m map[string]any) {
	t.Helper()
	data, _ := json.Marshal(m)
	if err := c.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func createRoom(t *testing.T, srvURL string, body any) CreateRoomResponse {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, srvURL+"/v1/rooms", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create room: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create room status: %d", resp.StatusCode)
	}
	var r CreateRoomResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	return r
}

// TestBasicFlow: host creates room via HTTP, host + guest connect, exchange
// offer/answer/ice, and peer-left is delivered on disconnect.
func TestBasicFlow(t *testing.T) {
	hub := newHub()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/rooms", handleCreateRoom(hub))
	mux.HandleFunc("/v1/ws/", handleWS(hub))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	room := createRoom(t, srv.URL, CreateRoomRequest{})

	host := dial(t, srv.URL, room.RoomID, room.HostToken, "")
	defer host.Close()
	guest := dial(t, srv.URL, room.RoomID, "", "")
	defer guest.Close()

	// Host receives "joined" with its peer id and empty peers.
	hJoined := readMsg(t, host)
	if hJoined["type"] != "joined" {
		t.Fatalf("host first msg: %v", hJoined)
	}
	hostID, _ := hJoined["peer_id"].(string)
	if hostID == "" {
		t.Fatal("host missing peer_id")
	}

	// Guest receives "joined" with peers=[hostID]; host gets "peer-joined".
	gJoined := readMsg(t, guest)
	if gJoined["type"] != "joined" {
		t.Fatalf("guest first msg: %v", gJoined)
	}
	guestID, _ := gJoined["peer_id"].(string)
	peers, _ := gJoined["peers"].([]any)
	if len(peers) != 1 || peers[0] != hostID {
		t.Fatalf("guest peers: %v", peers)
	}
	pj := readMsg(t, host)
	if pj["type"] != "peer-joined" || pj["peer_id"] != guestID {
		t.Fatalf("host peer-joined: %v", pj)
	}

	// Guest sends an offer to host; host should receive it relayed with from.
	sendMsg(t, guest, map[string]any{"type": "offer", "to": hostID, "sdp": "SDP-OFFER"})
	relay := readMsg(t, host)
	if relay["type"] != "offer" || relay["from"] != guestID || relay["sdp"] != "SDP-OFFER" {
		t.Fatalf("host relayed offer: %v", relay)
	}

	// Host answers.
	sendMsg(t, host, map[string]any{"type": "answer", "to": guestID, "sdp": "SDP-ANSWER"})
	relay = readMsg(t, guest)
	if relay["type"] != "answer" || relay["from"] != hostID || relay["sdp"] != "SDP-ANSWER" {
		t.Fatalf("guest relayed answer: %v", relay)
	}

	// ICE.
	sendMsg(t, guest, map[string]any{"type": "ice", "to": hostID, "candidate": "CAND"})
	relay = readMsg(t, host)
	if relay["type"] != "ice" || relay["from"] != guestID || relay["candidate"] != "CAND" {
		t.Fatalf("host relayed ice: %v", relay)
	}

	// Guest disconnects; host should get peer-left.
	guest.Close()
	pl := readMsg(t, host)
	if pl["type"] != "peer-left" || pl["peer_id"] != guestID {
		t.Fatalf("host peer-left: %v", pl)
	}
}

// TestPassword: a room with a password rejects guests without it.
func TestPassword(t *testing.T) {
	hub := newHub()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/rooms", handleCreateRoom(hub))
	mux.HandleFunc("/v1/ws/", handleWS(hub))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	room := createRoom(t, srv.URL, CreateRoomRequest{Password: "s3cret"})

	host := dial(t, srv.URL, room.RoomID, room.HostToken, "")
	defer host.Close()
	if m := readMsg(t, host); m["type"] != "joined" {
		t.Fatalf("host should join, got %v", m)
	}
	bad := dial(t, srv.URL, room.RoomID, "", "wrong")
	defer bad.Close()
	errMsg := readMsg(t, bad)
	if errMsg["type"] != "error" {
		t.Fatalf("expected error, got %v", errMsg)
	}
	// Correct password -> joined.
	good := dial(t, srv.URL, room.RoomID, "", "s3cret")
	defer good.Close()
	j := readMsg(t, good)
	if j["type"] != "joined" {
		t.Fatalf("expected joined, got %v", j)
	}
}

// TestMaxClients: a room with max_clients=3 admits two guests but rejects a
// third.
func TestMaxClients(t *testing.T) {
	hub := newHub()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/rooms", handleCreateRoom(hub))
	mux.HandleFunc("/v1/ws/", handleWS(hub))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	room := createRoom(t, srv.URL, CreateRoomRequest{MaxClients: 3})

	host := dial(t, srv.URL, room.RoomID, room.HostToken, "")
	defer host.Close()
	hostID := readMsg(t, host)["peer_id"].(string)

	g1 := dial(t, srv.URL, room.RoomID, "", "")
	defer g1.Close()
	g1ID := readMsg(t, g1)["peer_id"].(string)
	readMsg(t, host) // peer-joined g1

	g2 := dial(t, srv.URL, room.RoomID, "", "")
	defer g2.Close()
	g2ID := readMsg(t, g2)["peer_id"].(string)
	// host and g1 both get peer-joined for g2
	_ = readMsg(t, host)
	_ = readMsg(t, g1)

	// Third guest rejected.
	g3 := dial(t, srv.URL, room.RoomID, "", "")
	defer g3.Close()
	m := readMsg(t, g3)
	if m["type"] != "error" {
		t.Fatalf("expected error for full room, got %v", m)
	}

	// Relay works between host and g2 (non-host pair).
	sendMsg(t, g2, map[string]any{"type": "offer", "to": hostID, "sdp": "X"})
	r := readMsg(t, host)
	if r["from"] != g2ID || r["sdp"] != "X" {
		t.Fatalf("relay host<-g2: %v", r)
	}
	sendMsg(t, host, map[string]any{"type": "answer", "to": g2ID, "sdp": "Y"})
	r = readMsg(t, g2)
	if r["from"] != hostID || r["sdp"] != "Y" {
		t.Fatalf("relay g2<-host: %v", r)
	}
	// Relay between g1 and g2.
	sendMsg(t, g1, map[string]any{"type": "ice", "to": g2ID, "candidate": "Z"})
	r = readMsg(t, g2)
	if r["from"] != g1ID || r["candidate"] != "Z" {
		t.Fatalf("relay g2<-g1: %v", r)
	}
}

// TestHostTokenRequired: a guest cannot connect before the host.
func TestGuestBeforeHost(t *testing.T) {
	hub := newHub()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/rooms", handleCreateRoom(hub))
	mux.HandleFunc("/v1/ws/", handleWS(hub))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	room := createRoom(t, srv.URL, CreateRoomRequest{})

	g := dial(t, srv.URL, room.RoomID, "", "")
	defer g.Close()
	m := readMsg(t, g)
	if m["type"] != "error" || !strings.Contains(m["message"].(string), "host") {
		t.Fatalf("expected host-not-joined error, got %v", m)
	}
}

// TestBadHostToken: a wrong host token is rejected.
func TestBadHostToken(t *testing.T) {
	hub := newHub()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/rooms", handleCreateRoom(hub))
	mux.HandleFunc("/v1/ws/", handleWS(hub))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	room := createRoom(t, srv.URL, CreateRoomRequest{})

	h := dial(t, srv.URL, room.RoomID, "not-the-token", "")
	defer h.Close()
	m := readMsg(t, h)
	if m["type"] != "error" {
		t.Fatalf("expected error, got %v", m)
	}
}

// TestRoomIDFormat: room ids are 5 chars from the unambiguous alphabet.
func TestRoomIDFormat(t *testing.T) {
	hub := newHub()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/rooms", handleCreateRoom(hub))
	mux.HandleFunc("/v1/ws/", handleWS(hub))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	room := createRoom(t, srv.URL, CreateRoomRequest{})
	if len(room.RoomID) != roomIDLen {
		t.Fatalf("room id len: %d", len(room.RoomID))
	}
	for _, ch := range room.RoomID {
		if !strings.ContainsRune(roomAlphabet, ch) {
			t.Fatalf("room id %q contains invalid char %q", room.RoomID, ch)
		}
	}
	if len(room.HostToken) != tokenRandLen*2 {
		t.Fatalf("host token len: %d", len(room.HostToken))
	}
}

// TestHostLeaveDestroysRoom: when host leaves, guests are notified and the room
// is removed.
func TestHostLeaveDestroysRoom(t *testing.T) {
	hub := newHub()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/rooms", handleCreateRoom(hub))
	mux.HandleFunc("/v1/ws/", handleWS(hub))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	room := createRoom(t, srv.URL, CreateRoomRequest{MaxClients: 3})

	host := dial(t, srv.URL, room.RoomID, room.HostToken, "")
	g := dial(t, srv.URL, room.RoomID, "", "")
	defer g.Close()
	readMsg(t, host) // joined
	readMsg(t, g)    // joined
	readMsg(t, host) // peer-joined

	host.Close()
	pl := readMsg(t, g)
	if pl["type"] != "peer-left" {
		t.Fatalf("expected peer-left on host leave, got %v", pl)
	}

	// Room should be gone now.
	if r := hub.Room(room.RoomID); r != nil {
		t.Fatalf("room still exists after host left")
	}
}

// TestUnknownRoom: connecting to a non-existent room returns 404 over HTTP.
func TestUnknownRoom(t *testing.T) {
	hub := newHub()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/rooms", handleCreateRoom(hub))
	mux.HandleFunc("/v1/ws/", handleWS(hub))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/ws/NOPEX")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// TestMethodNotAllowed: GET on /v1/rooms is rejected.
func TestMethodNotAllowed(t *testing.T) {
	hub := newHub()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/rooms", handleCreateRoom(hub))
	mux.HandleFunc("/v1/ws/", handleWS(hub))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/rooms")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", resp.StatusCode)
	}
}

// TestRelayMissingTo: an offer without "to" returns an error.
func TestRelayMissingTo(t *testing.T) {
	hub := newHub()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/rooms", handleCreateRoom(hub))
	mux.HandleFunc("/v1/ws/", handleWS(hub))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	room := createRoom(t, srv.URL, CreateRoomRequest{})
	host := dial(t, srv.URL, room.RoomID, room.HostToken, "")
	defer host.Close()
	g := dial(t, srv.URL, room.RoomID, "", "")
	defer g.Close()
	readMsg(t, host)
	readMsg(t, g)
	readMsg(t, host)

	sendMsg(t, g, map[string]any{"type": "offer", "sdp": "X"})
	m := readMsg(t, g)
	if m["type"] != "error" {
		t.Fatalf("expected error for missing to, got %v", m)
	}
}
