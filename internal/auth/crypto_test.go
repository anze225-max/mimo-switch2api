package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// encryptAsPlatformDoes reproduces the server side of the contract: an ephemeral
// X25519 point, SHA-256 of the raw shared secret as the AES key, and a payload laid
// out as ephemeralPub || nonce || ciphertext || tag.
func encryptAsPlatformDoes(t *testing.T, recipient *ecdh.PublicKey, plaintext string) string {
	t.Helper()
	ephemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ephemeral key: %v", err)
	}
	shared, err := ephemeral.ECDH(recipient)
	if err != nil {
		t.Fatalf("ecdh: %v", err)
	}
	block, err := aes.NewCipher(sha256sum(shared))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	if gcm.NonceSize() != nonceLen {
		t.Fatalf("nonce size drifted from the platform's: %d != %d", gcm.NonceSize(), nonceLen)
	}
	sealed := gcm.Seal(nil, nonce, []byte(plaintext), nil)

	blob := append([]byte{}, ephemeral.PublicKey().Bytes()...)
	blob = append(blob, nonce...)
	blob = append(blob, sealed...)
	return base64.RawURLEncoding.EncodeToString(blob)
}

func TestDecryptRoundTrip(t *testing.T) {
	kp, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	const body = `{"uid":"3207174710","url":"https://token-plan-cn.xiaomimimo.com/v1","sk":"sk-aabbccddeeff00112233"}`

	res, err := kp.Decrypt(encryptAsPlatformDoes(t, kp.priv.PublicKey(), body))
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if res.UID != "3207174710" {
		t.Errorf("UID = %q", res.UID)
	}
	if res.URL != "https://token-plan-cn.xiaomimimo.com/v1" {
		t.Errorf("URL = %q", res.URL)
	}
	if res.SK != "sk-aabbccddeeff00112233" {
		t.Errorf("SK = %q", res.SK)
	}
}

func TestDecryptRejectsTamperedPayload(t *testing.T) {
	kp, _ := GenerateKeyPair()
	payload := encryptAsPlatformDoes(t, kp.priv.PublicKey(), `{"uid":"1","sk":"sk-x"}`)
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xff // break the GCM tag
	if _, err := kp.Decrypt(base64.RawURLEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("tampered payload decrypted without error")
	}
}

func TestDecryptRejectsPayloadWithoutKey(t *testing.T) {
	kp, _ := GenerateKeyPair()
	if _, err := kp.Decrypt(encryptAsPlatformDoes(t, kp.priv.PublicKey(), `{"uid":"1"}`)); err == nil {
		t.Fatal("payload with no sk accepted")
	}
}

func TestPublicKeyParamIsUnpaddedSPKI(t *testing.T) {
	kp, _ := GenerateKeyPair()
	encoded := kp.PublicKeyParam()
	if strings.Contains(encoded, "=") {
		t.Errorf("Node's base64url omits padding, ours does not: %q", encoded)
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("not base64url: %v", err)
	}
	if len(raw) != len(spkiX25519Prefix)+pointLen {
		t.Fatalf("spki length = %d", len(raw))
	}
	for i, b := range spkiX25519Prefix {
		if raw[i] != b {
			t.Fatalf("spki header byte %d = %#x, want %#x", i, raw[i], b)
		}
	}
	recovered, err := ecdh.X25519().NewPublicKey(raw[len(spkiX25519Prefix):])
	if err != nil {
		t.Fatal(err)
	}
	if string(recovered.Bytes()) != string(kp.priv.PublicKey().Bytes()) {
		t.Error("spki wrap does not round-trip to the public key")
	}
}

func TestKeyNameIsStablePerInstall(t *testing.T) {
	dir := t.TempDir()
	first, err := KeyName(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := KeyName(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("key name not stable: %q then %q", first, second)
	}
	if !regexp.MustCompile(`^mimo-code-cli-key-[0-9a-f]{8}$`).MatchString(first) {
		t.Errorf("key name shape: %q", first)
	}
	// The mode bits are a Unix-only courtesy and Windows reports them as 0600 -> 0666,
	// so assert the file is what will be re-read rather than its permissions.
	if b, err := os.ReadFile(filepath.Join(dir, "key-name")); err != nil || string(b) != first {
		t.Errorf("key-name file = %q, %v; want %q", b, err, first)
	}
}

func TestSessionAdvertisesReachableCallback(t *testing.T) {
	s, err := StartSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	parsed, err := url.Parse(s.AuthorizeURL())
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/authorize" {
		t.Errorf("path = %q", parsed.Path)
	}
	q := parsed.Query()
	for _, want := range []string{"pk", "redirect_uri", "key_name"} {
		if q.Get(want) == "" {
			t.Errorf("%s missing from %s", want, s.AuthorizeURL())
		}
	}
	if q.Get("kn") != product {
		t.Errorf("kn = %q", q.Get("kn"))
	}
	if !strings.HasPrefix(q.Get("redirect_uri"), "http://localhost:") {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	tcp, ok := s.ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T", s.ln.Addr())
	}
	if want := fmt.Sprintf(":%d/", tcp.Port); !strings.HasSuffix(q.Get("redirect_uri"), want) {
		t.Errorf("redirect_uri %q misses listening port %d", q.Get("redirect_uri"), tcp.Port)
	}
	manual, err := url.Parse(s.ManualURL())
	if err != nil {
		t.Fatalf("manual url: %v", err)
	}
	if want := PlatformURL() + "/authorize/code/callback"; manual.Query().Get("redirect_uri") != want {
		t.Errorf("manual redirect_uri = %q, want %q", manual.Query().Get("redirect_uri"), want)
	}
	if manual.Query().Get("pk") != q.Get("pk") {
		t.Error("manual fallback must carry the same ephemeral public key, or it cannot be decrypted")
	}
}
