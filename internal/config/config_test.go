package config

import (
	"net/url"
	"strings"
	"testing"
)

func baseEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"PORT", "TRACE_ADDRESS", "TRACE_DATABASE_PATH", "DB_HOST", "DB_USER", "DB_NAME", "DB_PASSWORD", "TRACE_DB_MAX_CONNECTIONS", "TRACE_MIN_LATITUDE", "TRACE_LIVE_POSITION_TTL", "TRACE_SESSION_TTL", "TRACE_MEMORY_SESSION_TTL", "TRACE_LIVE_SESSION_TTL", "TRACE_DATA_RETENTION", "TRACE_EDGE_SHARED_SECRET", "TRACE_REGISTRATION_OPEN", "TRACE_MEMORY_CONSENT_VERSION", "TRACE_CONSENT_BODY"} {
		t.Setenv(k, "")
	}
	for _, key := range []string{"TRACE_PHOTO_ENABLED", "TRACE_PHOTO_METADATA_ENABLED", "TRACE_PHOTO_BUCKET", "TRACE_PHOTO_SIGNER_EMAIL",
		"TRACE_PUSH_ENABLED", "TRACE_PUSH_VAPID_PUBLIC_KEY", "TRACE_PUSH_VAPID_PRIVATE_KEY", "TRACE_PUSH_VAPID_SUBJECT", "TRACE_PUSH_ENCRYPTION_KEY",
		"TRACE_ARRIVAL_ENABLED", "TRACE_ARRIVAL_ISSUER_KEY", "TRACE_ARRIVAL_PROOF_KEY", "TRACE_ARRIVAL_VENUE", "TRACE_ARRIVAL_CHECKPOINT_ID",
		"TRACE_DISPLAY_ENABLED", "TRACE_DISPLAY_GOOGLE_CLIENT_ID", "TRACE_DISPLAY_OPERATOR_EMAILS", "TRACE_DISPLAY_ALLOWED_ORIGINS", "TRACE_DISPLAY_TOKEN_KEY"} {
		t.Setenv(key, "")
	}
	for _, key := range []string{"TRACE_COORDINATE_VERSION", "TRACE_CAMPUS_GRAPH_VERSION", "TRACE_THREE_D_MAP_VERSION"} {
		t.Setenv(key, "")
	}
	t.Setenv("DATABASE_URL", "postgresql://local:local@localhost/trace?sslmode=disable")
}
func TestCloudRunPortAndPool(t *testing.T) {
	baseEnv(t)
	t.Setenv("PORT", "9090")
	t.Setenv("TRACE_DB_MAX_CONNECTIONS", "7")
	cfg, err := FromEnv()
	if err != nil || cfg.Address != ":9090" || cfg.DatabaseMaxConnections != 7 {
		t.Fatalf("port/pool: %v", err)
	}
	t.Setenv("TRACE_ADDRESS", ":8081")
	cfg, err = FromEnv()
	if err != nil || cfg.Address != ":8081" {
		t.Fatal("explicit address must take precedence")
	}
}
func TestCloudSQLSocketAndPasswordEncoding(t *testing.T) {
	baseEnv(t)
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DB_HOST", "/cloudsql/project:asia-northeast1:trace")
	t.Setenv("DB_NAME", "trace")
	t.Setenv("DB_USER", "trace_app")
	t.Setenv("DB_PASSWORD", "p@ss:/ ?#&")
	t.Setenv("DB_SSLMODE", "disable")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := u.User.Password()
	if password != "p@ss:/ ?#&" || u.Query().Get("host") != "/cloudsql/project:asia-northeast1:trace" {
		t.Fatal("connection settings not round-tripped")
	}
}

func TestRetiredGPSConsentVersionCannotBeReused(t *testing.T) {
	baseEnv(t)
	t.Setenv("TRACE_MEMORY_CONSENT_VERSION", "2026-memory-01")
	if _, err := FromEnv(); err == nil {
		t.Fatal("retired GPS consent version was accepted")
	}
}

func TestRejectInvalidConfiguration(t *testing.T) {
	for _, tt := range []struct{ key, value string }{
		{"PORT", "0"}, {"PORT", "abc"}, {"TRACE_DB_MAX_CONNECTIONS", "0"},
		{"TRACE_DATABASE_PATH", "trace.db"}, {"TRACE_MIN_LATITUDE", "NaN"}, {"TRACE_MIN_LATITUDE", "-91"},
		{"TRACE_LIVE_POSITION_TTL", "5m"}, {"TRACE_MEMORY_SESSION_TTL", "0s"},
		{"TRACE_LIVE_SESSION_TTL", "-1h"}, {"TRACE_SESSION_TTL", "6h"}, {"DATABASE_URL", ""},
		{"TRACE_EDGE_SHARED_SECRET", "too-short"},
	} {
		t.Run(tt.key+tt.value, func(t *testing.T) {
			baseEnv(t)
			t.Setenv(tt.key, tt.value)
			if _, err := FromEnv(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}
func TestIndependentSessionDurations(t *testing.T) {
	baseEnv(t)
	t.Setenv("TRACE_MEMORY_SESSION_TTL", "2h")
	t.Setenv("TRACE_LIVE_SESSION_TTL", "45m")
	cfg, err := FromEnv()
	if err != nil || cfg.MemorySessionTTL.Hours() != 2 || cfg.LiveSessionTTL.Minutes() != 45 {
		t.Fatalf("independent session lifetimes: %+v, %v", cfg, err)
	}
}
func TestDatabaseMissingDoesNotExposeSecrets(t *testing.T) {
	baseEnv(t)
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DB_PASSWORD", "do-not-log-this")
	_, err := FromEnv()
	if err == nil || strings.Contains(err.Error(), "do-not-log-this") {
		t.Fatal("unsafe or absent error")
	}
}

func TestPhotoStorageCannotBeEnabledWithoutCompletePrivateConfiguration(t *testing.T) {
	baseEnv(t)
	t.Setenv("TRACE_PHOTO_ENABLED", "true")
	if _, err := FromEnv(); err == nil {
		t.Fatal("enabled photos accepted without storage")
	}
	t.Setenv("TRACE_PHOTO_BUCKET", "trace-private-photos")
	t.Setenv("TRACE_PHOTO_SIGNER_EMAIL", "runtime@example.iam.gserviceaccount.com")
	cfg, err := FromEnv()
	if err != nil || !cfg.PhotoEnabled {
		t.Fatalf("configured photos rejected: %v", err)
	}
	t.Setenv("TRACE_PHOTO_ENABLED", "false")
	cfg, err = FromEnv()
	if err != nil || cfg.PhotoEnabled || cfg.PhotoBucket == "" {
		t.Fatal("disabling new photos must retain storage for deletion")
	}
	t.Setenv("TRACE_PHOTO_SIGNER_EMAIL", "")
	if _, err := FromEnv(); err == nil {
		t.Fatal("incomplete storage configuration accepted")
	}
}

func TestDevicePhotosKeepIntakeClosedUntilConsentIsConfigured(t *testing.T) {
	baseEnv(t)
	t.Setenv("TRACE_PHOTO_METADATA_ENABLED", "true")
	cfg, err := FromEnv()
	if err != nil || !cfg.PhotoMetadataEnabled || cfg.RegistrationOpen || cfg.PhotoBucket != "" {
		t.Fatalf("device metadata should require no image storage and default closed: %+v, %v", cfg, err)
	}
	t.Setenv("TRACE_REGISTRATION_OPEN", "true")
	if _, err := FromEnv(); err == nil {
		t.Fatal("unreviewed default consent opened registration")
	}
	t.Setenv("TRACE_MEMORY_CONSENT_VERSION", "reviewed-device-photo-version")
	t.Setenv("TRACE_CONSENT_BODY", "Device-only photos with server-side metadata.")
	cfg, err = FromEnv()
	if err != nil || !cfg.RegistrationOpen {
		t.Fatalf("explicitly configured device consent must permit intake after review: %v", err)
	}
	t.Setenv("TRACE_PHOTO_ENABLED", "true")
	if _, err := FromEnv(); err == nil {
		t.Fatal("device mode reopened server image uploads")
	}
}

func TestPushRequiresExplicitEnableAndAllSecrets(t *testing.T) {
	baseEnv(t)
	cfg, err := FromEnv()
	if err != nil || cfg.PushEnabled {
		t.Fatal("push enabled without opt-in", err)
	}
	t.Setenv("TRACE_PUSH_ENABLED", "true")
	if _, err := FromEnv(); err == nil {
		t.Fatal("missing keys accepted")
	}
	t.Setenv("TRACE_PUSH_VAPID_PUBLIC_KEY", "public")
	t.Setenv("TRACE_PUSH_VAPID_PRIVATE_KEY", "private")
	t.Setenv("TRACE_PUSH_VAPID_SUBJECT", "mailto:contact@example.org")
	t.Setenv("TRACE_PUSH_ENCRYPTION_KEY", "separate")
	cfg, err = FromEnv()
	if err != nil || !cfg.PushEnabled {
		t.Fatal("complete push config rejected", err)
	}
}

func TestArrivalIssuerAndProofKeysStaySeparateAndDisabledByDefault(t *testing.T) {
	baseEnv(t)
	cfg, err := FromEnv()
	if err != nil || cfg.ArrivalEnabled {
		t.Fatalf("arrival must default off: %v", err)
	}
	t.Setenv("TRACE_ARRIVAL_ENABLED", "true")
	if _, err := FromEnv(); err == nil {
		t.Fatal("enabled arrival without keys accepted")
	}
	issuer := "an-issuer-secret-unique-to-operator-1234"
	proof := "proof-secret-unique-to-projector-4567"
	t.Setenv("TRACE_ARRIVAL_ISSUER_KEY", issuer)
	t.Setenv("TRACE_ARRIVAL_PROOF_KEY", issuer)
	t.Setenv("TRACE_ARRIVAL_VENUE", "booth")
	t.Setenv("TRACE_ARRIVAL_CHECKPOINT_ID", "entrance")
	if _, err := FromEnv(); err == nil {
		t.Fatal("shared issuer and QR key accepted")
	}
	t.Setenv("TRACE_ARRIVAL_PROOF_KEY", proof)
	cfg, err = FromEnv()
	if err != nil || !cfg.ArrivalEnabled {
		t.Fatalf("valid arrival config rejected: %v", err)
	}
	t.Setenv("TRACE_EDGE_SHARED_SECRET", issuer)
	if _, err := FromEnv(); err == nil {
		t.Fatal("issuer key reused as participant edge secret")
	}
}

func displayEnv(t *testing.T) {
	t.Helper()
	baseEnv(t)
	t.Setenv("TRACE_DISPLAY_ENABLED", "true")
	t.Setenv("TRACE_DISPLAY_GOOGLE_CLIENT_ID", "123456789-testwebclient.apps.googleusercontent.com")
	t.Setenv("TRACE_DISPLAY_OPERATOR_EMAILS", " Operator@Example.com , second@example.org ")
	t.Setenv("TRACE_DISPLAY_ALLOWED_ORIGINS", "https://display.example.com, http://localhost:3000,http://127.0.0.1:5173")
	t.Setenv("TRACE_DISPLAY_TOKEN_KEY", "independent-display-token-secret-5678")
}

func TestDisplayGoogleConfigurationIsExplicitAndKeysStaySeparate(t *testing.T) {
	baseEnv(t)
	cfg, err := FromEnv()
	if err != nil || cfg.DisplayEnabled {
		t.Fatalf("display must default disabled: %v", err)
	}
	displayEnv(t)
	cfg, err = FromEnv()
	if err != nil || !cfg.DisplayEnabled {
		t.Fatalf("valid Google display configuration rejected: %v", err)
	}
	if strings.Join(cfg.DisplayOperatorEmails, ",") != "operator@example.com,second@example.org" ||
		len(cfg.DisplayAllowedOrigins) != 3 || cfg.DisplayAllowedOrigins[1] != "http://localhost:3000" {
		t.Fatalf("display configuration was not normalized: %+v", cfg.DisplayOperatorEmails)
	}
	for _, key := range []string{"TRACE_EDGE_SHARED_SECRET", "TRACE_ARRIVAL_ISSUER_KEY", "TRACE_ARRIVAL_PROOF_KEY"} {
		t.Run(key, func(t *testing.T) {
			displayEnv(t)
			t.Setenv(key, cfg.DisplayTokenKey)
			if _, err := FromEnv(); err == nil {
				t.Fatal("display signing key reused for another credential")
			}
		})
	}
}

func TestDisplayRejectsMissingOrInvalidGoogleConfiguration(t *testing.T) {
	for _, tt := range []struct{ key, value string }{
		{"TRACE_DISPLAY_GOOGLE_CLIENT_ID", ""},
		{"TRACE_DISPLAY_GOOGLE_CLIENT_ID", "not-a-google-client"},
		{"TRACE_DISPLAY_GOOGLE_CLIENT_ID", "123-secret.apps.googleusercontent.com/"},
		{"TRACE_DISPLAY_OPERATOR_EMAILS", ""},
		{"TRACE_DISPLAY_OPERATOR_EMAILS", "*@example.com"},
		{"TRACE_DISPLAY_OPERATOR_EMAILS", "Operator <operator@example.com>"},
		{"TRACE_DISPLAY_OPERATOR_EMAILS", "operator@example.com,"},
		{"TRACE_DISPLAY_OPERATOR_EMAILS", "operator@-example.com"},
		{"TRACE_DISPLAY_OPERATOR_EMAILS", "operator..name@example.com"},
		{"TRACE_DISPLAY_OPERATOR_EMAILS", "operator@example.com\nother@example.com"},
		{"TRACE_DISPLAY_ALLOWED_ORIGINS", ""},
		{"TRACE_DISPLAY_ALLOWED_ORIGINS", "*"},
		{"TRACE_DISPLAY_ALLOWED_ORIGINS", "https://*.example.com"},
		{"TRACE_DISPLAY_ALLOWED_ORIGINS", "https://display.example.com/"},
		{"TRACE_DISPLAY_ALLOWED_ORIGINS", "https://display.example.com?"},
		{"TRACE_DISPLAY_ALLOWED_ORIGINS", "https://display.example.com#"},
		{"TRACE_DISPLAY_ALLOWED_ORIGINS", "https://user@display.example.com"},
		{"TRACE_DISPLAY_ALLOWED_ORIGINS", "http://display.example.com"},
		{"TRACE_DISPLAY_ALLOWED_ORIGINS", "http://localhost.evil.example:3000"},
		{"TRACE_DISPLAY_ALLOWED_ORIGINS", "https://display.example.com:70000"},
		{"TRACE_DISPLAY_ALLOWED_ORIGINS", "https://display.example.com,"},
		{"TRACE_DISPLAY_ALLOWED_ORIGINS", "null"},
		{"TRACE_DISPLAY_TOKEN_KEY", ""},
		{"TRACE_DISPLAY_TOKEN_KEY", "too-short"},
	} {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			displayEnv(t)
			t.Setenv(tt.key, tt.value)
			if _, err := FromEnv(); err == nil {
				t.Fatal("invalid display configuration accepted")
			}
		})
	}
}
