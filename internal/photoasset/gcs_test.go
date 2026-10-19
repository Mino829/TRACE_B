package photoasset

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iamcredentials/v1"
	"google.golang.org/api/option"
	rawstorage "google.golang.org/api/storage/v1"
)

// These fixtures drive the official clients over HTTP. They model the relevant
// service preconditions and signing operation, not a second Storage adapter.
func fixtureGCS(t *testing.T, handler http.HandlerFunc) *GCS {
	t.Helper()
	t.Setenv("STORAGE_EMULATOR_HOST", "")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := storage.NewClient(context.Background(), option.WithHTTPClient(server.Client()),
		option.WithEndpoint(server.URL+"/storage/v1/"), storage.WithXMLReads(), storage.WithDisabledClientMetrics())
	if err != nil {
		t.Fatal(err)
	}
	objects, err := rawstorage.NewService(context.Background(), option.WithHTTPClient(server.Client()), option.WithEndpoint(server.URL+"/storage/v1/"))
	if err != nil {
		t.Fatal(err)
	}
	iam, err := iamcredentials.NewService(context.Background(), option.WithHTTPClient(server.Client()), option.WithEndpoint(server.URL+"/"))
	if err != nil {
		t.Fatal(err)
	}
	g := &GCS{client: client, bucket: client.Bucket("test-private-bucket"), objects: objects, iam: iam,
		httpClient: server.Client(), signerEmail: "photo-signer@test-project.iam.gserviceaccount.com"}
	t.Cleanup(func() { g.Close() })
	return g
}

func TestGCSBucketPrivacyGuards(t *testing.T) {
	safe := func() *storage.BucketAttrs {
		return &storage.BucketAttrs{Location: "ASIA-NORTHEAST1", UniformBucketLevelAccess: storage.UniformBucketLevelAccess{Enabled: true},
			PublicAccessPrevention: storage.PublicAccessPreventionEnforced, SoftDeletePolicy: &storage.SoftDeletePolicy{}}
	}
	if err := safeBucket(safe()); err != nil {
		t.Fatal(err)
	}
	withoutPolicy := safe()
	withoutPolicy.SoftDeletePolicy = nil
	if err := safeBucket(withoutPolicy); err != nil {
		t.Fatal("disabled soft delete omitted from old bucket response must be accepted")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*storage.BucketAttrs)
	}{
		{"wrong region", func(a *storage.BucketAttrs) { a.Location = "ASIA" }},
		{"object ACLs", func(a *storage.BucketAttrs) { a.UniformBucketLevelAccess.Enabled = false }},
		{"inherited public prevention", func(a *storage.BucketAttrs) { a.PublicAccessPrevention = storage.PublicAccessPreventionInherited }},
		{"versioning", func(a *storage.BucketAttrs) { a.VersioningEnabled = true }},
		{"retention", func(a *storage.BucketAttrs) { a.RetentionPolicy = &storage.RetentionPolicy{RetentionPeriod: time.Hour} }},
		{"zero retention policy still present", func(a *storage.BucketAttrs) { a.RetentionPolicy = &storage.RetentionPolicy{} }},
		{"soft delete", func(a *storage.BucketAttrs) { a.SoftDeletePolicy.RetentionDuration = 7 * 24 * time.Hour }},
		{"default hold", func(a *storage.BucketAttrs) { a.DefaultEventBasedHold = true }},
		{"object retention", func(a *storage.BucketAttrs) { a.ObjectRetentionMode = "Enabled" }},
		{"lifecycle can remove upload fences", func(a *storage.BucketAttrs) {
			a.Lifecycle.Rules = []storage.LifecycleRule{{Action: storage.LifecycleAction{Type: "Delete"}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attrs := safe()
			tc.mutate(attrs)
			if err := safeBucket(attrs); err == nil {
				t.Fatal("unsafe bucket accepted")
			}
		})
	}
	if err := safeBucket(nil); err == nil {
		t.Fatal("absent bucket attributes accepted")
	}
	for _, config := range [][2]string{{"https://bucket.invalid", "signer@project.iam.gserviceaccount.com"}, {"private-bucket", "not-a-service-account"}} {
		if _, err := NewGCS(context.Background(), config[0], config[1]); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
	t.Setenv("STORAGE_EMULATOR_HOST", "http://localhost:12345")
	if _, err := NewGCS(context.Background(), "private-bucket", "signer@project.iam.gserviceaccount.com"); err == nil {
		t.Fatal("production constructor accepted storage emulator")
	}
}

func TestGCSSignedURLConstraintsAndIAMSigning(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	payloads := make(chan []byte, 2)
	g := fixtureGCS(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, ":signBlob") {
			http.Error(w, "unsupported operation", http.StatusBadRequest)
			return
		}
		var request iamcredentials.SignBlobRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error("malformed signing request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		payload, err := base64.StdEncoding.DecodeString(request.Payload)
		if err != nil {
			t.Error("signing payload is not base64")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		digest := sha256.Sum256(payload)
		signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
		if err != nil {
			t.Error("fixture signing failed")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		payloads <- payload
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(iamcredentials.SignBlobResponse{SignedBlob: base64.StdEncoding.EncodeToString(signature)})
	})
	meta := imageMetadata([]byte("encoded"), "image/jpeg", 1, 1)
	signed, headers, err := g.UploadURL(context.Background(), "opaque-stage-key", meta, time.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(signed)
	if err != nil {
		t.Fatal("invalid signed URL")
	}
	query := parsed.Query()
	if parsed.Scheme != "https" || parsed.Host != "storage.googleapis.com" || query.Get("X-Goog-Algorithm") != "GOOG4-RSA-SHA256" ||
		query.Get("X-Goog-SignedHeaders") != "cache-control;content-length;content-type;host;x-goog-if-generation-match" {
		t.Fatal("PUT URL does not pin required headers with V4 over HTTPS")
	}
	if headers["x-goog-if-generation-match"] != "0" || headers["Content-Length"] != "7" || headers["Content-Type"] != "image/jpeg" ||
		headers["Cache-Control"] != privateCacheControl {
		t.Fatal("upload headers do not constrain upload")
	}
	verifyURLSignature(t, query, <-payloads, &privateKey.PublicKey)
	view, err := g.ViewURL(context.Background(), "opaque-final-key", time.Now().Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err = url.Parse(view)
	if err != nil {
		t.Fatal("invalid view URL")
	}
	if parsed.Query().Get("response-content-disposition") != "inline" || parsed.Query().Get("X-Goog-SignedHeaders") != "host" {
		t.Fatal("view URL has incorrect disposition or unexpected required headers")
	}
	verifyURLSignature(t, parsed.Query(), <-payloads, &privateKey.PublicKey)
}

func verifyURLSignature(t *testing.T, query url.Values, payload []byte, publicKey *rsa.PublicKey) {
	t.Helper()
	seconds, err := strconv.Atoi(query.Get("X-Goog-Expires"))
	if err != nil || seconds <= 0 || seconds > 300 {
		t.Fatal("URL lifetime not bounded to five minutes")
	}
	signature, err := hex.DecodeString(query.Get("X-Goog-Signature"))
	if err != nil {
		t.Fatal("URL signature is not hexadecimal")
	}
	digest := sha256.Sum256(payload)
	if err := rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatal("URL does not use the actual IAM signature")
	}
}

func TestGCSSigningRejectsUnsafeInputs(t *testing.T) {
	g := fixtureGCS(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid input performed cloud I/O")
		w.WriteHeader(http.StatusBadRequest)
	})
	meta := imageMetadata([]byte("encoded"), "image/jpeg", 1, 1)
	for _, expires := range []time.Time{time.Now().Add(-time.Second), time.Now().Add(6 * time.Minute)} {
		if _, _, err := g.UploadURL(context.Background(), "stage", meta, expires); err == nil {
			t.Fatal("invalid PUT expiration accepted")
		}
		if _, err := g.ViewURL(context.Background(), "final", expires); err == nil {
			t.Fatal("invalid GET expiration accepted")
		}
	}
	meta.SizeBytes = MaxBytes + 1
	if _, _, err := g.UploadURL(context.Background(), "stage", meta, time.Now().Add(time.Minute)); !errors.Is(err, ErrInvalidImage) {
		t.Fatal("oversized PUT accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.ViewURL(ctx, "final", time.Now().Add(time.Minute)); !errors.Is(err, context.Canceled) {
		t.Fatal("signing did not honor cancellation")
	}
	for _, key := range []string{"", "secret\nheader", "secret\x00key", string([]byte{0xff})} {
		if _, err := g.ViewURL(context.Background(), key, time.Now().Add(time.Minute)); err == nil {
			t.Fatal("invalid object reference accepted")
		}
	}
}

func TestGCSPromoteGenerationAndNoOverwrite(t *testing.T) {
	var mu sync.Mutex
	currentGeneration := int64(41)
	destinationExists := false
	pending := false
	meta := imageMetadata([]byte("encoded"), "image/jpeg", 1, 1)
	g := fixtureGCS(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		query := r.URL.Query()
		if r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "/rewriteTo/") ||
			query.Get("ifGenerationMatch") != "0" || query.Get("ifSourceGenerationMatch") != "41" ||
			query.Get("sourceGeneration") != "41" || query.Get("dropContextGroups") != "custom" {
			t.Error("copy did not pin source and protect destination")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var attrs map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&attrs); err != nil || string(attrs["metadata"]) != "{}" ||
			string(attrs["contentEncoding"]) != "null" || string(attrs["contentLanguage"]) != "null" || string(attrs["customTime"]) != "null" ||
			string(attrs["cacheControl"]) != strconv.Quote(privateCacheControl) || string(attrs["contentType"]) != strconv.Quote("image/jpeg") {
			t.Error("copy preserved untrusted object metadata")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if currentGeneration != 41 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if destinationExists {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if !pending {
			pending = true
			json.NewEncoder(w).Encode(rawstorage.RewriteResponse{RewriteToken: "continue-copy", ObjectSize: meta.SizeBytes})
			return
		}
		if query.Get("rewriteToken") != "continue-copy" {
			t.Error("copy did not continue rewrite")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		destinationExists = true
		json.NewEncoder(w).Encode(rawstorage.RewriteResponse{Done: true, ObjectSize: meta.SizeBytes,
			Resource: &rawstorage.Object{Generation: 42, Size: uint64(meta.SizeBytes), ContentType: "image/jpeg"}})
	})
	if err := g.Promote(context.Background(), "stage", "final", 41, meta); err != nil {
		t.Fatal(err)
	}
	if err := g.Promote(context.Background(), "stage", "final", 41, meta); !errors.Is(err, ErrAlreadyExists) {
		t.Fatal("existing destination was not protected")
	}
	mu.Lock()
	currentGeneration, destinationExists = 43, false
	mu.Unlock()
	if err := g.Promote(context.Background(), "stage", "final", 41, meta); !errors.Is(err, ErrNotFound) {
		t.Fatal("copy accepted a stage replaced after validation")
	}
	if err := g.Promote(context.Background(), "stage", "stage", 41, meta); !errors.Is(err, ErrInvalidImage) {
		t.Fatal("same-key promotion accepted")
	}
}

func TestGCSReadBoundsAndIntegrity(t *testing.T) {
	data := []byte("encoded")
	for _, tc := range []struct {
		name               string
		size, limit        int64
		encoding, checksum string
		wantError          bool
	}{
		{"valid generation", 7, MaxBytes, "", "valid", false},
		{"caller bound", 7, 6, "", "valid", true},
		{"hard bound", MaxBytes + 1, MaxBytes, "", "", true},
		{"encoded object", 7, MaxBytes, "gzip", "", true},
		{"checksum failure", 7, MaxBytes, "", "invalid", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := fixtureGCS(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.Header.Get("Accept-Encoding") != "gzip" {
					t.Error("read may transparently transcode uploaded bytes")
				}
				w.Header().Set("Content-Length", strconv.FormatInt(tc.size, 10))
				w.Header().Set("Content-Type", "image/jpeg")
				w.Header().Set("X-Goog-Generation", "41")
				if tc.encoding != "" {
					w.Header().Set("Content-Encoding", tc.encoding)
				}
				if tc.checksum != "" {
					checksum := crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))
					if tc.checksum == "invalid" {
						checksum++
					}
					var hash [4]byte
					binary.BigEndian.PutUint32(hash[:], checksum)
					w.Header().Set("X-Goog-Hash", "crc32c="+base64.StdEncoding.EncodeToString(hash[:]))
				}
				w.WriteHeader(http.StatusOK)
				if tc.size <= MaxBytes {
					w.Write(data)
				}
			})
			object, err := g.Read(context.Background(), "stage", tc.limit)
			if tc.wantError {
				if err == nil || len(object.Data) != 0 {
					t.Fatal("unsafe object read returned bytes")
				}
			} else if err != nil || !bytes.Equal(object.Data, data) || object.Generation != 41 || object.ContentType != "image/jpeg" {
				t.Fatalf("read did not return exact bytes, type, and generation: object=%+v err=%v", object, err)
			}
		})
	}
}

func TestGCSFenceZeroBytesAndIdempotentDelete(t *testing.T) {
	var mu sync.Mutex
	data := []byte("private photo bytes")
	exists := true
	g := fixtureGCS(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mediaType != "multipart/related" || r.URL.Query().Get("uploadType") != "multipart" {
				t.Error("zero-byte fence is not a simple object upload")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			parts := multipart.NewReader(r.Body, params["boundary"])
			metadata, err := parts.NextPart()
			if err != nil {
				t.Error("missing fence metadata")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var attrs rawstorage.Object
			if err := json.NewDecoder(metadata).Decode(&attrs); err != nil || attrs.CacheControl != privateCacheControl ||
				attrs.ContentType != "application/octet-stream" || len(attrs.Metadata) != 0 {
				t.Error("unsafe fence object metadata")
			}
			photo, err := parts.NextPart()
			if err != nil {
				t.Error("missing zero-byte fence body")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			body, err := io.ReadAll(photo)
			if err != nil || len(body) != 0 {
				t.Error("fence retained photo bytes")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			data, exists = body, true
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(rawstorage.Object{Generation: 42, ContentType: attrs.ContentType})
		case http.MethodDelete:
			if !exists {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			data, exists = nil, false
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	for range 2 {
		if err := g.Fence(context.Background(), "stage"); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	fenced := exists && len(data) == 0
	mu.Unlock()
	if !fenced {
		t.Fatal("fence did not replace photo with a live empty object")
	}
	for range 2 {
		if err := g.Delete(context.Background(), "stage"); err != nil {
			t.Fatal("delete was not idempotent:", err)
		}
	}
}

func TestGCSErrorsAreRedacted(t *testing.T) {
	secret := "secret-object-key signed-url credential photo-hash"
	for _, source := range []error{
		&googleapi.Error{Code: http.StatusForbidden, Message: secret, Body: secret},
		&url.Error{Op: "Get", URL: "https://example.invalid/" + secret, Err: errors.New(secret)},
		errors.New(secret),
	} {
		err := cloudError("read", source)
		if strings.Contains(err.Error(), "secret") || errors.Is(err, source) {
			t.Fatal("cloud error leaked sensitive detail")
		}
	}
	g := fixtureGCS(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": 403, "message": secret}})
	})
	if _, err := g.ViewURL(context.Background(), "private-key", time.Now().Add(time.Minute)); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("IAM signing failure was not redacted")
	}
	if cloudError("read", storage.ErrObjectNotExist) != ErrNotFound ||
		cloudError("read", &googleapi.Error{Code: 404}) != ErrNotFound ||
		cloudError("read", context.Canceled) != context.Canceled ||
		cloudError("read", context.DeadlineExceeded) != context.DeadlineExceeded {
		t.Fatal("safe error semantics were lost")
	}
}
