// Package notify sends backup outcomes to Telegram via the project's
// telegram-pusher relay, mirroring the PHP app's alert_telegram/push_log.
package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"backupdb/internal/config"
)

const pusherURL = "https://telegram-pusher.phuongnamdigital.com"

var httpClient = &http.Client{Timeout: 15 * time.Second}

// AlertError reports a backup failure to the error Telegram channel.
func AlertError(cfg *config.Config, err error) {
	if cfg.TelegramBotToken == "" || cfg.TelegramChatID == "" {
		return
	}
	message := fmt.Sprintf("Lỗi backup database: %s\n - %s", cfg.ProjectName, err.Error())
	send(cfg.TelegramBotToken, cfg.TelegramChatID, message)
}

// PushLog reports a successful backup to the log Telegram channel.
func PushLog(cfg *config.Config, dbname, driver string) {
	if cfg.TelegramLogBotToken == "" || cfg.TelegramLogChatID == "" {
		return
	}
	message := fmt.Sprintf("Backup: %s (%s)", dbname, driver)
	send(cfg.TelegramLogBotToken, cfg.TelegramLogChatID, message)
}

func send(botToken, chatID, message string) {
	body, err := json.Marshal(map[string]string{
		"bot_token":  botToken,
		"chat_id":    chatID,
		"message":    message,
		"parse_mode": "HTML",
	})
	if err != nil {
		return
	}

	req, err := http.NewRequest(http.MethodPost, pusherURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
}
