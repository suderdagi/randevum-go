// Package config reads settings from environment variables and fails fast when one is missing.
package config

import (
	"errors"
	"os"
	"time"
)

type Config struct {
	Port            string
	DatabaseURL     string
	JWTSecret       []byte
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		Port:            getenv("PORT", "4000"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		JWTSecret:       []byte(os.Getenv("JWT_SECRET")),
		AccessTokenTTL:  15 * time.Minute,
		RefreshTokenTTL: 30 * 24 * time.Hour,
	}
	if cfg.DatabaseURL == "" {
		return cfg, errors.New("DATABASE_URL is required")
	}
	if len(cfg.JWTSecret) < 32 {
		return cfg, errors.New("JWT_SECRET must be at least 32 characters")
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
