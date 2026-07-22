// Package notify sends backup outcomes to a database's configured
// notification channels (internal/registry's notify_channels — a Telegram
// bot/chat today, more kinds later). Mirrors internal/storage's
// kind-dispatch design: adding a new kind means one new case in Build and
// one new file implementing Channel; no changes anywhere else.
package notify

import (
	"context"
	"fmt"
	"log"

	"backupdb/internal/registry"
)

// Channel delivers a single already-formatted message. Message formatting
// (which event, which database, the error text) stays in this package's
// Dispatch functions so every kind gets the same wording for free instead
// of reimplementing it.
type Channel interface {
	Send(ctx context.Context, message string) error
}

// Build constructs the Channel for an already-loaded notify_channels row.
func Build(ch registry.NotifyChannel) (Channel, error) {
	switch ch.Kind {
	case "telegram":
		return newTelegram(ch)
	default:
		return nil, fmt.Errorf("unknown notify channel kind: %q", ch.Kind)
	}
}

// DispatchSuccess sends a success message to every channel the database is
// assigned to. Failures to list channels, build one, or send are logged and
// otherwise ignored — one bad channel must never block the others or fail
// the backup job itself.
func DispatchSuccess(ctx context.Context, reg *registry.Registry, databaseID int64, projectName, dbname, driver string) {
	message := fmt.Sprintf("Backup: %s (%s)", dbname, driver)
	dispatch(ctx, reg, databaseID, withProject(projectName, message))
}

// DispatchError sends a failure message to every channel the database is
// assigned to.
func DispatchError(ctx context.Context, reg *registry.Registry, databaseID int64, projectName, dbname string, jobErr error) {
	message := fmt.Sprintf("Lỗi backup database: %s\n - %s", dbname, jobErr.Error())
	dispatch(ctx, reg, databaseID, withProject(projectName, message))
}

func withProject(projectName, message string) string {
	if projectName == "" {
		return message
	}
	return fmt.Sprintf("[%s] %s", projectName, message)
}

func dispatch(ctx context.Context, reg *registry.Registry, databaseID int64, message string) {
	channels, err := reg.ListNotifyChannelsForDatabase(ctx, databaseID)
	if err != nil {
		log.Println("notify: list channels for database", databaseID, ":", err)
		return
	}
	for _, ch := range channels {
		impl, err := Build(ch)
		if err != nil {
			log.Println("notify: build channel", ch.ID, "(", ch.Label, "):", err)
			continue
		}
		if err := impl.Send(ctx, message); err != nil {
			log.Println("notify: send via channel", ch.ID, "(", ch.Label, "):", err)
		}
	}
}
