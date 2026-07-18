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

	TelegramBotToken    string
	TelegramChatID      string
	TelegramLogBotToken string
	TelegramLogChatID   string

	MongoHost     string
	MongoPort     string
	MongoUsername string
	MongoPassword string
	MongoAuthDB   string

	// GoogleCredentialsFile is the OAuth client (app identity), shared by
	// every connected Google account. Per-account tokens live in the
	// registry's storage_targets table, not on disk.
	GoogleCredentialsFile string

	SQLitePath string

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

		TelegramBotToken:    getEnv("TELEGRAM_BOT_TOKEN", ""),
		TelegramChatID:      getEnv("TELEGRAM_CHAT_ID", ""),
		TelegramLogBotToken: getEnv("TELEGRAM_LOG_BOT_TOKEN", ""),
		TelegramLogChatID:   getEnv("TELEGRAM_LOG_CHAT_ID", ""),

		MongoHost:     getEnv("MONGO_HOST", "127.0.0.1"),
		MongoPort:     getEnv("MONGO_PORT", "27017"),
		MongoUsername: getEnv("MONGO_USERNAME", ""),
		MongoPassword: getEnv("MONGO_PASSWORD", ""),
		MongoAuthDB:   getEnv("MONGO_AUTH_DB", "admin"),

		GoogleCredentialsFile: getEnv("GOOGLE_CREDENTIALS_FILE", "./google/credentials.json"),

		SQLitePath: getEnv("SQLITE_PATH", "./data/backupdb.sqlite3"),

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
