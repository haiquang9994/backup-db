package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"backupdb/internal/config"
	"backupdb/internal/dump"
	"backupdb/internal/notify"
	"backupdb/internal/queue"
	"backupdb/internal/registry"
	"backupdb/internal/storage"
)

// runConsumer processes jobs from the Redis queue until it receives
// SIGTERM/SIGINT, finishing the in-flight job before exiting. Unlike the
// original PHP worker, it does not need to self-terminate after N jobs —
// that was a workaround for PHP long-running process stability that Go
// doesn't need. Docker's `restart: unless-stopped` still recovers it from a
// crash.
//
// It opens the registry read-only in spirit (only storage_targets lookups
// and, on Google token refresh, a write-back) so it never needs the upload
// destination to be configured just to start: a job that arrives before its
// database's storage destination is set up simply fails with a clear error
// instead of blocking every other job.
func runConsumer(args []string) error {
	cfg := config.Load()

	if err := os.MkdirAll(cfg.TmpDir, 0o755); err != nil {
		return fmt.Errorf("prepare tmp dir: %w", err)
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

	logLine("Consumer started, waiting for jobs...")

	for ctx.Err() == nil {
		job, err := q.Pop(ctx, 5*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			logErr("queue pop error: %v", err)
			time.Sleep(time.Second)
			continue
		}
		if job == nil {
			continue
		}
		processJob(ctx, cfg, reg, *job)
	}

	logLine("Shutting down gracefully...")
	return nil
}

func processJob(ctx context.Context, cfg *config.Config, reg *registry.Registry, job queue.Job) {
	if job.Cmd != "backup" {
		return
	}

	if err := backupAndUpload(ctx, cfg, reg, job); err != nil {
		logErr("%s: FAILED: %v", job.DBName, err)
		notify.AlertError(cfg, err)
	} else {
		notify.PushLog(cfg, job.DBName, job.Driver)
	}
}

func backupAndUpload(ctx context.Context, cfg *config.Config, reg *registry.Registry, job queue.Job) error {
	start := time.Now()
	params := dump.ParseParams(job.Params)

	var ext string
	switch job.Driver {
	case "mysql", "postgres":
		ext = "sql"
	case "mongo":
		ext = "archive"
	default:
		return fmt.Errorf("unknown driver: %s", job.Driver)
	}

	date := time.Now().Format("060102")
	stamp := time.Now().Format("15h04")
	filename := fmt.Sprintf("%s_%s_%s.%s.gz", job.DBName, date, stamp, ext)
	outPath := filepath.Join(cfg.TmpDir, filename)

	dumpStart := time.Now()
	var err error
	switch job.Driver {
	case "mysql":
		err = dump.MySQL(ctx, job.DBName, params, outPath)
	case "postgres":
		err = dump.Postgres(ctx, job.DBName, params, outPath)
	case "mongo":
		err = dump.Mongo(ctx, job.DBName, params, outPath)
	}
	if err != nil {
		return fmt.Errorf("dump failed: %w", err)
	}
	dumpDuration := time.Since(dumpStart)

	var sizeMB float64
	if info, statErr := os.Stat(outPath); statErr == nil {
		sizeMB = float64(info.Size()) / 1024 / 1024
	}
	logLine("%s (%s): dump ok, %.2f MB, %s", job.DBName, job.Driver, sizeMB, round(dumpDuration))

	if job.StorageTargetID == 0 {
		return fmt.Errorf("no storage destination selected for this database — assign one in the admin UI")
	}
	target, err := reg.GetStorageTarget(ctx, job.StorageTargetID)
	if err != nil {
		return fmt.Errorf("load storage destination #%d: %w", job.StorageTargetID, err)
	}
	if target == nil {
		return fmt.Errorf("storage destination #%d not found (was it deleted?)", job.StorageTargetID)
	}
	store, err := storage.Build(cfg, reg, *target)
	if err != nil {
		return fmt.Errorf("init storage destination: %w", err)
	}

	uploadStart := time.Now()
	if err := store.Upload(ctx, job.DBName, date, filename, outPath); err != nil {
		return fmt.Errorf("upload failed: %w", err)
	}
	logLine("%s: upload ok -> %s %q, %s (tổng %s)", job.DBName, target.Kind, target.Label, round(time.Since(uploadStart)), round(time.Since(start)))

	_ = os.Remove(outPath)
	return nil
}

func round(d time.Duration) time.Duration {
	return d.Round(100 * time.Millisecond)
}

func logLine(format string, args ...any) {
	fmt.Printf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

func logErr(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}
