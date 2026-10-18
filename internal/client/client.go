// Package client talks to a thread splice relay on behalf of a local agent:
// relay API calls, local identities, and sealing/opening messages.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/t0uh33d/threadsplice/internal/api"
)

// Client is a thin HTTP client for the relay API.
type Client struct {
	Base  string // relay base URL, e.g. https://relay.example.com
	Token string // bearer credential
	HTTP  *http.Client
}

// New returns a client for the relay at base.
func New(base, token string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), Token: token, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// StatusError is a non-2xx relay response.
type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string { return fmt.Sprintf("relay: %s (HTTP %d)", e.Msg, e.Code) }

// CheckRelayURL rejects relay URLs that would send bearer tokens in cleartext.
// Plain http is allowed only for loopback, or when THREADSPLICE_INSECURE=1.
func CheckRelayURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("invalid relay URL %q (want https://host[:port])", raw)
	}
	if u.Scheme == "https" || os.Getenv("THREADSPLICE_INSECURE") == "1" {
		return nil
	}
	if ip := net.ParseIP(u.Hostname()); u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback()) {
		return nil
	}
	return fmt.Errorf("refusing plain http to %s: use https (or set THREADSPLICE_INSECURE=1 on a trusted private network)", u.Host)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e api.Error
		json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
		if e.Error == "" {
			e.Error = http.StatusText(resp.StatusCode)
		}
		return &StatusError{Code: resp.StatusCode, Msg: e.Error}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// CreateRoom creates a room; c.Token must be the relay admin token.
func (c *Client) CreateRoom(ctx context.Context, name, owner string) (api.Membership, error) {
	var m api.Membership
	err := c.do(ctx, "POST", "/v1/rooms", api.CreateRoomRequest{Name: name, Owner: owner}, &m)
	return m, err
}

// CreateInvite mints a single-use invite token (owner only).
func (c *Client) CreateInvite(ctx context.Context, room string, ttl time.Duration) (api.Invite, error) {
	var inv api.Invite
	err := c.do(ctx, "POST", "/v1/rooms/"+room+"/invites", api.CreateInviteRequest{TTLSeconds: int(ttl.Seconds())}, &inv)
	return inv, err
}

// Join redeems an invite token. It needs no credential.
func (c *Client) Join(ctx context.Context, invite, name string) (api.Membership, error) {
	var m api.Membership
	err := c.do(ctx, "POST", "/v1/join", api.JoinRequest{Invite: invite, Name: name}, &m)
	return m, err
}

// Participants lists room members with presence and acknowledgement cursors.
func (c *Client) Participants(ctx context.Context, room string) ([]api.Participant, error) {
	var ps []api.Participant
	err := c.do(ctx, "GET", "/v1/rooms/"+room+"/participants", nil, &ps)
	return ps, err
}

// SetState reports this participant's state (e.g. "busy", "idle") to the room.
// Any authenticated request can carry ?state=; this is the cheapest one.
func (c *Client) SetState(ctx context.Context, room, state string) error {
	return c.do(ctx, "GET", "/v1/rooms/"+room+"/participants?state="+url.QueryEscape(state), nil, nil)
}

// Revoke removes a participant's access immediately (owner only).
func (c *Client) Revoke(ctx context.Context, room, pid string) error {
	return c.do(ctx, "DELETE", "/v1/rooms/"+room+"/participants/"+url.PathEscape(pid), nil, nil)
}

// Send submits a sealed message. It retries transient failures; the relay
// deduplicates retries by message id, so this is safe.
func (c *Client) Send(ctx context.Context, room string, req api.SendRequest) (api.SendResponse, error) {
	var resp api.SendResponse
	var err error
	for attempt := range 4 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return resp, ctx.Err()
			case <-time.After(Backoff(attempt)):
			}
		}
		if err = c.do(ctx, "POST", "/v1/rooms/"+room+"/messages", req, &resp); !retryable(err) {
			return resp, err
		}
	}
	return resp, err
}

// Messages long-polls for envelopes after seq `after` (after < 0 resumes from the
// caller's acknowledged cursor), waiting up to `wait`. state, if set, reports
// presence such as "idle" or "busy".
func (c *Client) Messages(ctx context.Context, room string, after int64, wait time.Duration, state string) ([]api.Envelope, error) {
	q := url.Values{}
	if after >= 0 {
		q.Set("after", strconv.FormatInt(after, 10))
	}
	if wait > 0 {
		q.Set("wait", strconv.Itoa(int(wait.Seconds())))
	}
	if state != "" {
		q.Set("state", state)
	}
	var resp api.MessagesResponse
	err := c.do(ctx, "GET", "/v1/rooms/"+room+"/messages?"+q.Encode(), nil, &resp)
	return resp.Messages, err
}

// Ack records that everything up to seq has been processed.
func (c *Client) Ack(ctx context.Context, room string, seq int64) error {
	return c.do(ctx, "POST", "/v1/rooms/"+room+"/ack", api.AckRequest{Seq: seq}, nil)
}

// retryable reports whether err is a transient failure (network error or 5xx).
func retryable(err error) bool {
	if err == nil {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code >= 500
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// Backoff is exponential backoff with full jitter: random in [0, min(30s, 500ms*2^attempt)].
func Backoff(attempt int) time.Duration {
	d := min(30*time.Second, 500*time.Millisecond<<min(attempt, 6))
	return time.Duration(rand.Int64N(int64(d) + 1))
}
