package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Address                    string
	DatabasePath               string
	AllowedOrigins             []string
	RegistrationOpen           bool
	MemoryConsentVersion       string
	LiveConsentVersion         string
	OnSiteConsentVersion       string
	ConsentLanguage            string
	ConsentBody                string
	Contact                    string
	PositionIntervalSeconds    int
	LivePositionTTL            time.Duration
	SessionTTL                 time.Duration
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
	cfg.Address = env("TRACE_ADDRESS", cfg.Address)
	cfg.DatabasePath = env("TRACE_DATABASE_PATH", cfg.DatabasePath)
	cfg.MemoryConsentVersion = env("TRACE_MEMORY_CONSENT_VERSION", cfg.MemoryConsentVersion)
	cfg.LiveConsentVersion = env("TRACE_LIVE_CONSENT_VERSION", cfg.LiveConsentVersion)
	cfg.OnSiteConsentVersion = env("TRACE_ON_SITE_CONSENT_VERSION", cfg.OnSiteConsentVersion)
	cfg.ConsentLanguage = env("TRACE_CONSENT_LANGUAGE", cfg.ConsentLanguage)
	cfg.ConsentBody = env("TRACE_CONSENT_BODY", cfg.ConsentBody)
	cfg.Contact = env("TRACE_CONTACT", cfg.Contact)
	if raw := os.Getenv("TRACE_ALLOWED_ORIGINS"); raw != "" {
		cfg.AllowedOrigins = splitNonEmpty(raw)
	}

	var err error
	if cfg.RegistrationOpen, err = boolEnv("TRACE_REGISTRATION_OPEN", cfg.RegistrationOpen); err != nil {
		return Config{}, err
	}
	for name, target := range map[string]*time.Duration{
		"TRACE_LIVE_POSITION_TTL": &cfg.LivePositionTTL,
		"TRACE_SESSION_TTL":       &cfg.SessionTTL,
		"TRACE_DATA_RETENTION":    &cfg.DataRetention,
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
	return cfg, nil
}

func Default() Config {
	return Config{
		Address:                    ":8080",
		DatabasePath:               "trace.db",
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
		SessionTTL:                 6 * time.Hour,
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
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
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
