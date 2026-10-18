// Package api defines the relay's HTTP wire format (protocol v1, served under /v1).
//
// The relay stores and routes Envelopes. The conversation itself travels in
// Envelope.Sealed, encrypted end-to-end with the room key (package e2ee), so the
// relay only ever sees routing metadata: room, sender, recipient, message id,
// size and time.
package api

import "time"

// CreateRoomRequest creates a room and its owner. Requires the relay admin token.
type CreateRoomRequest struct {
	Name  string `json:"name"`  // room display name
	Owner string `json:"owner"` // owner's participant name
}

// Membership is returned when a participant is created (room creation or join).
// Token is the participant's bearer credential; the relay stores only its hash.
type Membership struct {
	RoomID        string `json:"room_id"`
	RoomName      string `json:"room_name"`
	ParticipantID string `json:"participant_id"`
	Token         string `json:"token"`
}

// CreateInviteRequest asks for a single-use invite token. Owner only.
type CreateInviteRequest struct {
	TTLSeconds int `json:"ttl_seconds,omitempty"` // default 24h, max 7 days
}

// Invite is a single-use, expiring invite token. It grants room membership but
// does NOT contain the room key: clients append the key to the invite string
// locally, so the relay never sees it.
type Invite struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// JoinRequest redeems an invite token.
type JoinRequest struct {
	Invite string `json:"invite"`
	Name   string `json:"name"`
}

// Participant is a room member as the relay sees it.
type Participant struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Owner    bool      `json:"owner,omitempty"`
	State    string    `json:"state,omitempty"` // self-reported, e.g. "idle", "busy"
	LastSeen time.Time `json:"last_seen,omitzero"`
	Online   bool      `json:"online"`
	AckedSeq int64     `json:"acked_seq"` // highest message seq the participant has acknowledged
	Revoked  bool      `json:"revoked,omitempty"`
}

// SendRequest submits a sealed message. Retrying with the same ID is safe: the
// relay deduplicates on (sender, ID) and returns the original sequence number.
type SendRequest struct {
	ID     string `json:"id"`
	To     string `json:"to,omitempty"` // participant id; empty addresses the whole room
	Sealed []byte `json:"sealed"`
}

// SendResponse acknowledges durable acceptance.
type SendResponse struct {
	Seq       int64 `json:"seq"`
	Duplicate bool  `json:"duplicate,omitempty"`
}

// Envelope is a stored message. Seq is a relay-assigned, monotonically
// increasing cursor; clients resume with GET .../messages?after=<seq>.
type Envelope struct {
	Seq       int64     `json:"seq"`
	ID        string    `json:"id"`
	From      string    `json:"from"`
	To        string    `json:"to,omitempty"`
	Sealed    []byte    `json:"sealed"`
	CreatedAt time.Time `json:"created_at"`
}

// MessagesResponse is a page of envelopes, oldest first.
type MessagesResponse struct {
	Messages []Envelope `json:"messages"`
}

// AckRequest records that the caller has processed everything up to Seq.
type AckRequest struct {
	Seq int64 `json:"seq"`
}

// Error is the body of every non-2xx response.
type Error struct {
	Error string `json:"error"`
}

// Payload is the plaintext inside Envelope.Sealed. The relay never sees it.
type Payload struct {
	Kind    string `json:"kind"` // KindMessage, KindAsk or KindReply
	Text    string `json:"text"`
	ReplyTo string `json:"reply_to,omitempty"` // id of the message this answers
	Depth   int    `json:"depth,omitempty"`    // automatic-reply chain length, for loop prevention
}

// Payload kinds.
const (
	KindMessage = "message" // informational; no reply expected
	KindAsk     = "ask"     // the sender expects a reply
	KindReply   = "reply"   // answers ReplyTo
)
