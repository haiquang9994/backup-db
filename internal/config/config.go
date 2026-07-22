// Package config loads runtime configuration from .env and the process environment.
package config

import (
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	RedisHost string
	RedisPort string

	ProjectName string

	// GoogleCredentialsFile is the OAuth client (app identity), shared by
	// every connected Google account. Per-account tokens live in the
	// registry's storage_targets table, not on disk.
	GoogleCredentialsFile string

	SQLitePath string

	// SchedulerTimezone is the IANA zone schedules and shared schedule times
	// are interpreted in — both by the scheduler when deciding what's due,
	// and in the admin UI's "(giờ ...)" labels. One value for the whole
	// deployment, not per admin/browser: schedules are stored as plain
	// "HH:MM" with no timezone of their own, so everyone reading/editing
	// them needs to agree on what zone that means.
	SchedulerTimezone string

	AdminUsername string
	AdminPassword string
	AdminPort     string

	TmpDir string
}

// Load reads .env (if present) into the process environment, then builds a Config.
// It never fails when .env is missing — real deployments pass config via the
// container environment instead.
func Load() *Config {
	_ = godotenv.Load()

	return &Config{
		RedisHost: getEnv("REDIS_HOST", "127.0.0.1"),
		RedisPort: getEnv("REDIS_PORT", "6379"),

		ProjectName: getEnv("PROJECT_NAME", ""),

		GoogleCredentialsFile: getEnv("GOOGLE_CREDENTIALS_FILE", "./google/credentials.json"),

		SQLitePath: getEnv("SQLITE_PATH", "./data/backupdb.sqlite3"),

		SchedulerTimezone: getEnv("SCHEDULER_TIMEZONE", "Asia/Ho_Chi_Minh"),

		AdminUsername: getEnv("ADMIN_USERNAME", ""),
		AdminPassword: getEnv("ADMIN_PASSWORD", ""),
		AdminPort:     getEnv("ADMIN_PORT", "8080"),

		TmpDir: getEnv("TMP_DIR", os.TempDir()+"/backupdb"),
	}
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
