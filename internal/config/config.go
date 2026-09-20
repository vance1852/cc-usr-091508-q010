package config

import (
	"os"
)

type Config struct {
	HTTPAddr      string
	DatabaseURL   string
	SchemaPath    string
	SeedPath      string
	RunMigrations bool
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func Load() Config {
	return Config{
		HTTPAddr:      getenv("HTTP_ADDR", ":8080"),
		DatabaseURL:   getenv("DATABASE_URL", "postgres://node@127.0.0.1:55432/runway?sslmode=disable"),
		SchemaPath:    getenv("SCHEMA_PATH", "internal/store/schema.sql"),
		SeedPath:      getenv("SEED_PATH", ""),
		RunMigrations: getenv("RUN_MIGRATIONS", "1") != "0",
	}
}
