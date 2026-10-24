package config

import (
	"net/url"
	"strings"
	"testing"
)

func baseEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"PORT", "TRACE_ADDRESS", "TRACE_DATABASE_PATH", "DB_HOST", "DB_USER", "DB_NAME", "DB_PASSWORD", "TRACE_DB_MAX_CONNECTIONS", "TRACE_MIN_LATITUDE", "TRACE_LIVE_POSITION_TTL", "TRACE_SESSION_TTL", "TRACE_MEMORY_SESSION_TTL", "TRACE_LIVE_SESSION_TTL", "TRACE_DATA_RETENTION", "TRACE_EDGE_SHARED_SECRET"} {
		t.Setenv(k, "")
	}
	for _, key := range []string{"TRACE_PHOTO_ENABLED", "TRACE_PHOTO_BUCKET", "TRACE_PHOTO_SIGNER_EMAIL",
		"TRACE_PUSH_ENABLED", "TRACE_PUSH_VAPID_PUBLIC_KEY", "TRACE_PUSH_VAPID_PRIVATE_KEY", "TRACE_PUSH_VAPID_SUBJECT", "TRACE_PUSH_ENCRYPTION_KEY",
		"TRACE_ARRIVAL_ENABLED", "TRACE_ARRIVAL_ISSUER_KEY", "TRACE_ARRIVAL_PROOF_KEY", "TRACE_ARRIVAL_VENUE", "TRACE_ARRIVAL_CHECKPOINT_ID",
		"TRACE_DISPLAY_ENABLED", "TRACE_DISPLAY_ISSUER_KEY", "TRACE_DISPLAY_TOKEN_KEY"} {
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

func TestSpatialVersionsAreExplicitAndPhotoFlagRemainsOff(t *testing.T) {
	baseEnv(t)
	t.Setenv("TRACE_COORDINATE_VERSION", "toyosu-2026-08-28.1")
	t.Setenv("TRACE_CAMPUS_GRAPH_VERSION", "toyosu-graph-2026-10-21.1.draft")
	t.Setenv("TRACE_THREE_D_MAP_VERSION", "キャンパス全体.blend 2026-10-21")
	cfg, err := FromEnv()
	if err != nil || cfg.PhotoEnabled || cfg.CoordinateVersion != "toyosu-2026-08-28.1" ||
		cfg.CampusGraphVersion != "toyosu-graph-2026-10-21.1.draft" || cfg.ThreeDMapVersion != "キャンパス全体.blend 2026-10-21" {
		t.Fatalf("spatial version overrides: %+v, %v", cfg, err)
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

func TestDisplayCredentialsAreOptionalAndSeparated(t *testing.T) {
	baseEnv(t)
	cfg, err := FromEnv()
	if err != nil || cfg.DisplayEnabled {
		t.Fatalf("display must default disabled: %v", err)
	}
	t.Setenv("TRACE_DISPLAY_ENABLED", "true")
	if _, err := FromEnv(); err == nil {
		t.Fatal("display without secrets accepted")
	}
	issuer := "independent-display-issuer-secret-1234"
	token := "independent-display-token-secret-5678"
	t.Setenv("TRACE_DISPLAY_ISSUER_KEY", issuer)
	t.Setenv("TRACE_DISPLAY_TOKEN_KEY", issuer)
	if _, err := FromEnv(); err == nil {
		t.Fatal("reused display signing key accepted")
	}
	t.Setenv("TRACE_DISPLAY_TOKEN_KEY", token)
	cfg, err = FromEnv()
	if err != nil || !cfg.DisplayEnabled {
		t.Fatalf("independent keys refused: %v", err)
	}
	t.Setenv("TRACE_EDGE_SHARED_SECRET", issuer)
	if _, err := FromEnv(); err == nil {
		t.Fatal("edge key reused for display issuer")
	}
	t.Setenv("TRACE_EDGE_SHARED_SECRET", "")
	t.Setenv("TRACE_ARRIVAL_ISSUER_KEY", token)
	if _, err := FromEnv(); err == nil {
		t.Fatal("arrival issuer key reused for display signing")
	}
}
