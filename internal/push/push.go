// Package push encrypts subscriptions at rest and sends standards-compliant
// aes128gcm Web Push messages. No endpoint, key or provider response is logged.
package push

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/hkdf"

	"github.com/Mino829/trace_Backend/internal/config"
	"github.com/Mino829/trace_Backend/internal/store"
)

const Payload = `{"title":"写真を撮る時間です","body":"次の思い出を残しませんか？","url":"/photo"}`

var encoding = base64.RawURLEncoding
var ErrInvalidConfig = errors.New("invalid Web Push configuration")

type Service struct {
	key     [32]byte
	vapid   *ecdsa.PrivateKey
	public  string
	subject string
	client  *http.Client
}

func New(cfg config.Config) (*Service, error) {
	key, err := encoding.DecodeString(cfg.PushEncryptionKey)
	if err != nil || len(key) != 32 {
		return nil, ErrInvalidConfig
	}
	private, err := encoding.DecodeString(cfg.PushVAPIDPrivateKey)
	if err != nil || len(private) != 32 || subtle.ConstantTimeCompare(key, private) == 1 {
		return nil, ErrInvalidConfig
	}
	public, err := encoding.DecodeString(cfg.PushVAPIDPublicKey)
	if err != nil || len(public) != 65 || public[0] != 4 {
		return nil, ErrInvalidConfig
	}
	curve := elliptic.P256()
	x, y := elliptic.Unmarshal(curve, public)
	if x == nil {
		return nil, ErrInvalidConfig
	}
	d := new(big.Int).SetBytes(private)
	if d.Sign() == 0 || d.Cmp(curve.Params().N) >= 0 {
		return nil, ErrInvalidConfig
	}
	px, py := curve.ScalarBaseMult(private)
	if px.Cmp(x) != 0 || py.Cmp(y) != 0 || !strings.HasPrefix(cfg.PushVAPIDSubject, "mailto:") ||
		!strings.Contains(strings.TrimPrefix(cfg.PushVAPIDSubject, "mailto:"), "@") {
		return nil, ErrInvalidConfig
	}
	s := &Service{vapid: &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y}, D: d},
		public: cfg.PushVAPIDPublicKey, subject: cfg.PushVAPIDSubject,
		client: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	copy(s.key[:], key)
	return s, nil
}

// AllowedEndpoint restricts network access to established browser push providers.
// Caller-controlled URLs must never turn the delivery worker into an SSRF proxy.
func AllowedEndpoint(endpoint string) bool {
	if len(endpoint) > 2048 {
		return false
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.RawQuery == "" && u.Path == "" || u.Port() != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, suffix := range []string{"fcm.googleapis.com", "updates.push.services.mozilla.com", "push.services.mozilla.com", "notify.windows.com", "push.apple.com"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

func DecodeKeys(p256dh, auth string) error {
	pub, err := encoding.DecodeString(p256dh)
	if err != nil || len(pub) != 65 {
		return errors.New("invalid p256dh")
	}
	if _, err = ecdh.P256().NewPublicKey(pub); err != nil {
		return errors.New("invalid p256dh")
	}
	secret, err := encoding.DecodeString(auth)
	if err != nil || len(secret) != 16 {
		return errors.New("invalid auth")
	}
	return nil
}

// Seal uses AES-256-GCM, fresh nonces and a session-bound AAD. VAPID keys are
// never used as the at-rest encryption key.
func (s *Service) Seal(id string, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	out := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plaintext)+aead.Overhead())
	if _, err = rand.Read(out); err != nil {
		return nil, err
	}
	out = aead.Seal(out, out[:aead.NonceSize()], plaintext, []byte(id))
	return out, nil
}
func (s *Service) Open(id string, data []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(data) < aead.NonceSize()+aead.Overhead() {
		return nil, errors.New("invalid encrypted subscription")
	}
	return aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], []byte(id))
}

type Keys struct {
	P256DH string `json:"p256dh"`
	Auth   string `json:"auth"`
}

func (s *Service) Send(ctx context.Context, p store.PushSubscription, now time.Time) (int, error) {
	endpoint, err := s.Open(p.MemorySessionID, p.EndpointEncrypted)
	if err != nil {
		return 0, err
	}
	keysJSON, err := s.Open(p.MemorySessionID, p.KeysEncrypted)
	if err != nil {
		return 0, err
	}
	var keys Keys
	if err = json.Unmarshal(keysJSON, &keys); err != nil {
		return 0, err
	}
	if !AllowedEndpoint(string(endpoint)) || DecodeKeys(keys.P256DH, keys.Auth) != nil {
		return 0, errors.New("invalid stored subscription")
	}
	ciphertext, err := encryptPayload([]byte(Payload), keys)
	if err != nil {
		return 0, err
	}
	target, _ := url.Parse(string(endpoint))
	audience := target.Scheme + "://" + target.Host
	jwt, err := s.token(audience, now)
	if err != nil {
		return 0, err
	}
	if !p.ExpiresAt.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, p.ExpiresAt)
		defer cancel()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, string(endpoint), bytes.NewReader(ciphertext))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Encoding", "aes128gcm")
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("TTL", "60")
	request.Header.Set("Authorization", "vapid t="+jwt+", k="+s.public)
	response, err := s.client.Do(request)
	if err != nil {
		return 0, errors.New("push provider unavailable")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
	return response.StatusCode, nil
}

func (s *Service) token(audience string, now time.Time) (string, error) {
	header := encoding.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, _ := json.Marshal(struct {
		Audience string `json:"aud"`
		Expiry   int64  `json:"exp"`
		Subject  string `json:"sub"`
	}{audience, now.Add(12 * time.Hour).Unix(), s.subject})
	message := header + "." + encoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(message))
	r, ss, err := ecdsa.Sign(rand.Reader, s.vapid, digest[:])
	if err != nil {
		return "", err
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	ss.FillBytes(signature[32:])
	return message + "." + encoding.EncodeToString(signature), nil
}

// RFC 8291 / RFC 8188: one aes128gcm record, with final-record delimiter 0x02.
func encryptPayload(payload []byte, keys Keys) ([]byte, error) {
	subscriber, _ := encoding.DecodeString(keys.P256DH)
	auth, _ := encoding.DecodeString(keys.Auth)
	clientKey, err := ecdh.P256().NewPublicKey(subscriber)
	if err != nil {
		return nil, err
	}
	ephemeral, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	serverKey := ephemeral.PublicKey().Bytes()
	shared, err := ephemeral.ECDH(clientKey)
	if err != nil {
		return nil, err
	}
	info := make([]byte, 0, len("WebPush: info\x00")+130)
	info = append(info, []byte("WebPush: info\x00")...)
	info = append(info, subscriber...)
	info = append(info, serverKey...)
	ikm := make([]byte, 32)
	if _, err = io.ReadFull(hkdf.Expand(sha256.New, hkdf.Extract(sha256.New, shared, auth), info), ikm); err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err = rand.Read(salt); err != nil {
		return nil, err
	}
	prk := hkdf.Extract(sha256.New, ikm, salt)
	cek := make([]byte, 16)
	nonce := make([]byte, 12)
	if _, err = io.ReadFull(hkdf.Expand(sha256.New, prk, []byte("Content-Encoding: aes128gcm\x00")), cek); err != nil {
		return nil, err
	}
	if _, err = io.ReadFull(hkdf.Expand(sha256.New, prk, []byte("Content-Encoding: nonce\x00")), nonce); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	record := append(append([]byte{}, payload...), 0x02)
	if len(record)+aead.Overhead() > 4096 {
		return nil, errors.New("push payload too large")
	}
	body := make([]byte, 0, 16+4+1+len(serverKey)+len(record)+aead.Overhead())
	body = append(body, salt...)
	size := make([]byte, 4)
	binary.BigEndian.PutUint32(size, 4096)
	body = append(body, size...)
	body = append(body, byte(len(serverKey)))
	body = append(body, serverKey...)
	return aead.Seal(body, nonce, record, nil), nil
}
