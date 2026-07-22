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

	// Timezone is the IANA zone for the whole deployment: the scheduler uses
	// it to decide what's due, the admin UI uses it for "(giờ ...)" labels
	// and timestamp display, and the consumer/agent/upload paths use it to
	// stamp backup filenames and the storage date-folder. One value for the
	// whole deployment, not per admin/browser/agent server — an agent may
	// run on a different machine with a different OS clock/zone, so it must
	// be told this value rather than using its own local time.
	Timezone string

	AdminUsername string
	AdminPassword string
	AdminPort     string

	// Agent* configure the `backupdb agent` subcommand — a standalone HTTPS
	// server for dump+upload on a database server this deployment can't
	// reach any other way (see internal/agentproto). Unused by every other
	// subcommand.
	AgentPort     string
	AgentToken    string // shared secret; required to start the agent, never defaulted
	AgentCertFile string // self-signed TLS cert, generated on first run if missing
	AgentKeyFile  string

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

		Timezone: getEnv("TIMEZONE", "Asia/Ho_Chi_Minh"),

		AdminUsername: getEnv("ADMIN_USERNAME", ""),
		AdminPassword: getEnv("ADMIN_PASSWORD", ""),
		AdminPort:     getEnv("ADMIN_PORT", "8080"),

		AgentPort:     getEnv("AGENT_PORT", "8443"),
		AgentToken:    getEnv("AGENT_TOKEN", ""),
		AgentCertFile: getEnv("AGENT_CERT_FILE", "./data/agent-cert.pem"),
		AgentKeyFile:  getEnv("AGENT_KEY_FILE", "./data/agent-key.pem"),

		TmpDir: getEnv("TMP_DIR", os.TempDir()+"/backupdb"),
	}
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
