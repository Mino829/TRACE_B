package photoasset

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iamcredentials/v1"
	"google.golang.org/api/option"
	rawstorage "google.golang.org/api/storage/v1"
	htransport "google.golang.org/api/transport/http"
)

const privateCacheControl = "private, no-store, max-age=0"
const maxURLLifetime = 5 * time.Minute

// GCS uses application default credentials for object operations and IAM
// signBlob. It never loads a signing key or falls back to local storage.
// Bucket policy must remain unchanged while this adapter is running.
type GCS struct {
	client      *storage.Client
	bucket      *storage.BucketHandle
	objects     *rawstorage.Service
	iam         *iamcredentials.Service
	httpClient  *http.Client
	signerEmail string
}

var _ Storage = (*GCS)(nil)

func NewGCS(ctx context.Context, bucket, signerEmail string) (*GCS, error) {
	if !validBucketName(bucket) || !validSignerEmail(signerEmail) || os.Getenv("STORAGE_EMULATOR_HOST") != "" {
		return nil, errors.New("invalid private photo storage configuration")
	}
	// One ADC-authenticated HTTP client is shared by all official clients. A
	// discard logger also keeps SDK debug logging from exposing request bodies,
	// object names, credentials, or signed capabilities through a global logger.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	httpClient, _, err := htransport.NewClient(ctx, option.WithScopes(iamcredentials.CloudPlatformScope), option.WithLogger(logger))
	if err != nil {
		return nil, cloudError("authentication", err)
	}
	opts := []option.ClientOption{option.WithHTTPClient(httpClient), option.WithLogger(logger)}
	// JSON media downloads reject the explicit Accept-Encoding needed for
	// byte-preserving reads. XML reads support it without transcoding input.
	client, err := storage.NewClient(ctx, append(opts, storage.WithXMLReads(), storage.WithDisabledClientMetrics())...)
	if err != nil {
		httpClient.CloseIdleConnections()
		return nil, cloudError("initialization", err)
	}
	closeOnError := func() {
		client.Close()
		httpClient.CloseIdleConnections()
	}
	bucketHandle := client.Bucket(bucket)
	attrs, err := bucketHandle.Attrs(ctx)
	if err != nil {
		closeOnError()
		return nil, cloudError("bucket inspection", err)
	}
	if err := safeBucket(attrs); err != nil {
		closeOnError()
		return nil, err
	}
	iam, err := iamcredentials.NewService(ctx, opts...)
	if err != nil {
		closeOnError()
		return nil, cloudError("signer initialization", err)
	}
	objects, err := rawstorage.NewService(ctx, opts...)
	if err != nil {
		closeOnError()
		return nil, cloudError("copy initialization", err)
	}
	return &GCS{client: client, bucket: bucketHandle, objects: objects, iam: iam,
		httpClient: httpClient, signerEmail: signerEmail}, nil
}

func (g *GCS) Close() error {
	err := g.client.Close()
	g.httpClient.CloseIdleConnections()
	if err != nil {
		return cloudError("close", err)
	}
	return nil
}

func safeBucket(attrs *storage.BucketAttrs) error {
	if attrs == nil || !strings.EqualFold(attrs.Location, "ASIA-NORTHEAST1") ||
		!attrs.UniformBucketLevelAccess.Enabled || attrs.PublicAccessPrevention != storage.PublicAccessPreventionEnforced ||
		attrs.VersioningEnabled || attrs.RetentionPolicy != nil || attrs.DefaultEventBasedHold || attrs.ObjectRetentionMode != "" || len(attrs.Lifecycle.Rules) != 0 ||
		(attrs.SoftDeletePolicy != nil && attrs.SoftDeletePolicy.RetentionDuration != 0) {
		return errors.New("private photo bucket does not meet privacy and deletion requirements")
	}
	// GCS omits softDeletePolicy entirely for older buckets without soft delete;
	// nil and an explicit zero duration both mean disabled, not the new-bucket
	// default (which the Attrs response reports as a nonzero duration).
	return nil
}

func (g *GCS) UploadURL(ctx context.Context, key string, meta Metadata, expires time.Time) (string, map[string]string, error) {
	if !validObjectKey(key) || !validMetadata(meta) {
		return "", nil, ErrInvalidImage
	}
	length := strconv.FormatInt(meta.SizeBytes, 10)
	headers := map[string]string{
		"Content-Type":               meta.MIMEType,
		"Content-Length":             length,
		"x-goog-if-generation-match": "0",
		"Cache-Control":              privateCacheControl,
	}
	// A browser sets Content-Length from the Blob automatically; callers must
	// send the remaining returned headers unchanged. Signing the precondition
	// makes an old capability unusable once the stage is occupied or fenced.
	signed, err := g.signedURL(ctx, key, expires, &storage.SignedURLOptions{
		Method: http.MethodPut, ContentType: meta.MIMEType,
		Headers: []string{"content-length:" + length, "x-goog-if-generation-match:0", "cache-control:" + privateCacheControl},
	})
	if err != nil {
		return "", nil, err
	}
	return signed, headers, nil
}

// Read bounds the encoded bytes, even if the peer lies about object length.
// maxBytes is a caller limit, never a generation selector, and cannot exceed
// MaxBytes. The returned generation is used by Promote to avoid a read/copy race.
func (g *GCS) Read(ctx context.Context, key string, maxBytes int64) (Object, error) {
	if !validObjectKey(key) || maxBytes <= 0 || maxBytes > MaxBytes {
		return Object{}, ErrInvalidImage
	}
	reader, err := g.bucket.Object(key).ReadCompressed(true).NewReader(ctx)
	if err != nil {
		return Object{}, cloudError("read", err)
	}
	defer reader.Close()
	if reader.Attrs.Size < 0 || reader.Attrs.Size > maxBytes || reader.Attrs.ContentEncoding != "" || reader.Attrs.Decompressed || reader.Attrs.Generation <= 0 {
		return Object{}, ErrInvalidImage
	}
	// One extra byte detects overflow without allocating an unbounded buffer.
	// ReadFull must encounter EOF after the declared size, which also lets the
	// official reader finish its checksum verification before we accept bytes.
	data := make([]byte, int(reader.Attrs.Size)+1)
	n, err := io.ReadFull(reader, data)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return Object{}, cloudError("read", err)
	}
	if int64(n) != reader.Attrs.Size {
		return Object{}, ErrInvalidImage
	}
	return Object{Data: data[:n], Generation: reader.Attrs.Generation, ContentType: reader.Attrs.ContentType}, nil
}

func (g *GCS) Promote(ctx context.Context, source, destination string, generation int64, meta Metadata) error {
	if !validObjectKey(source) || !validObjectKey(destination) || source == destination || generation <= 0 || !validMetadata(meta) {
		return ErrInvalidImage
	}
	// The high-level Copier omits zero-valued attributes. The official JSON
	// client exposes explicit empty metadata and context dropping, so neither
	// client-supplied metadata nor encoding/disposition survives promotion.
	attrs := &rawstorage.Object{
		ContentType: meta.MIMEType, CacheControl: privateCacheControl,
		ContentDisposition: "inline", Metadata: map[string]string{},
		ForceSendFields: []string{"Metadata"},
		NullFields:      []string{"ContentEncoding", "ContentLanguage", "CustomTime"},
	}
	call := g.objects.Objects.Rewrite(g.bucket.BucketName(), source, g.bucket.BucketName(), destination, attrs).
		SourceGeneration(generation).IfSourceGenerationMatch(generation).IfGenerationMatch(0).DropContextGroups("custom").Context(ctx)
	for {
		result, err := call.Do()
		if err != nil {
			var apiErr *googleapi.Error
			if errors.As(err, &apiErr) && (apiErr.Code == http.StatusPreconditionFailed || apiErr.Code == http.StatusConflict) {
				return ErrAlreadyExists
			}
			return cloudError("promotion", err)
		}
		if result.Done {
			if result.Resource == nil || result.Resource.Generation <= 0 || result.ObjectSize != meta.SizeBytes {
				return errors.New("private photo copy returned invalid object attributes")
			}
			return nil
		}
		if result.RewriteToken == "" {
			return errors.New("private photo copy did not finish")
		}
		call.RewriteToken(result.RewriteToken)
	}
}

// Fence must overwrite, not delete: a live zero-byte object permanently blocks
// generation-match:0 PUT capabilities until the service removes it after expiry.
// With versioning and soft delete disabled, the replaced photo is not retained.
func (g *GCS) Fence(ctx context.Context, key string) error {
	if !validObjectKey(key) {
		return errors.New("invalid photo object reference")
	}
	writer := g.bucket.Object(key).NewWriter(ctx)
	writer.ChunkSize = 0
	writer.ContentType = "application/octet-stream"
	writer.CacheControl = privateCacheControl
	writer.ContentDisposition = "attachment"
	if err := writer.Close(); err != nil {
		return cloudError("fencing", err)
	}
	return nil
}

func (g *GCS) Delete(ctx context.Context, key string) error {
	if !validObjectKey(key) {
		return errors.New("invalid photo object reference")
	}
	if err := g.bucket.Object(key).Delete(ctx); err != nil {
		if redacted := cloudError("deletion", err); redacted != ErrNotFound {
			return redacted
		}
	}
	return nil
}

func (g *GCS) ViewURL(ctx context.Context, key string, expires time.Time) (string, error) {
	if !validObjectKey(key) {
		return "", errors.New("invalid photo object reference")
	}
	// GCS XML downloads use stored Cache-Control metadata; only disposition
	// and content type have supported response-override query parameters.
	return g.signedURL(ctx, key, expires, &storage.SignedURLOptions{
		Method: http.MethodGet,
		QueryParameters: url.Values{
			"response-content-disposition": {"inline"},
		},
	})
}

func (g *GCS) signedURL(ctx context.Context, key string, expires time.Time, opts *storage.SignedURLOptions) (string, error) {
	now := time.Now()
	if !expires.After(now) || expires.After(now.Add(maxURLLifetime)) {
		return "", errors.New("photo URL expiration must be within five minutes")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	opts.GoogleAccessID = g.signerEmail
	opts.Scheme = storage.SigningSchemeV4
	opts.Expires = expires
	opts.SignBytes = func(payload []byte) ([]byte, error) {
		result, err := g.iam.Projects.ServiceAccounts.SignBlob("projects/-/serviceAccounts/"+g.signerEmail,
			&iamcredentials.SignBlobRequest{Payload: base64.StdEncoding.EncodeToString(payload)}).Context(ctx).Do()
		if err != nil {
			return nil, cloudError("signing", err)
		}
		signature, err := base64.StdEncoding.DecodeString(result.SignedBlob)
		if err != nil || len(signature) == 0 {
			return nil, errors.New("photo URL signing returned an invalid signature")
		}
		return signature, nil
	}
	signed, err := storage.SignedURL(g.bucket.BucketName(), key, opts)
	if err != nil {
		return "", cloudError("signing", err)
	}
	return signed, nil
}

func cloudError(operation string, err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, storage.ErrObjectNotExist) {
		return ErrNotFound
	}
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) && apiErr.Code == http.StatusNotFound {
		return ErrNotFound
	}
	// Never wrap cloud errors: they can contain request URLs, keys, credentials,
	// payloads, and response bodies. Only fixed local operation names enter here.
	return errors.New("private photo storage " + operation + " failed")
}

func validObjectKey(key string) bool {
	return key != "" && len(key) <= 1024 && utf8.ValidString(key) && !strings.ContainsAny(key, "\r\n\x00")
}

func validBucketName(bucket string) bool {
	if len(bucket) < 3 || len(bucket) > 222 || strings.ContainsAny(bucket, "/\\:") {
		return false
	}
	for _, label := range strings.Split(bucket, ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		for i, char := range label {
			if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
				continue
			}
			if (char != '-' && char != '_') || i == 0 || i == len(label)-1 {
				return false
			}
		}
	}
	return true
}

func validSignerEmail(email string) bool {
	if len(email) > 254 || !strings.HasSuffix(email, ".gserviceaccount.com") || strings.Count(email, "@") != 1 {
		return false
	}
	for i, char := range email {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-' || char == '.' || char == '_' {
			continue
		}
		if char != '@' || i == 0 {
			return false
		}
	}
	return true
}
