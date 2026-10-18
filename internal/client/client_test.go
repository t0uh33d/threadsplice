package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/t0uh33d/threadsplice/internal/api"
	"github.com/t0uh33d/threadsplice/internal/e2ee"
)

// A malicious relay replaying a stored message must not get it shown twice,
// and the cursor must still move past the replay (or polls would spin on it).
func TestFetchDropsReplays(t *testing.T) {
	key := e2ee.NewKey()
	pt, _ := json.Marshal(api.Payload{Kind: api.KindAsk, Text: "deploy?"})
	env := api.Envelope{ID: "m1", From: "bob", Sealed: key.Seal(e2ee.Header{Room: "r", From: "bob", ID: "m1"}, pt)}
	forged := api.Envelope{Seq: 1, ID: "m1", From: "bob", Sealed: []byte{1, 0, 0}}
	first, replay := env, env
	first.Seq, replay.Seq = 2, 7

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/participants") {
			json.NewEncoder(w).Encode([]api.Participant{{ID: "bob", Name: "bob"}})
			return
		}
		json.NewEncoder(w).Encode(api.MessagesResponse{Messages: []api.Envelope{forged, first, replay}})
	}))
	defer ts.Close()

	a, err := NewAgent(Identity{Relay: ts.URL, RoomID: "r", ParticipantID: "alice", RoomKey: key.String()})
	if err != nil {
		t.Fatal(err)
	}
	msgs, last, err := a.Fetch(context.Background(), -1, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if last != 7 {
		t.Errorf("last = %d, want 7 (past the replay)", last)
	}
	if len(msgs) != 2 || msgs[0].Err == nil || msgs[1].Text != "deploy?" || msgs[1].FromName != "bob" {
		t.Fatalf("msgs = %+v (want the forgery flagged, the genuine message once)", msgs)
	}
}

func TestInviteRoundTrip(t *testing.T) {
	key := e2ee.NewKey()
	inv := FormatInvite("https://relay.example.com/", "tok_123", key)
	relay, tok, k, err := ParseInvite("  " + inv + "\n")
	if err != nil || relay != "https://relay.example.com" || tok != "tok_123" || k != key {
		t.Fatalf("ParseInvite = %q %q %v %v", relay, tok, k == key, err)
	}
	for _, bad := range []string{"https://relay.example.com", "https://r#tok", "https://r#tok.notakey", "::"} {
		if _, _, _, err := ParseInvite(bad); err == nil {
			t.Errorf("ParseInvite(%q) accepted", bad)
		}
	}
}

func TestCheckRelayURL(t *testing.T) {
	t.Setenv("THREADSPLICE_INSECURE", "")
	for u, ok := range map[string]bool{
		"https://relay.example.com": true,
		"http://127.0.0.1:8080":     true,
		"http://localhost:8080":     true,
		"http://[::1]:8080":         true,
		"http://relay.example.com":  false,
		"ftp://relay.example.com":   false,
		"relay.example.com":         false,
	} {
		if err := CheckRelayURL(u); (err == nil) != ok {
			t.Errorf("CheckRelayURL(%q) = %v, want ok=%v", u, err, ok)
		}
	}
}
