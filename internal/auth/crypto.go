// Package auth implements MiMo's first-party key-issuing flow: register an X25519
// public key with platform.xiaomimimo.com, receive an encrypted {uid,url,sk} back on
// the localhost callback, and decrypt it.
package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// The platform speaks in bare 32-byte X25519 points, wrapped in this DER
// SubjectPublicKeyInfo header when Node encodes them as SPKI/der.
var spkiX25519Prefix = []byte{
	0x30, 0x2a, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x6e, 0x03, 0x21, 0x00,
}

const (
	pointLen  = 32
	nonceLen  = 12
	tagLen    = 16
	overhead  = pointLen + nonceLen + tagLen
	keyPrefix = "mimo-code-cli-key-"
)

// Result is the JSON the platform encrypts into the ?u= callback parameter.
type Result struct {
	UID string `json:"uid"`
	URL string `json:"url"`
	SK  string `json:"sk"`
}

// KeyPair is an ephemeral X25519 identity, valid for a single authorize round trip.
type KeyPair struct {
	priv *ecdh.PrivateKey
}

func GenerateKeyPair() (*KeyPair, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate x25519 key: %w", err)
	}
	return &KeyPair{priv: priv}, nil
}

// PublicKeyParam is the `pk` query value: base64url(SPKI-DER), unpadded, matching
// Node's generateKeyPairSync(...).publicKeyEncoding{spki,der} + toString("base64url").
func (k *KeyPair) PublicKeyParam() string {
	return base64.RawURLEncoding.EncodeToString(
		append(append([]byte{}, spkiX25519Prefix...), k.priv.PublicKey().Bytes()...))
}

// Decrypt unwraps a `u` payload laid out as ephemeralPub(32) || nonce(12) || ct || tag(16),
// keyed by SHA-256 of the raw ECDH secret.
func (k *KeyPair) Decrypt(u string) (*Result, error) {
	blob, err := decodeBase64URL(u)
	if err != nil {
		return nil, fmt.Errorf("decode u payload: %w", err)
	}
	if len(blob) <= overhead {
		return nil, fmt.Errorf("u payload too short: %d bytes", len(blob))
	}
	ephemeral, nonce, sealed := blob[:pointLen], blob[pointLen:pointLen+nonceLen], blob[pointLen+nonceLen:]
	plaintext, err := k.open(ephemeral, nonce, sealed)
	if err != nil {
		return nil, err
	}
	var result Result
	if err := json.Unmarshal(plaintext, &result); err != nil {
		return nil, fmt.Errorf("decrypt yields JSON: %w", err)
	}
	if result.SK == "" {
		return nil, errors.New("decrypted payload carries no sk")
	}
	return &result, nil
}

func (k *KeyPair) open(ephemeral, nonce, sealed []byte) ([]byte, error) {
	peer, err := ecdh.X25519().NewPublicKey(ephemeral)
	if err != nil {
		return nil, fmt.Errorf("ephemeral public key: %w", err)
	}
	shared, err := k.priv.ECDH(peer)
	if err != nil {
		return nil, fmt.Errorf("ecdh: %w", err)
	}
	block, err := aes.NewCipher(sha256sum(shared))
	if err != nil {
		return nil, fmt.Errorf("aes key: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	// The layout is fixed by the desktop plugin's Node implementation; a drift here
	// would silently mis-frame every payload, so assert rather than assume.
	if gcm.NonceSize() != len(nonce) {
		return nil, fmt.Errorf("nonce size %d, platform uses %d", len(nonce), gcm.NonceSize())
	}
	// Go folds the GCM tag into the ciphertext argument, unlike Node's setAuthTag.
	plaintext, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("gcm open: %w", err)
	}
	return plaintext, nil
}

// decodeBase64URL accepts both padded and unpadded input, since the authorize page
// controls what it puts in the query string.
func decodeBase64URL(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

func sha256sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}
