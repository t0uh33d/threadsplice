package e2ee

import (
	"bytes"
	"testing"
)

func TestSealOpen(t *testing.T) {
	k := NewKey()
	h := Header{Room: "r1", From: "alice", To: "bob", ID: "m1"}
	msg := []byte("review my diff please")

	sealed := k.Seal(h, msg)
	if bytes.Contains(sealed, msg) {
		t.Fatal("plaintext visible in sealed output")
	}
	got, err := k.Open(h, sealed)
	if err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("Open = %q, %v", got, err)
	}

	// Anything the relay could tamper with must break authentication.
	for name, h2 := range map[string]Header{
		"room":    {Room: "r2", From: "alice", To: "bob", ID: "m1"},
		"from":    {Room: "r1", From: "mallory", To: "bob", ID: "m1"},
		"to":      {Room: "r1", From: "alice", To: "", ID: "m1"},
		"id":      {Room: "r1", From: "alice", To: "bob", ID: "m2"},
		"shifted": {Room: "r1", From: "alicebo", To: "b", ID: "m1"}, // length-prefixing prevents field-boundary games
	} {
		if _, err := k.Open(h2, sealed); err != ErrOpen {
			t.Errorf("%s: tampered header accepted (err=%v)", name, err)
		}
	}
	flipped := bytes.Clone(sealed)
	flipped[len(flipped)-1] ^= 1
	if _, err := k.Open(h, flipped); err != ErrOpen {
		t.Errorf("tampered ciphertext accepted (err=%v)", err)
	}
	if _, err := NewKey().Open(h, sealed); err != ErrOpen {
		t.Errorf("wrong key accepted (err=%v)", err)
	}
	if _, err := k.Open(h, append([]byte{9}, sealed[1:]...)); err == nil {
		t.Error("unknown version accepted")
	}
	if bytes.Equal(k.Seal(h, msg), sealed) {
		t.Error("nonce reuse: two seals produced identical output")
	}
}

func TestKeyRoundTrip(t *testing.T) {
	k := NewKey()
	k2, err := ParseKey(k.String())
	if err != nil || k2 != k {
		t.Fatalf("ParseKey(String()) = %v, %v", k2, err)
	}
	for _, bad := range []string{"", "short", k.String() + "AA", "!!!" + k.String()[3:]} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) accepted", bad)
		}
	}
}
