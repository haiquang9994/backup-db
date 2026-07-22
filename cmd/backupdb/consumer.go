package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"backupdb/internal/agentproto"
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

	loc, err := time.LoadLocation(cfg.SchedulerTimezone)
	if err != nil {
		return fmt.Errorf("load timezone %s: %w", cfg.SchedulerTimezone, err)
	}

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

	// Jobs dispatched to a remote agent spend almost all their time idly
	// polling over the network, not doing real work on this machine — run
	// those in the background instead of blocking the loop, so a slow (or
	// slow-to-respond) agent never stalls every other database's backup
	// behind it. Local jobs keep processing one at a time as before (still
	// worth serializing: several mysqldump/pg_dump/mongodump at once would
	// compete for this machine's own CPU/network). wg makes sure any
	// still-running background dispatch finishes before reg/q close below.
	var wg sync.WaitGroup
	defer wg.Wait()

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
		if job.AgentID != 0 {
			wg.Add(1)
			go func(j queue.Job) {
				defer wg.Done()
				processJob(ctx, cfg, reg, loc, j)
			}(*job)
			continue
		}
		processJob(ctx, cfg, reg, loc, *job)
	}

	logLine("Shutting down gracefully...")
	return nil
}

func processJob(ctx context.Context, cfg *config.Config, reg *registry.Registry, loc *time.Location, job queue.Job) {
	if job.Cmd != "backup" {
		return
	}

	started := time.Now().In(loc)
	result, jobErr := backupAndUpload(ctx, cfg, reg, job)
	duration := time.Since(started)
	if jobErr != nil {
		logErr("%s: FAILED: %v", job.DBName, jobErr)
	}

	// Channels are assigned per database (registry), not carried on the
	// job, so resolve job.DBName back to its current row. A database
	// deleted between enqueue and completion just means no notification —
	// same as it already did for storage.
	d, err := reg.GetByName(ctx, job.DBName)
	if err != nil {
		logErr("notify: look up %s: %v", job.DBName, err)
	}

	recordBackupRun(ctx, reg, d, job, started, duration, jobErr)
	if jobErr == nil {
		recordBackupFile(ctx, reg, d, job, result)
	}

	if d == nil {
		return
	}
	if jobErr != nil {
		notify.DispatchError(ctx, reg, d.ID, cfg.ProjectName, job.DBName, jobErr)
	} else {
		notify.DispatchSuccess(ctx, reg, d.ID, cfg.ProjectName, job.DBName, job.Driver)
	}
}

// recordBackupRun writes one entry to the admin UI's "Nhật ký" log, best
// effort — a logging failure must never fail the job itself.
func recordBackupRun(ctx context.Context, reg *registry.Registry, d *registry.Database, job queue.Job, started time.Time, duration time.Duration, jobErr error) {
	var databaseID int64
	if d != nil {
		databaseID = d.ID
	}
	status, message := "success", ""
	if jobErr != nil {
		status, message = "error", jobErr.Error()
	}
	run := registry.BackupRun{
		DatabaseID: databaseID,
		DBName:     job.DBName,
		Driver:     job.Driver,
		Status:     status,
		Message:    message,
		DurationMS: duration.Milliseconds(),
		StartedAt:  started.Format("2006-01-02 15:04:05"),
	}
	if _, err := reg.CreateBackupRun(ctx, run); err != nil {
		logErr("record backup run for %s: %v", job.DBName, err)
	}
}

// recordBackupFile writes one entry to the admin UI's per-database file
// list, best effort — a logging failure must never fail the job itself.
// Only called after a successful upload, so result is always non-nil.
func recordBackupFile(ctx context.Context, reg *registry.Registry, d *registry.Database, job queue.Job, result *uploadResult) {
	var databaseID int64
	if d != nil {
		databaseID = d.ID
	}
	file := registry.BackupFile{
		DatabaseID:      databaseID,
		DBName:          job.DBName,
		StorageTargetID: result.StorageTargetID,
		Filename:        result.Filename,
		RemoteRef:       result.RemoteRef,
		SizeBytes:       result.SizeBytes,
	}
	if _, err := reg.CreateBackupFile(ctx, file); err != nil {
		logErr("record backup file for %s: %v", job.DBName, err)
	}
}

// uploadResult carries the details recordBackupFile needs, once
// backupAndUpload's dump+upload has actually succeeded.
type uploadResult struct {
	Filename        string
	RemoteRef       string
	SizeBytes       int64
	StorageTargetID int64
}

func backupAndUpload(ctx context.Context, cfg *config.Config, reg *registry.Registry, job queue.Job) (*uploadResult, error) {
	if job.AgentID != 0 {
		return remoteBackupAndUpload(ctx, reg, job)
	}

	start := time.Now()
	params := dump.ParseParams(job.Params)

	ext, err := dump.Extension(job.Driver)
	if err != nil {
		return nil, err
	}

	date := time.Now().Format("060102")
	stamp := time.Now().Format("15h04")
	filename := fmt.Sprintf("%s_%s_%s.%s.gz", job.DBName, date, stamp, ext)
	outPath := filepath.Join(cfg.TmpDir, filename)

	dumpStart := time.Now()
	switch job.Driver {
	case "mysql":
		err = dump.MySQL(ctx, job.DBName, params, outPath)
	case "postgres":
		err = dump.Postgres(ctx, job.DBName, params, outPath)
	case "mongo":
		err = dump.Mongo(ctx, job.DBName, params, outPath)
	}
	if err != nil {
		return nil, fmt.Errorf("dump failed: %w", err)
	}
	dumpDuration := time.Since(dumpStart)

	var sizeMB float64
	if info, statErr := os.Stat(outPath); statErr == nil {
		sizeMB = float64(info.Size()) / 1024 / 1024
	}
	logLine("%s (%s): dump ok, %.2f MB, %s", job.DBName, job.Driver, sizeMB, round(dumpDuration))

	if job.StorageTargetID == 0 {
		return nil, fmt.Errorf("no storage destination selected for this database — assign one in the admin UI")
	}
	target, err := reg.GetStorageTarget(ctx, job.StorageTargetID)
	if err != nil {
		return nil, fmt.Errorf("load storage destination #%d: %w", job.StorageTargetID, err)
	}
	if target == nil {
		return nil, fmt.Errorf("storage destination #%d not found (was it deleted?)", job.StorageTargetID)
	}
	store, err := storage.Build(cfg, reg, *target)
	if err != nil {
		return nil, fmt.Errorf("init storage destination: %w", err)
	}

	uploadStart := time.Now()
	remoteRef, sizeBytes, err := store.Upload(ctx, job.DBName, date, filename, outPath)
	if err != nil {
		return nil, fmt.Errorf("upload failed: %w", err)
	}
	logLine("%s: upload ok -> %s %q, %s (tổng %s)", job.DBName, target.Kind, target.Label, round(time.Since(uploadStart)), round(time.Since(start)))

	_ = os.Remove(outPath)
	return &uploadResult{Filename: filename, RemoteRef: remoteRef, SizeBytes: sizeBytes, StorageTargetID: job.StorageTargetID}, nil
}

// remoteAgentPollInterval and remoteAgentMaxWait bound how the consumer
// waits on a job dispatched to a remote_agents server: poll gently (no
// point hammering a dump that takes minutes to run), but give up eventually
// rather than blocking every other queued job forever if an agent never
// reports "done" — a bug on its side, or it silently died mid-job.
const (
	remoteAgentPollInterval = 5 * time.Second
	remoteAgentMaxWait      = 6 * time.Hour
)

// remoteBackupAndUpload dispatches the dump+upload to a remote_agents
// server instead of running it in this process: this deployment either
// can't reach the target database directly, or isn't allowed to expose any
// inbound port for the agent to call back on, so the consumer pushes the
// job (internal/agentproto.Client.Run) and polls for the result itself —
// the only connections here are outbound, initiated by this side. On
// success the returned *uploadResult is identical in shape to the local
// path's, so every caller downstream (recordBackupRun/recordBackupFile/
// notify) needs no branching of its own.
func remoteBackupAndUpload(ctx context.Context, reg *registry.Registry, job queue.Job) (*uploadResult, error) {
	agent, err := reg.GetRemoteAgent(ctx, job.AgentID)
	if err != nil {
		return nil, fmt.Errorf("load remote agent #%d: %w", job.AgentID, err)
	}
	if agent == nil {
		return nil, fmt.Errorf("remote agent #%d not found (was it deleted?)", job.AgentID)
	}
	if job.StorageTargetID == 0 {
		return nil, fmt.Errorf("no storage destination selected for this database — assign one in the admin UI")
	}
	target, err := reg.GetStorageTarget(ctx, job.StorageTargetID)
	if err != nil {
		return nil, fmt.Errorf("load storage destination #%d: %w", job.StorageTargetID, err)
	}
	if target == nil {
		return nil, fmt.Errorf("storage destination #%d not found (was it deleted?)", job.StorageTargetID)
	}

	client := agentproto.NewClient(agent.Endpoint, agent.Token, agent.CertFingerprint)
	jobID, err := client.Run(ctx, agentproto.RunRequest{
		DBName: job.DBName,
		Driver: job.Driver,
		Params: job.Params,
		Storage: agentproto.StorageConfig{
			Kind: target.Kind, Label: target.Label, Config: target.Config,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("dispatch to agent %q: %w", agent.Label, err)
	}
	logLine("%s: dispatched to agent %q, job %s", job.DBName, agent.Label, jobID)

	deadline := time.Now().Add(remoteAgentMaxWait)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(remoteAgentPollInterval):
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("agent %q job %s: timed out after %s waiting for a result", agent.Label, jobID, remoteAgentMaxWait)
		}

		status, err := client.Status(ctx, jobID)
		if err != nil {
			logErr("%s: poll agent %q job %s: %v", job.DBName, agent.Label, jobID, err)
			continue
		}
		if status.Status != "done" {
			continue
		}
		if !status.Success {
			return nil, fmt.Errorf("agent %q: %s", agent.Label, status.Message)
		}
		return &uploadResult{
			Filename:        status.Filename,
			RemoteRef:       status.RemoteRef,
			SizeBytes:       status.SizeBytes,
			StorageTargetID: job.StorageTargetID,
		}, nil
	}
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
