package main

// Message is the envelope for every WebSocket frame exchanged with the server.
//
// All messages are JSON objects identified by a "type" field. The structs below
// cover both directions (client→server and server→client); the wire format is
// documented in PROTOCOL.md.

// ClientIn is any message sent from a client to the server.
type ClientIn struct {
	Type      string `json:"type"`                // "offer" | "answer" | "ice" | "leave"
	To        string `json:"to,omitempty"`        // target peer id (for offer/answer/ice)
	SDP       any    `json:"sdp,omitempty"`       // SDP payload (offer/answer)
	Candidate any    `json:"candidate,omitempty"` // ICE candidate (ice)
}

// ServerOut is any message sent from the server to a client.
type ServerOut struct {
	Type      string   `json:"type"`              // see PROTOCOL.md
	Room      string   `json:"room,omitempty"`    // room id
	PeerID    string   `json:"peer_id,omitempty"` // this client's peer id
	Peers     []string `json:"peers"`             // existing peer ids (in "joined")
	From      string   `json:"from,omitempty"`    // originator peer id (relayed msgs)
	To        string   `json:"to,omitempty"`      // target peer id (relayed msgs)
	SDP       any      `json:"sdp,omitempty"`
	Candidate any      `json:"candidate,omitempty"`
	Message   string   `json:"message,omitempty"` // error detail
}

// CreateRoomRequest is the body of POST /v1/rooms.
type CreateRoomRequest struct {
	Password   string `json:"password,omitempty"`
	MaxClients int    `json:"max_clients,omitempty"`
}

// CreateRoomResponse is returned by POST /v1/rooms.
type CreateRoomResponse struct {
	RoomID    string `json:"room_id"`
	HostToken string `json:"host_token"`
}
