package bridge

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/t0uh33d/threadsplice/internal/api"
	"github.com/t0uh33d/threadsplice/internal/client"
	"github.com/t0uh33d/threadsplice/internal/e2ee"
	"github.com/t0uh33d/threadsplice/internal/relay"
)

type fakeSession struct {
	mu      sync.Mutex
	prompts []string
}

func (f *fakeSession) Turn(ctx context.Context, prompt string) (string, error) {
	f.mu.Lock()
	f.prompts = append(f.prompts, prompt)
	f.mu.Unlock()
	if strings.Contains(prompt, "explode") {
		return "", errors.New("model overloaded")
	}
	return "answer to: 4", nil
}

func (f *fakeSession) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

func TestChildEnv(t *testing.T) {
	t.Setenv("THREADSPLICE_ADMIN_TOKEN", "secret-admin-token")
	t.Setenv("THREADSPLICE_BRIDGE", "0")
	env := strings.Join(ChildEnv(), "\n")
	if strings.Contains(env, "secret-admin-token") {
		t.Error("admin token leaks into the bridged agent's environment")
	}
	if !strings.Contains(env, "THREADSPLICE_BRIDGE=1") || strings.Contains(env, "THREADSPLICE_BRIDGE=0") {
		t.Error("bridge marker missing or duplicated")
	}
}

type bigSession struct{}

func (bigSession) Turn(context.Context, string) (string, error) {
	return strings.Repeat("é<", maxPayload), nil // multi-byte and HTML-escaped: the fit must hold after escaping
}

func TestBridge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const admin = "test-admin-token-0123456789"
	rs, err := relay.New(relay.Config{DBPath: filepath.Join(t.TempDir(), "r.db"), AdminToken: admin})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(rs.Handler())
	defer func() { ts.Close(); rs.Close() }()
	m, _ := client.New(ts.URL, admin).CreateRoom(ctx, "proj", "asker")
	key := e2ee.NewKey().String()
	asker, _ := client.NewAgent(client.Identity{Relay: ts.URL, RoomID: m.RoomID, RoomName: "proj", ParticipantID: m.ParticipantID, Name: "asker", Owner: true, Token: m.Token, RoomKey: key})
	inv, _ := asker.Relay.CreateInvite(ctx, m.RoomID, time.Hour)
	j, err := client.New(ts.URL, "").Join(ctx, inv.Token, "codex")
	if err != nil {
		t.Fatal(err)
	}
	bot, _ := client.NewAgent(client.Identity{Relay: ts.URL, RoomID: j.RoomID, RoomName: "proj", ParticipantID: j.ParticipantID, Name: "codex", Token: j.Token, RoomKey: key})

	sess := &fakeSession{}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, bot, sess, Options{MaxDepth: 3}) }()

	ask, _ := asker.Send(ctx, "codex", api.Payload{Kind: api.KindAsk, Text: "what is 2+2"})
	asker.Send(ctx, "codex", api.Payload{Kind: api.KindMessage, Text: "fyi: build is green"})
	deep, _ := asker.Send(ctx, "codex", api.Payload{Kind: api.KindAsk, Text: "still there?", Depth: 3})
	boom, _ := asker.Send(ctx, "codex", api.Payload{Kind: api.KindAsk, Text: "explode"})

	replies := map[string]client.Message{}
	deadline := time.Now().Add(10 * time.Second)
	for len(replies) < 3 && time.Now().Before(deadline) {
		msgs, err := asker.Inbox(ctx, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range msgs {
			replies[r.ReplyTo] = r
		}
	}
	if r := replies[ask]; r.Kind != api.KindReply || r.Depth != 1 || !strings.Contains(r.Text, "answer to:") {
		t.Errorf("reply to ask = %+v", r)
	}
	if r := replies[deep]; !strings.Contains(r.Text, "automatic-reply limit") || r.Depth != 4 {
		t.Errorf("deep ask should be refused without a turn: %+v", r)
	}
	if r := replies[boom]; !strings.Contains(r.Text, "model overloaded") {
		t.Errorf("failed turn should be reported: %+v", r)
	}
	if len(replies) != 3 {
		t.Errorf("got %d replies, want 3 (the plain message gets none)", len(replies))
	}
	if n := sess.count(); n != 3 { // ask, message, explode — not the over-depth ask
		t.Errorf("turns = %d, want 3", n)
	}
	sess.mu.Lock()
	first := sess.prompts[0]
	sess.mu.Unlock()
	if !strings.Contains(first, "untrusted input") || !strings.Contains(first, "sent back to \"asker\" automatically") {
		t.Errorf("turn prompt is not framed as untrusted peer input:\n%s", first)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v", err)
	}

	// An oversized answer is truncated to fit the relay instead of being lost.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go Run(ctx2, bot, bigSession{}, Options{})
	big, _ := asker.Send(ctx2, "codex", api.Payload{Kind: api.KindAsk, Text: "dump everything"})
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		msgs, err := asker.Inbox(ctx2, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range msgs {
			if r.ReplyTo == big {
				if !strings.Contains(r.Text, "truncated") || !utf8.ValidString(r.Text) || len(r.Text) > maxPayload {
					t.Fatalf("oversized answer: len %d, truncated note %v, valid %v", len(r.Text), strings.Contains(r.Text, "truncated"), utf8.ValidString(r.Text))
				}
				return
			}
		}
	}
	t.Fatal("no reply to the oversized answer")
}
