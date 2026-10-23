package config

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Address                    string
	DatabaseURL                string
	DatabaseMaxConnections     int
	AllowedOrigins             []string
	EdgeSharedSecret           string
	RegistrationOpen           bool
	PhotoEnabled               bool
	PhotoBucket                string
	PhotoSignerEmail           string
	MemoryConsentVersion       string
	LiveConsentVersion         string
	OnSiteConsentVersion       string
	ConsentLanguage            string
	ConsentBody                string
	Contact                    string
	PositionIntervalSeconds    int
	LivePositionTTL            time.Duration
	MemorySessionTTL           time.Duration
	LiveSessionTTL             time.Duration
	DataRetention              time.Duration
	CoordinateVersion          string
	CampusGraphVersion         string
	ThreeDMapVersion           string
	MinLatitude                float64
	MaxLatitude                float64
	MinLongitude               float64
	MaxLongitude               float64
	OriginLatitude             float64
	OriginLongitude            float64
	PositionQuantizationMeters float64
	MaxPositionAccuracyMeters  float64
}

func FromEnv() (Config, error) {
	cfg := Default()
	if port := os.Getenv("PORT"); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return Config{}, fmt.Errorf("PORT must be a valid port number")
		}
		cfg.Address = ":" + port
	}
	cfg.Address = env("TRACE_ADDRESS", cfg.Address)
	cfg.EdgeSharedSecret = os.Getenv("TRACE_EDGE_SHARED_SECRET")
	if cfg.EdgeSharedSecret != "" && len(cfg.EdgeSharedSecret) < 32 {
		return Config{}, fmt.Errorf("TRACE_EDGE_SHARED_SECRET must have at least 32 bytes")
	}
	if os.Getenv("TRACE_DATABASE_PATH") != "" {
		return Config{}, fmt.Errorf("TRACE_DATABASE_PATH is no longer supported; configure PostgreSQL")
	}
	cfg.DatabaseURL = os.Getenv("DATABASE_URL")
	if os.Getenv("TRACE_SESSION_TTL") != "" {
		return Config{}, fmt.Errorf("TRACE_SESSION_TTL is obsolete; set TRACE_MEMORY_SESSION_TTL and TRACE_LIVE_SESSION_TTL")
	}
	if cfg.DatabaseURL == "" {
		host, user, name := os.Getenv("DB_HOST"), os.Getenv("DB_USER"), os.Getenv("DB_NAME")
		if host == "" || user == "" || name == "" || os.Getenv("DB_PASSWORD") == "" {
			return Config{}, fmt.Errorf("set DATABASE_URL or DB_HOST, DB_USER, DB_NAME and DB_PASSWORD")
		}
		u := url.URL{Scheme: "postgresql", Host: "localhost", Path: "/" + name, User: url.UserPassword(user, os.Getenv("DB_PASSWORD"))}
		q := url.Values{"host": {host}, "sslmode": {env("DB_SSLMODE", "verify-full")}}
		u.RawQuery = q.Encode()
		cfg.DatabaseURL = u.String()
	}
	if raw := os.Getenv("TRACE_DB_MAX_CONNECTIONS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			return Config{}, fmt.Errorf("TRACE_DB_MAX_CONNECTIONS must be between 1 and 100")
		}
		cfg.DatabaseMaxConnections = n
	}
	cfg.MemoryConsentVersion = env("TRACE_MEMORY_CONSENT_VERSION", cfg.MemoryConsentVersion)
	cfg.LiveConsentVersion = env("TRACE_LIVE_CONSENT_VERSION", cfg.LiveConsentVersion)
	cfg.OnSiteConsentVersion = env("TRACE_ON_SITE_CONSENT_VERSION", cfg.OnSiteConsentVersion)
	cfg.ConsentLanguage = env("TRACE_CONSENT_LANGUAGE", cfg.ConsentLanguage)
	cfg.ConsentBody = env("TRACE_CONSENT_BODY", cfg.ConsentBody)
	cfg.CoordinateVersion = env("TRACE_COORDINATE_VERSION", cfg.CoordinateVersion)
	cfg.CampusGraphVersion = env("TRACE_CAMPUS_GRAPH_VERSION", cfg.CampusGraphVersion)
	cfg.ThreeDMapVersion = env("TRACE_THREE_D_MAP_VERSION", cfg.ThreeDMapVersion)
	cfg.Contact = env("TRACE_CONTACT", cfg.Contact)
	if raw := os.Getenv("TRACE_ALLOWED_ORIGINS"); raw != "" {
		cfg.AllowedOrigins = splitNonEmpty(raw)
	}

	var err error
	if cfg.RegistrationOpen, err = boolEnv("TRACE_REGISTRATION_OPEN", cfg.RegistrationOpen); err != nil {
		return Config{}, err
	}
	if cfg.PhotoEnabled, err = boolEnv("TRACE_PHOTO_ENABLED", false); err != nil {
		return Config{}, err
	}
	cfg.PhotoBucket = os.Getenv("TRACE_PHOTO_BUCKET")
	cfg.PhotoSignerEmail = os.Getenv("TRACE_PHOTO_SIGNER_EMAIL")
	if cfg.PhotoEnabled && (cfg.PhotoBucket == "" || cfg.PhotoSignerEmail == "") {
		return Config{}, fmt.Errorf("enabled photos require TRACE_PHOTO_BUCKET and TRACE_PHOTO_SIGNER_EMAIL")
	}
	if (cfg.PhotoBucket == "") != (cfg.PhotoSignerEmail == "") {
		return Config{}, fmt.Errorf("photo storage requires both TRACE_PHOTO_BUCKET and TRACE_PHOTO_SIGNER_EMAIL")
	}
	if strings.ContainsAny(cfg.PhotoBucket, "/\\ \t\r\n") ||
		(cfg.PhotoSignerEmail != "" && (!strings.Contains(cfg.PhotoSignerEmail, "@") ||
			!strings.HasSuffix(cfg.PhotoSignerEmail, ".iam.gserviceaccount.com") || strings.ContainsAny(cfg.PhotoSignerEmail, "/\\ \t\r\n"))) {
		return Config{}, fmt.Errorf("photo bucket or signing service account is invalid")
	}
	for name, target := range map[string]*time.Duration{
		"TRACE_LIVE_POSITION_TTL":  &cfg.LivePositionTTL,
		"TRACE_MEMORY_SESSION_TTL": &cfg.MemorySessionTTL,
		"TRACE_LIVE_SESSION_TTL":   &cfg.LiveSessionTTL,
		"TRACE_DATA_RETENTION":     &cfg.DataRetention,
	} {
		if err := durationEnv(name, target); err != nil {
			return Config{}, err
		}
	}
	for name, target := range map[string]*float64{
		"TRACE_MIN_LATITUDE":                 &cfg.MinLatitude,
		"TRACE_MAX_LATITUDE":                 &cfg.MaxLatitude,
		"TRACE_MIN_LONGITUDE":                &cfg.MinLongitude,
		"TRACE_MAX_LONGITUDE":                &cfg.MaxLongitude,
		"TRACE_ORIGIN_LATITUDE":              &cfg.OriginLatitude,
		"TRACE_ORIGIN_LONGITUDE":             &cfg.OriginLongitude,
		"TRACE_POSITION_QUANTIZATION_METERS": &cfg.PositionQuantizationMeters,
		"TRACE_MAX_POSITION_ACCURACY_METERS": &cfg.MaxPositionAccuracyMeters,
	} {
		if err := floatEnv(name, target); err != nil {
			return Config{}, err
		}
	}
	if cfg.MinLatitude >= cfg.MaxLatitude || cfg.MinLongitude >= cfg.MaxLongitude {
		return Config{}, fmt.Errorf("campus geofence minimums must be below maximums")
	}
	if cfg.PositionQuantizationMeters <= 0 || cfg.MaxPositionAccuracyMeters <= 0 {
		return Config{}, fmt.Errorf("position limits must be positive")
	}
	if cfg.MinLatitude < -90 || cfg.MaxLatitude > 90 || cfg.MinLongitude < -180 || cfg.MaxLongitude > 180 ||
		cfg.OriginLatitude < cfg.MinLatitude || cfg.OriginLatitude > cfg.MaxLatitude || cfg.OriginLongitude < cfg.MinLongitude || cfg.OriginLongitude > cfg.MaxLongitude {
		return Config{}, fmt.Errorf("geofence must be valid coordinates and contain the origin")
	}
	if cfg.LivePositionTTL < 30*time.Second || cfg.LivePositionTTL > 60*time.Second {
		return Config{}, fmt.Errorf("TRACE_LIVE_POSITION_TTL must be between 30s and 60s")
	}
	if cfg.MemorySessionTTL <= 0 || cfg.LiveSessionTTL <= 0 || cfg.DataRetention <= 0 {
		return Config{}, fmt.Errorf("session and retention durations must be positive")
	}
	return cfg, nil
}

func Default() Config {
	return Config{
		Address:                    ":8080",
		DatabaseMaxConnections:     5,
		AllowedOrigins:             []string{"http://localhost:3000"},
		RegistrationOpen:           true,
		MemoryConsentVersion:       "2026-memory-01",
		LiveConsentVersion:         "2026-live-01",
		OnSiteConsentVersion:       "2026-on-site-01",
		ConsentLanguage:            "ja",
		ConsentBody:                "写真と位置情報の利用目的・保存期間を確認し、同意して参加してください。",
		Contact:                    "運営担当者へお問い合わせください",
		PositionIntervalSeconds:    5,
		LivePositionTTL:            60 * time.Second,
		MemorySessionTTL:           6 * time.Hour,
		LiveSessionTTL:             6 * time.Hour,
		DataRetention:              24 * time.Hour,
		CoordinateVersion:          "campus-2026-01",
		CampusGraphVersion:         "graph-2026-01",
		ThreeDMapVersion:           "map-2026-01",
		MinLatitude:                34.98,
		MaxLatitude:                35.02,
		MinLongitude:               134.98,
		MaxLongitude:               135.02,
		OriginLatitude:             35.0,
		OriginLongitude:            135.0,
		PositionQuantizationMeters: 10,
		MaxPositionAccuracyMeters:  100,
	}
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func splitNonEmpty(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func boolEnv(name string, fallback bool) (bool, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s: %w", name, err)
	}
	return value, nil
}

func floatEnv(name string, target *float64) error {
	raw := os.Getenv(name)
	if raw == "" {
		return nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("%s must be a finite number", name)
	}
	*target = value
	return nil
}

func durationEnv(name string, target *time.Duration) error {
	raw := os.Getenv(name)
	if raw == "" {
		return nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if value <= 0 {
		return fmt.Errorf("%s must be positive", name)
	}
	*target = value
	return nil
}
