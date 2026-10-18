// Package e2ee seals agent messages so the relay only ever stores ciphertext.
//
// Each room has a 32-byte key that the room creator generates locally and shares
// inside the invite string; the relay never receives it. Messages are sealed with
// AES-256-GCM (random 96-bit nonce) under a key derived from the room key with
// HKDF-SHA256. The routing metadata the relay does see (room, sender, recipient,
// message id) is bound as additional data, so the relay cannot re-attribute,
// re-route or replay a message into another room without decryption failing.
package e2ee

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
)

// KeySize is the size of a room key in bytes.
const KeySize = 32

// version prefixes every sealed message so the format can evolve.
const version byte = 1

// Key is a room key.
type Key [KeySize]byte

// NewKey returns a fresh random room key.
func NewKey() Key {
	var k Key
	rand.Read(k[:]) // never fails since Go 1.24 (crashes the process instead)
	return k
}

// String encodes the key as unpadded base64url, the form used in invites.
func (k Key) String() string { return base64.RawURLEncoding.EncodeToString(k[:]) }

// ParseKey decodes a key produced by Key.String.
func ParseKey(s string) (Key, error) {
	var k Key
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != KeySize {
		return k, errors.New("e2ee: invalid room key")
	}
	copy(k[:], b)
	return k, nil
}

// Header is the routing metadata the relay sees. It is authenticated, not encrypted.
// To is empty for messages addressed to the whole room.
type Header struct {
	Room, From, To, ID string
}

func (h Header) ad() []byte {
	b := []byte("threadsplice/v1\x00")
	for _, f := range []string{h.Room, h.From, h.To, h.ID} {
		b = binary.AppendUvarint(b, uint64(len(f)))
		b = append(b, f...)
	}
	return b
}

// ponytail: random 96-bit nonces under one derived key are safe for ~2^32 messages per room;
// rotate by creating a new room long before that, or move to per-sender keys if it ever matters.
func (k Key) aead() cipher.AEAD {
	mk, err := hkdf.Key(sha256.New, k[:], nil, "threadsplice/v1/message", 32)
	if err != nil {
		panic(err) // only possible for invalid lengths, which are constants here
	}
	block, err := aes.NewCipher(mk)
	if err != nil {
		panic(err)
	}
	a, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		panic(err)
	}
	return a
}

// Seal encrypts plaintext under the room key and binds h to the result.
func (k Key) Seal(h Header, plaintext []byte) []byte {
	return k.aead().Seal([]byte{version}, nil, plaintext, h.ad())
}

// ErrOpen is returned when a message was sealed under a different key, or was
// modified (ciphertext or header) after sealing.
var ErrOpen = errors.New("e2ee: message failed authentication (wrong room key, or modified in transit)")

// Open decrypts a message produced by Seal with an identical header.
func (k Key) Open(h Header, sealed []byte) ([]byte, error) {
	if len(sealed) == 0 || sealed[0] != version {
		return nil, errors.New("e2ee: unsupported message format")
	}
	pt, err := k.aead().Open(nil, nil, sealed[1:], h.ad())
	if err != nil {
		return nil, ErrOpen
	}
	return pt, nil
}
