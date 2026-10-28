package config

import (
	"fmt"
	"math"
	"net"
	"net/mail"
	"net/url"
	"os"
	"regexp"
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
	PhotoMetadataEnabled       bool
	ArrivalEnabled             bool
	ArrivalIssuerKey           string
	ArrivalProofKey            string
	ArrivalVenue               string
	ArrivalCheckpointID        string
	DisplayEnabled             bool
	DisplayGoogleClientID      string
	DisplayOperatorEmails      []string
	DisplayAllowedOrigins      []string
	DisplayTokenKey            string
	PhotoBucket                string
	PhotoSignerEmail           string
	PushEnabled                bool
	PushVAPIDPublicKey         string
	PushVAPIDPrivateKey        string
	PushVAPIDSubject           string
	PushEncryptionKey          string
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
	if cfg.MemoryConsentVersion == "2026-memory-01" {
		return Config{}, fmt.Errorf("TRACE_MEMORY_CONSENT_VERSION must not reuse the retired GPS consent version")
	}
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
	if cfg.ArrivalEnabled, err = boolEnv("TRACE_ARRIVAL_ENABLED", false); err != nil {
		return Config{}, err
	}
	cfg.ArrivalIssuerKey = os.Getenv("TRACE_ARRIVAL_ISSUER_KEY")
	cfg.ArrivalProofKey = os.Getenv("TRACE_ARRIVAL_PROOF_KEY")
	cfg.ArrivalVenue = os.Getenv("TRACE_ARRIVAL_VENUE")
	cfg.ArrivalCheckpointID = os.Getenv("TRACE_ARRIVAL_CHECKPOINT_ID")
	if cfg.ArrivalEnabled && (len(cfg.ArrivalIssuerKey) < 32 || len(cfg.ArrivalProofKey) < 32 ||
		cfg.ArrivalIssuerKey == cfg.ArrivalProofKey ||
		(cfg.EdgeSharedSecret != "" && (cfg.EdgeSharedSecret == cfg.ArrivalIssuerKey || cfg.EdgeSharedSecret == cfg.ArrivalProofKey)) ||
		cfg.ArrivalVenue == "" || cfg.ArrivalCheckpointID == "" ||
		strings.ContainsAny(cfg.ArrivalVenue+cfg.ArrivalCheckpointID, " \t\r\n")) {
		return Config{}, fmt.Errorf("arrival requires distinct 32-byte issuer/proof keys and venue/checkpoint identifiers")
	}
	if cfg.DisplayEnabled, err = boolEnv("TRACE_DISPLAY_ENABLED", false); err != nil {
		return Config{}, err
	}
	cfg.DisplayGoogleClientID = os.Getenv("TRACE_DISPLAY_GOOGLE_CLIENT_ID")
	if raw := os.Getenv("TRACE_DISPLAY_OPERATOR_EMAILS"); raw != "" {
		for _, email := range strings.Split(raw, ",") {
			cfg.DisplayOperatorEmails = append(cfg.DisplayOperatorEmails, strings.ToLower(strings.TrimSpace(email)))
		}
	}
	if raw := os.Getenv("TRACE_DISPLAY_ALLOWED_ORIGINS"); raw != "" {
		for _, origin := range strings.Split(raw, ",") {
			cfg.DisplayAllowedOrigins = append(cfg.DisplayAllowedOrigins, strings.TrimSpace(origin))
		}
	}
	cfg.DisplayTokenKey = os.Getenv("TRACE_DISPLAY_TOKEN_KEY")
	if cfg.DisplayEnabled {
		if err := cfg.ValidateDisplay(); err != nil {
			return Config{}, err
		}
	}
	if cfg.PhotoEnabled, err = boolEnv("TRACE_PHOTO_ENABLED", false); err != nil {
		return Config{}, err
	}
	if cfg.PhotoMetadataEnabled, err = boolEnv("TRACE_PHOTO_METADATA_ENABLED", false); err != nil {
		return Config{}, err
	}
	if cfg.PushEnabled, err = boolEnv("TRACE_PUSH_ENABLED", false); err != nil {
		return Config{}, err
	}
	cfg.PushVAPIDPublicKey = os.Getenv("TRACE_PUSH_VAPID_PUBLIC_KEY")
	cfg.PushVAPIDPrivateKey = os.Getenv("TRACE_PUSH_VAPID_PRIVATE_KEY")
	cfg.PushVAPIDSubject = os.Getenv("TRACE_PUSH_VAPID_SUBJECT")
	cfg.PushEncryptionKey = os.Getenv("TRACE_PUSH_ENCRYPTION_KEY")
	if cfg.PushEnabled && (cfg.PushVAPIDPublicKey == "" || cfg.PushVAPIDPrivateKey == "" ||
		cfg.PushVAPIDSubject == "" || cfg.PushEncryptionKey == "") {
		return Config{}, fmt.Errorf("enabled push requires VAPID public/private keys, subject and separate encryption key")
	}
	cfg.PhotoBucket = os.Getenv("TRACE_PHOTO_BUCKET")
	cfg.PhotoSignerEmail = os.Getenv("TRACE_PHOTO_SIGNER_EMAIL")
	if cfg.PhotoEnabled && (cfg.PhotoBucket == "" || cfg.PhotoSignerEmail == "") {
		return Config{}, fmt.Errorf("enabled photos require TRACE_PHOTO_BUCKET and TRACE_PHOTO_SIGNER_EMAIL")
	}
	if (cfg.PhotoBucket == "") != (cfg.PhotoSignerEmail == "") {
		return Config{}, fmt.Errorf("photo storage requires both TRACE_PHOTO_BUCKET and TRACE_PHOTO_SIGNER_EMAIL")
	}
	if cfg.PhotoMetadataEnabled && cfg.PhotoEnabled {
		return Config{}, fmt.Errorf("device-only photo metadata cannot be enabled with legacy photo uploads")
	}
	if cfg.RegistrationOpen && (!cfg.PhotoMetadataEnabled ||
		os.Getenv("TRACE_MEMORY_CONSENT_VERSION") == "" ||
		os.Getenv("TRACE_CONSENT_BODY") == "" ||
		cfg.MemoryConsentVersion == Default().MemoryConsentVersion ||
		cfg.ConsentBody == Default().ConsentBody) {
		return Config{}, fmt.Errorf("registration requires device-only photos and a separately configured, reviewed memory consent")
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

var displayClientIDPattern = regexp.MustCompile(`^[0-9]+-[A-Za-z0-9_-]+\.apps\.googleusercontent\.com$`)
var displayEmailLocalPattern = regexp.MustCompile("^[a-z0-9.!#$%&'+/=?^_`{|}~-]+$")
var displayHostLabelPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`)

// ValidateDisplay checks the settings required to enable browser display access.
func (cfg Config) ValidateDisplay() error {
	if len(cfg.DisplayTokenKey) < 32 || cfg.DisplayTokenKey == cfg.ArrivalIssuerKey ||
		cfg.DisplayTokenKey == cfg.ArrivalProofKey || cfg.DisplayTokenKey == cfg.EdgeSharedSecret {
		return fmt.Errorf("display requires an independent 32-byte token signing key")
	}
	if !displayClientIDPattern.MatchString(cfg.DisplayGoogleClientID) {
		return fmt.Errorf("display requires a Google OAuth Web client ID")
	}
	if len(cfg.DisplayOperatorEmails) == 0 || len(cfg.DisplayAllowedOrigins) == 0 {
		return fmt.Errorf("display requires operator emails and allowed origins")
	}
	for _, email := range cfg.DisplayOperatorEmails {
		address, err := mail.ParseAddress(email)
		local, domain, ok := strings.Cut(email, "@")
		if err != nil || address.Name != "" || address.Address != email || !ok ||
			email != strings.ToLower(strings.TrimSpace(email)) || len(email) > 254 || len(local) > 64 ||
			!displayEmailLocalPattern.MatchString(local) || strings.HasPrefix(local, ".") ||
			strings.HasSuffix(local, ".") || strings.Contains(local, "..") ||
			!strings.Contains(domain, ".") || !validDisplayHost(domain) {
			return fmt.Errorf("display operator emails must be exact valid email addresses without wildcards")
		}
	}
	for _, origin := range cfg.DisplayAllowedOrigins {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" ||
			u.ForceQuery || u.Fragment != "" || strings.ContainsAny(origin, "*\\ \t\r\n?#") ||
			u.Opaque != "" || strings.HasSuffix(u.Host, ":") ||
			!validDisplayHost(u.Hostname()) ||
			(u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"))) {
			return fmt.Errorf("display origins must be exact HTTPS origins or localhost/127.0.0.1 HTTP origins")
		}
		if port := u.Port(); port != "" {
			value, err := strconv.Atoi(port)
			if err != nil || value < 1 || value > 65535 {
				return fmt.Errorf("display origin port must be between 1 and 65535")
			}
		}
	}
	return nil
}

func validDisplayHost(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) > 63 || !displayHostLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

func Default() Config {
	return Config{
		Address:                    ":8080",
		DatabaseMaxConnections:     5,
		AllowedOrigins:             []string{"http://localhost:3000"},
		RegistrationOpen:           false,
		MemoryConsentVersion:       "2026-memory-photo-no-gps-draft-1",
		LiveConsentVersion:         "2026-live-01",
		OnSiteConsentVersion:       "2026-on-site-01",
		ConsentLanguage:            "ja",
		ConsentBody:                "写真による参加の同意文は未承認です。承認済み文面と保存期間を設定するまで新規受付を開始しないでください。",
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
