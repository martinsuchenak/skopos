// Package secretbox seals the secrets skopos stores — webhook signing
// secrets, the credentials of MCP servers — under the server's secret key
// (integrations.secret_key), and lets that key be rotated
// (tmp/central-config — design.md, phase 2b).
//
// A sealed value names the key that sealed it. A server holds the current
// key and, while a rotation is under way, the previous one: it opens what
// either sealed and seals with the current one. `skopos secret-key rotate`
// seals everything again under the current key, after which the previous
// key can be dropped.
package secretbox

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

var (
	// ErrNoKey: the server has no secret key, so it stores no secrets.
	ErrNoKey = errors.New("the server has no secret key (integrations.secret_key / SKOPOS_INTEGRATIONS_KEY)")
	// ErrUnknownKey: the value was sealed under a key this server does not
	// hold.
	ErrUnknownKey = errors.New("sealed under a secret key this server does not hold")
)

// prefix marks a sealed value that names its key: "skb1:<key id>:" then
// the nonce and the ciphertext. A value without it is from before keys
// were named (a webhook secret of an earlier release).
const prefix = "skb1:"

// LegacyKey is the key id reported for a sealed value that does not name
// its key.
const LegacyKey = "unnamed"

type key struct {
	id  string
	raw []byte
}

// Ring is the keys a server holds: the current one first.
type Ring struct {
	keys []key
}

// KeyID is the public name of a secret key: a fingerprint that says which
// key sealed a value without saying anything about the key.
func KeyID(secret string) string {
	raw := sha256.Sum256([]byte(secret))
	sum := sha256.Sum256(append([]byte("skopos secret key id\x00"), raw[:]...))
	return hex.EncodeToString(sum[:])[:12]
}

// New builds a ring from the current key and the previous ones still held
// ("" entries are skipped). Without a current key the ring is disabled:
// nothing is sealed or opened.
func New(current string, previous ...string) *Ring {
	r := &Ring{}
	if strings.TrimSpace(current) == "" {
		return r
	}
	for _, secret := range append([]string{current}, previous...) {
		if strings.TrimSpace(secret) == "" {
			continue
		}
		id := KeyID(secret)
		dup := false
		for _, k := range r.keys {
			dup = dup || k.id == id
		}
		if dup {
			continue
		}
		raw := sha256.Sum256([]byte(secret))
		r.keys = append(r.keys, key{id: id, raw: raw[:]})
	}
	return r
}

// Enabled reports whether the server has a secret key.
func (r *Ring) Enabled() bool { return r != nil && len(r.keys) > 0 }

// Current is the id of the key new values are sealed under ("" = none).
func (r *Ring) Current() string {
	if !r.Enabled() {
		return ""
	}
	return r.keys[0].id
}

// Previous lists the ids of the other keys the server still holds.
func (r *Ring) Previous() []string {
	if !r.Enabled() {
		return nil
	}
	out := make([]string, 0, len(r.keys)-1)
	for _, k := range r.keys[1:] {
		out = append(out, k.id)
	}
	return out
}

func gcmOf(raw []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal encrypts plain under the current key.
func (r *Ring) Seal(plain []byte) ([]byte, error) {
	if !r.Enabled() {
		return nil, ErrNoKey
	}
	k := r.keys[0]
	gcm, err := gcmOf(k.raw)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	head := []byte(prefix + k.id + ":")
	// The key's name is authenticated with the value: a value cannot be
	// relabelled as another key's.
	return append(head, gcm.Seal(nonce, nonce, plain, head)...), nil
}

// SealedBy is the id of the key a sealed value names (LegacyKey for one
// from before keys were named, "" for an empty value).
func SealedBy(sealed []byte) string {
	if len(sealed) == 0 {
		return ""
	}
	id, _, ok := split(sealed)
	if !ok {
		return LegacyKey
	}
	return id
}

func split(sealed []byte) (id string, body []byte, ok bool) {
	if !bytes.HasPrefix(sealed, []byte(prefix)) {
		return "", sealed, false
	}
	rest := sealed[len(prefix):]
	i := bytes.IndexByte(rest, ':')
	if i <= 0 || i > 32 {
		return "", sealed, false
	}
	return string(rest[:i]), rest[i+1:], true
}

func openWith(k key, body, aad []byte) ([]byte, error) {
	gcm, err := gcmOf(k.raw)
	if err != nil {
		return nil, err
	}
	if len(body) < gcm.NonceSize() {
		return nil, errors.New("sealed value is corrupt")
	}
	return gcm.Open(nil, body[:gcm.NonceSize()], body[gcm.NonceSize():], aad)
}

// Open decrypts a sealed value with the key it names. A value that names
// none is tried with every key held.
func (r *Ring) Open(sealed []byte) ([]byte, error) {
	if len(sealed) == 0 {
		return nil, nil
	}
	if !r.Enabled() {
		return nil, ErrNoKey
	}
	id, body, named := split(sealed)
	if named {
		for _, k := range r.keys {
			if k.id != id {
				continue
			}
			plain, err := openWith(k, body, sealed[:len(sealed)-len(body)])
			if err != nil {
				return nil, fmt.Errorf("sealed value under key %s cannot be opened: it is corrupt", id)
			}
			return plain, nil
		}
		return nil, fmt.Errorf("%w (key %s)", ErrUnknownKey, id)
	}
	for _, k := range r.keys {
		if plain, err := openWith(k, sealed, nil); err == nil {
			return plain, nil
		}
	}
	return nil, fmt.Errorf("%w (a value from before keys were named: was the secret key changed without a rotation?)", ErrUnknownKey)
}

// Current reports whether a sealed value names the current key.
func (r *Ring) UnderCurrent(sealed []byte) bool {
	return r.Enabled() && SealedBy(sealed) == r.keys[0].id
}

// Reseal seals a value again under the current key when it is not already.
// changed is false, and out is the input, for a value that is.
func (r *Ring) Reseal(sealed []byte) (out []byte, changed bool, err error) {
	if len(sealed) == 0 || r.UnderCurrent(sealed) {
		return sealed, false, nil
	}
	plain, err := r.Open(sealed)
	if err != nil {
		return nil, false, err
	}
	out, err = r.Seal(plain)
	return out, err == nil, err
}
