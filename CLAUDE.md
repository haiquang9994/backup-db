# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`backupdb` is a Go service that backs up MySQL/PostgreSQL/MongoDB databases to Google Drive and/or S3-compatible storage (AWS S3, MinIO, Cloudflare R2, DigitalOcean Spaces...), driven by a Redis job queue and managed through a small web admin UI. It connects to target databases directly over TCP (host/port/user/pass) — no `docker exec`, no `/var/run/docker.sock` dependency.

## Commands

```bash
go build ./...              # build everything
go build -o backupdb ./cmd/backupdb  # build the single binary
go vet ./...                 # static checks
```

There are no automated tests in this repo currently.

Local run (needs `.env`, see `.env.example`, plus Redis and a SQLite path):

```bash
go run ./cmd/backupdb <subcommand>
```

Docker (the real deployment path):

```bash
docker compose up -d --build
```

### CLI subcommands (`backupdb <subcommand>`)

| Command | Purpose |
|---|---|
| `backup [dbname driver params]` | No args: enqueue a job for every enabled database in the registry. With args: push one ad-hoc job. |
| `consumer` | Worker loop: pop a job, dump, gzip, upload, notify. Runs forever. |
| `scheduler` | Poll the registry's `schedules` table every 30s, enqueue jobs when a schedule's time-of-day hits and hasn't already fired today. |
| `admin` | Basic-Auth web UI: manage databases, per-database and shared schedules, storage destinations, notify channels, remote agents. |
| `agent` | Standalone HTTPS server for a database only reachable from a different server (see "Remote agents" below). No SQLite/Redis. |
| `upload <dbname> <filepath> <filename> [targetID]` | Upload a single file directly, bypassing the queue. |
| `migrate <path-to-databases.txt>` | One-time import of a legacy flat-file database list into the SQLite registry. |

## Architecture

Four processes (see `docker-compose.yml`), each a thin `cmd/backupdb/*.go` entry point around the shared `internal/` packages:

```
redis      — job queue (RPUSH/BLPOP), queue name "backup_db_queue"
admin      — web UI: manage databases, schedules, storage destinations, notify channels (Basic Auth)
scheduler  — every 30s, checks SQLite schedules (per-database and shared), pushes jobs to redis when due
consumer   — pops jobs from redis, dumps (mysqldump/pg_dump/mongodump), gzips, uploads, notifies via each database's assigned channels
```

`admin`, `scheduler`, and `consumer` share one SQLite file (`internal/registry`, WAL mode for safe concurrent access across processes). `consumer` needs to be on the same Docker network as the target database containers to resolve their hostnames (the external `dbnet` network in `docker-compose.yml`). All three services build from and run the same Docker image (`image: backupdb:latest` in `docker-compose.yml`), so `docker compose up -d --build` builds it once.

A 5th, optional process — `agent` — runs on a *different* server, for a database this deployment can only reach from over there (different provider, no shared network) and where this deployment isn't allowed to expose any inbound port of its own to receive a callback. It has no SQLite/Redis of its own; the central `consumer`, for any database with a non-zero `agent_id`, dispatches the whole job to that `remote_agents` row over HTTPS (`internal/agentproto`) instead of dumping locally — every connection in that exchange is opened by the central side (push the job, then poll for the result), never the reverse, so the central server never needs to accept anything inbound either. See "Remote agents" below for the full protocol.

### Package layout

- `internal/config` — loads `.env` + process env into a `Config` struct. No validation at load time; each consumer of a missing value fails with its own clear error (e.g. consumer fails a specific job, not startup). `SchedulerTimezone` (env `SCHEDULER_TIMEZONE`, default `Asia/Ho_Chi_Minh`) is the one IANA zone every schedule's HH:MM is interpreted in, deployment-wide — schedules don't carry their own timezone.
- `internal/registry` — SQLite-backed source of truth: `databases` (what to back up and where, plus `agent_id` — 0 runs locally, non-zero routes through a `remote_agents` row), `schedules` (per-database HH:MM backup times, any number per database), `shared_schedules`/`shared_schedule_databases`/`shared_schedule_times` (one group of databases, any number of HH:MM triggers, for backing up several databases on the same schedule without duplicating the time on each one), `storage_targets` (configured upload destinations), `notify_channels`/`database_notify_channels` (configured notification destinations, any number assigned per database), `backup_runs` (append-only history log of every finished job, success or error, shown on the admin "Nhật ký" page in place of reading container stdout — no FK to `databases` since a run must stay visible after its database is renamed/deleted), `backup_files` (one row per successfully uploaded file — dbname, storage_target_id, filename, an opaque `remote_ref` (gdrive file ID or s3 object key), size — backing the admin UI's per-database "File" list/download page; same no-FK-on-`database_id` reasoning as `backup_runs`), `remote_agents` (one row per registered `agent` endpoint — label, endpoint URL, Bearer token, pinned TLS cert fingerprint; see "Remote agents" below). All `*_run_date`-tracked tables use that column to avoid double-firing the same day. Schema lives in `internal/registry/schema.go` and is applied idempotently (`CREATE TABLE IF NOT EXISTS`) on every `Open` — **no built-in migration system**, so a schema change requires either a hand-written one-time migration in `registry.Open` (see `migrateSharedScheduleTimes`/`migrateNotifyChannelEvents`/`migrateAgentIDColumn` for the pattern: check column presence via `PRAGMA table_info`, then `ALTER TABLE`/backfill) or wiping the `sqlite-data` volume.
- `internal/queue` — minimal Redis list wrapper (`RPUSH`/`BLPOP`). `Job` carries the driver, connection params packed into a pipe-delimited string (`host|port|user|pass|authDB`, see `queue.NewBackupJob` / `dump.ParseParams`), a `storage_target_id`, and an `agent_id` (0 = local).
- `internal/dump` — shells out to `mysqldump` / `pg_dump` / `mongodump` over TCP, streaming stdout through gzip into a tmp file. MySQL password is passed via `MYSQL_PWD` env var, never on the command line. `ParseParams` rewrites a `localhost`/`127.0.0.1` host to `host.docker.internal` (mapped via `extra_hosts` in `docker-compose.yml`), since the consumer's own loopback never has the target database — that's always meant to be the Docker host instead. `ParseParamsRaw` skips that rewrite, for callers not running inside that same container — namely `agent`, normally run as a bare process directly on the target database's own host, where `localhost` already means exactly that host.
- `internal/storage` — destination-agnostic `Provider` interface: `Upload(ctx, dbname, date, filename, localPath)` returns a kind-specific opaque `remoteRef` (gdrive file ID | s3 object key) plus the uploaded size, persisted to `backup_files` by the caller; `Download(ctx, remoteRef)` resolves it back, returning either a `redirectURL` (S3's presigned URL — the admin server 302s the browser straight to the bucket, no bytes touch our process) or a `body io.ReadCloser` (Google Drive — streamed through our own OAuth token, since Drive files here are never made publicly linkable). `storage.New`/`storage.Build` read a `storage_targets` row and dispatch on `Kind` (`"gdrive"` or `"s3"`) to build the concrete provider. **Adding a new storage kind = one new package implementing `Provider` + one new `case` in `storage.Build`.**
  - `internal/storage/gdrive` — uploads into a `{rootFolder}/{dbname}/{date}/` folder tree. OAuth tokens live in the registry (`storage_targets.config`, JSON), not on disk — only the OAuth *app* credentials (`google/credentials.json`) are a file, shared across every connected Google account. Token refreshes are persisted back to the registry automatically via a `TokenStore` implementation (`storage.registryTokenStore`).
  - `internal/storage/s3store` — talks to any S3-compatible endpoint via `minio-go`.
- `internal/admin` — `net/http` server (Go 1.22+ method+pattern mux, e.g. `"POST /edit/{id}"`), `html/template` views embedded via `go:embed` (`templates/*.html`, `static/*`). Optional Basic Auth (disabled with a startup warning if `ADMIN_USERNAME`/`ADMIN_PASSWORD` are blank). Google account linking is done via a copy-paste verification code flow (`gdrive.AuthURL` → user authorizes in browser → pastes the code shown at the unreachable `localhost` redirect back into the form) rather than a callback server, since admin may not be reachable from the internet.
- `internal/notify` — destination-agnostic `Channel` interface (`Send(ctx, message)`), mirroring `internal/storage`'s design: each database picks any number of `notify_channels` rows (Telegram today, more kinds later); a channel gets every event (success and failure) for every database it's assigned to, no per-channel event filtering. `notify.Build` dispatches on `Kind` to construct the concrete `Channel` — adding a new kind means one new file implementing `Channel` (unlike storage's kinds, notify's kinds live in the same package, not subpackages) plus one new `case` in `Build`. Telegram sends straight to the Bot API (`api.telegram.org/bot<token>/sendMessage`), no relay in between.
- `internal/agentproto` — the HTTP+TLS protocol between `consumer` and an `agent`: `RunRequest` (dbname/driver/params + the resolved `StorageConfig`, sent inline since the agent has no registry to look it up from), `POST /run` → `RunAccepted{JobID}` (agent queues the job and replies immediately — dump+upload can take minutes, too long to hold one connection open across the public internet), `GET /run/{jobID}` → `RunStatus` (`"pending"` or `"done"`, polled repeatedly). `Client` pins the agent's certificate by SHA-256 fingerprint (`agentproto.Fingerprint`) instead of trusting a public CA, since agents self-sign; `RequireToken` is the agent server's matching Bearer-auth middleware. On the agent side (`cmd/backupdb/agent.go`'s `jobStore`), `POST /run` only enqueues onto a buffered channel — a single worker goroutine drains it and runs jobs strictly one at a time, so several databases sharing one agent never run `mysqldump`/`pg_dump`/`mongodump` concurrently on that server. On the consumer side, dispatching to an agent (`remoteBackupAndUpload` in `cmd/backupdb/consumer.go`) runs in its own goroutine per job instead of blocking the main pop loop, since polling is mostly idle waiting — so a slow agent never stalls other databases (local or on a different agent); local jobs still process one at a time as before.

### Data flow for a backup

1. A job is enqueued either by `scheduler` (a per-database or shared schedule fires), `backup` with no args (all enabled databases), `backup` with args (one ad-hoc job), or the admin UI's "Backup now" button — all converge on `queue.NewBackupJob` / `queue.Client.Push`.
2. `consumer` pops the job. If `job.AgentID == 0` (the common case) it dumps + gzips to `cfg.TmpDir` itself, resolves `storage_target_id` via the registry, builds the matching `storage.Provider`, and uploads. If `job.AgentID != 0`, `remoteBackupAndUpload` instead resolves the `remote_agents` row and the storage target, dispatches the whole job to that agent over HTTPS (`internal/agentproto.Client.Run`), and polls `Client.Status` until it reports done — this path runs in its own goroutine so it never blocks other jobs (see `internal/agentproto`'s package note above). Either way the result funnels into the same `*uploadResult`, so everything downstream is identical: `consumer` deletes the local tmp file (local path only — the agent already deleted its own), looks up the database by name (`registry.GetByName`) to resolve its assigned `notify_channels` and dispatches a success or failure message to each (a job with no storage target configured fails only that job, not the whole consumer; a bad notify channel is logged and skipped, never fails the job). Every finished job — success or error — is also recorded to `backup_runs` (`registry.CreateBackupRun`), best-effort, so a logging failure never fails the job itself. A successful upload is additionally recorded to `backup_files` (`registry.CreateBackupFile`), same best-effort treatment — feeds the admin UI's per-database file list/download page. The `upload` CLI subcommand records the same way, since it also calls `Provider.Upload` directly (always locally — `upload` has no agent-routing of its own).

### Known limitations (intentional, not bugs to silently fix)

- Database passwords, S3 secret keys, and notify channel credentials (e.g. Telegram bot tokens) are stored as plaintext in SQLite — the admin port must stay off the public internet regardless of Basic Auth.
- No schema migrations: a breaking schema change means wiping `sqlite-data` or hand-writing a migration, not expecting in-place upgrade.
- `storage_target_id = 0` means "unset" and has no FK constraint (SQLite FK enforcement can't express "0 or a valid row"); a deleted-but-still-referenced target is instead caught at upload time with a clear error.
