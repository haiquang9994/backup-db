package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "time/tzdata" // guarantee LoadLocation works even without host tzdata

	"backupdb/internal/config"
	"backupdb/internal/queue"
	"backupdb/internal/registry"
)

// runScheduler replaces the old crontab-driven dispatch: it polls the
// registry's `schedules` table (managed from the admin UI, any number of
// times per day per database) and enqueues a job whenever a schedule's
// time_of_day matches the current time and hasn't already fired today.
func runScheduler(args []string) error {
	cfg := config.Load()

	loc, err := time.LoadLocation(cfg.SchedulerTimezone)
	if err != nil {
		return fmt.Errorf("load timezone %s: %w", cfg.SchedulerTimezone, err)
	}

	reg, err := registry.Open(cfg.SQLitePath)
	if err != nil {
		return fmt.Errorf("open registry: %w", err)
	}
	defer reg.Close()

	q := queue.New(cfg.RedisHost, cfg.RedisPort)
	defer q.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	fmt.Printf("Scheduler started (timezone %s), checking every 30s...\n", cfg.SchedulerTimezone)

	checkAndEnqueue(ctx, reg, q, loc)

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("Shutting down gracefully...")
			return nil
		case <-ticker.C:
			checkAndEnqueue(ctx, reg, q, loc)
		}
	}
}

func checkAndEnqueue(ctx context.Context, reg *registry.Registry, q *queue.Client, loc *time.Location) {
	now := time.Now().In(loc)

	due, err := reg.ListDueSchedules(ctx, now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "list due schedules:", err)
	}
	for _, item := range due {
		enqueueDue(ctx, reg, q, now, item, reg.MarkScheduleRun)
	}

	dueShared, err := reg.ListDueSharedSchedules(ctx, now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "list due shared schedules:", err)
	}
	for _, item := range dueShared {
		enqueueDue(ctx, reg, q, now, item, reg.MarkSharedScheduleRun)
	}
}

func enqueueDue(ctx context.Context, reg *registry.Registry, q *queue.Client, now time.Time, item registry.DueJob, markRun func(context.Context, int64, string) error) {
	d := item.Database
	job := queue.NewBackupJob(d.Name, d.Driver, d.Host, d.Port, d.Username, d.Password, d.AuthDB, d.StorageTargetID, d.AgentID)

	if err := q.Push(ctx, job); err != nil {
		fmt.Fprintf(os.Stderr, "enqueue %s: %v\n", d.Name, err)
		return
	}
	if err := markRun(ctx, item.ScheduleID, now.Format("2006-01-02")); err != nil {
		fmt.Fprintf(os.Stderr, "mark schedule %d run: %v\n", item.ScheduleID, err)
	}
	fmt.Printf("[%s] Enqueued %s (%s) via schedule at %s\n", now.Format("2006-01-02 15:04:05"), d.Name, d.Driver, now.Format("15:04"))
}
