package push

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/hkdf"

	"github.com/Mino829/trace_Backend/internal/config"
	"github.com/Mino829/trace_Backend/internal/store"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (fn transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func testSender(t *testing.T) *Service {
	t.Helper()
	enc := base64.RawURLEncoding
	private := append(make([]byte, 31), 1)
	x, y := elliptic.P256().ScalarBaseMult(private)
	cfg := config.Default()
	cfg.PushVAPIDPrivateKey = enc.EncodeToString(private)
	cfg.PushVAPIDPublicKey = enc.EncodeToString(elliptic.Marshal(elliptic.P256(), x, y))
	cfg.PushEncryptionKey = enc.EncodeToString(bytes.Repeat([]byte{77}, 32))
	cfg.PushVAPIDSubject = "mailto:contact@example.org"
	sender, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return sender
}

func TestSeparateEncryptionKeyAndProviderAllowlist(t *testing.T) {
	cfg := config.Default()
	scalar := append(make([]byte, 31), 1)
	x, y := elliptic.P256().ScalarBaseMult(scalar)
	cfg.PushVAPIDPrivateKey = encoding.EncodeToString(scalar)
	cfg.PushVAPIDPublicKey = encoding.EncodeToString(elliptic.Marshal(elliptic.P256(), x, y))
	cfg.PushEncryptionKey = cfg.PushVAPIDPrivateKey
	cfg.PushVAPIDSubject = "mailto:contact@example.org"
	if _, err := New(cfg); err == nil {
		t.Fatal("VAPID key reused for at-rest encryption")
	}
	for _, hostile := range []string{"http://fcm.googleapis.com/secret", "https://fcm.googleapis.com.evil.test/secret", "https://127.0.0.1/secret", "https://fcm.googleapis.com:8443/secret", "https://fcm.googleapis.com@evil.test/secret"} {
		if AllowedEndpoint(hostile) {
			t.Fatal("unsafe provider URL accepted")
		}
	}
}
func TestEncryptedPushRoundTripAndProviderRequest(t *testing.T) {
	sender := testSender(t)
	client, err := ecdh.P256().NewPrivateKey(append(make([]byte, 31), 2))
	if err != nil {
		t.Fatal(err)
	}
	auth := bytes.Repeat([]byte{19}, 16)
	keys := Keys{P256DH: encoding.EncodeToString(client.PublicKey().Bytes()), Auth: encoding.EncodeToString(auth)}
	id := "memory-private"
	endpoint := "https://fcm.googleapis.com/fcm/send/provider-secret"
	encryptedEndpoint, err := sender.Seal(id, []byte(endpoint))
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, _ := json.Marshal(keys)
	encryptedKeys, err := sender.Seal(id, keyBytes)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encryptedEndpoint, []byte(endpoint)) || bytes.Contains(encryptedKeys, []byte(keys.Auth)) {
		t.Fatal("plaintext credentials")
	}
	if _, err := sender.Open("different-session", encryptedKeys); err == nil {
		t.Fatal("cross-session decryption")
	}
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	sender.client.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != endpoint || r.Header.Get("Content-Encoding") != "aes128gcm" || r.Header.Get("TTL") != "60" {
			t.Fatal("provider request headers")
		}
		authHeader := r.Header.Get("Authorization")
		if !strings.HasPrefix(authHeader, "vapid t=") || !strings.HasSuffix(authHeader, ", k="+sender.public) {
			t.Fatal("missing VAPID authorization")
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) < 86 || binary.BigEndian.Uint32(data[16:20]) != 4096 || data[20] != 65 {
			t.Fatal("invalid aes128gcm record")
		}
		shared, err := client.ECDH(mustPublic(t, data[21:86]))
		if err != nil {
			t.Fatal(err)
		}
		info := append([]byte("WebPush: info\x00"), client.PublicKey().Bytes()...)
		info = append(info, data[21:86]...)
		ikm := make([]byte, 32)
		if _, err = io.ReadFull(hkdf.Expand(sha256.New, hkdf.Extract(sha256.New, shared, auth), info), ikm); err != nil {
			t.Fatal(err)
		}
		prk := hkdf.Extract(sha256.New, ikm, data[:16])
		key := make([]byte, 16)
		nonce := make([]byte, 12)
		_, _ = io.ReadFull(hkdf.Expand(sha256.New, prk, []byte("Content-Encoding: aes128gcm\x00")), key)
		_, _ = io.ReadFull(hkdf.Expand(sha256.New, prk, []byte("Content-Encoding: nonce\x00")), nonce)
		block, _ := aes.NewCipher(key)
		aead, _ := cipher.NewGCM(block)
		plain, err := aead.Open(nil, nonce, data[86:], nil)
		if err != nil || !bytes.Equal(plain, append([]byte(Payload), 2)) {
			t.Fatal("invalid Web Push payload", err)
		}
		if strings.Contains(Payload, "memory-private") || strings.Contains(Payload, "provider-secret") || !strings.Contains(Payload, `"url":"/photo"`) {
			t.Fatal("payload leaks private data")
		}
		token := strings.TrimSuffix(strings.TrimPrefix(authHeader, "vapid t="), ", k="+sender.public)
		pieces := strings.Split(token, ".")
		if len(pieces) != 3 {
			t.Fatal("invalid VAPID JWT")
		}
		claimsJSON, err := encoding.DecodeString(pieces[1])
		if err != nil {
			t.Fatal(err)
		}
		var claims struct {
			Aud string `json:"aud"`
			Exp int64  `json:"exp"`
		}
		if err := json.Unmarshal(claimsJSON, &claims); err != nil || claims.Aud != "https://fcm.googleapis.com" || claims.Exp != now.Add(12*time.Hour).Unix() {
			t.Fatal("invalid VAPID claims", err)
		}
		sig, err := encoding.DecodeString(pieces[2])
		if err != nil || len(sig) != 64 {
			t.Fatal("invalid VAPID signature")
		}
		digest := sha256.Sum256([]byte(pieces[0] + "." + pieces[1]))
		if !elliptic.P256().IsOnCurve(sender.vapid.X, sender.vapid.Y) || !verifyVAPID(sender.vapid.PublicKey, digest[:], sig) {
			t.Fatal("VAPID signature mismatch")
		}
		return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})
	status, err := sender.Send(context.Background(), store.PushSubscription{MemorySessionID: id, EndpointEncrypted: encryptedEndpoint, KeysEncrypted: encryptedKeys}, now)
	if err != nil || status != 201 {
		t.Fatal("delivery", err, status)
	}
}
func mustPublic(t *testing.T, raw []byte) *ecdh.PublicKey {
	t.Helper()
	key, err := ecdh.P256().NewPublicKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
func verifyVAPID(key ecdsa.PublicKey, digest, sig []byte) bool {
	return ecdsa.Verify(&key, digest, new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
}
