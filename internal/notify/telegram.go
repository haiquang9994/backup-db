package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"backupdb/internal/registry"
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

// telegramConfig is the JSON shape stored in notify_channels.config for
// kind="telegram".
type telegramConfig struct {
	BotToken string `json:"bot_token"`
	ChatID   string `json:"chat_id"`
}

type telegramChannel struct {
	botToken string
	chatID   string
}

func newTelegram(ch registry.NotifyChannel) (Channel, error) {
	var tc telegramConfig
	if err := json.Unmarshal([]byte(ch.Config), &tc); err != nil {
		return nil, fmt.Errorf("parse telegram channel config: %w", err)
	}
	if tc.BotToken == "" || tc.ChatID == "" {
		return nil, fmt.Errorf("telegram channel missing bot token or chat id")
	}
	return &telegramChannel{botToken: tc.BotToken, chatID: tc.ChatID}, nil
}

// Send posts directly to the Telegram Bot API — no relay in between.
func (t *telegramChannel) Send(ctx context.Context, message string) error {
	body, err := json.Marshal(map[string]string{
		"chat_id":    t.chatID,
		"text":       message,
		"parse_mode": "HTML",
	})
	if err != nil {
		return err
	}

	url := "https://api.telegram.org/bot" + t.botToken + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram api %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	return nil
}
