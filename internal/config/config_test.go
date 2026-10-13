package config

import (
	"net/url"
	"strings"
	"testing"
)

func baseEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"PORT", "TRACE_ADDRESS", "TRACE_DATABASE_PATH", "DB_HOST", "DB_USER", "DB_NAME", "DB_PASSWORD", "TRACE_DB_MAX_CONNECTIONS", "TRACE_MIN_LATITUDE", "TRACE_LIVE_POSITION_TTL", "TRACE_EDGE_SHARED_SECRET"} {
		t.Setenv(k, "")
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
		{"TRACE_LIVE_POSITION_TTL", "5m"}, {"DATABASE_URL", ""},
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
func TestDatabaseMissingDoesNotExposeSecrets(t *testing.T) {
	baseEnv(t)
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DB_PASSWORD", "do-not-log-this")
	_, err := FromEnv()
	if err == nil || strings.Contains(err.Error(), "do-not-log-this") {
		t.Fatal("unsafe or absent error")
	}
}
