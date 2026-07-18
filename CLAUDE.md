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
| `admin` | Basic-Auth web UI: manage databases, schedules, storage destinations. |
| `upload <dbname> <filepath> <filename> [targetID]` | Upload a single file directly, bypassing the queue. |
| `migrate <path-to-databases.txt>` | One-time import of a legacy flat-file database list into the SQLite registry. |

## Architecture

Four processes (see `docker-compose.yml`), each a thin `cmd/backupdb/*.go` entry point around the shared `internal/` packages:

```
redis      — job queue (RPUSH/BLPOP), queue name "backup_db_queue"
admin      — web UI: manage databases, schedules, storage destinations (Basic Auth)
scheduler  — every 30s, checks SQLite schedules, pushes jobs to redis when due
consumer   — pops jobs from redis, dumps (mysqldump/pg_dump/mongodump), gzips, uploads, notifies via Telegram
```

`admin`, `scheduler`, and `consumer` share one SQLite file (`internal/registry`, WAL mode for safe concurrent access across processes). `consumer` needs to be on the same Docker network as the target database containers to resolve their hostnames (the external `dbnet` network in `docker-compose.yml`).

### Package layout

- `internal/config` — loads `.env` + process env into a `Config` struct. No validation at load time; each consumer of a missing value fails with its own clear error (e.g. consumer fails a specific job, not startup).
- `internal/registry` — SQLite-backed source of truth for three tables: `databases` (what to back up and where), `schedules` (per-database HH:MM backup times, any number per database, tracked via `last_run_date` to avoid double-firing), `storage_targets` (configured upload destinations). Schema lives in `internal/registry/schema.go` and is applied idempotently (`CREATE TABLE IF NOT EXISTS`) on every `Open` — **no migration system**, so a schema change requires either a manual `ALTER` path or wiping the `sqlite-data` volume.
- `internal/queue` — minimal Redis list wrapper (`RPUSH`/`BLPOP`). `Job` carries the driver, connection params packed into a pipe-delimited string (`host|port|user|pass|authDB`, see `queue.NewBackupJob` / `dump.ParseParams`), and a `storage_target_id`.
- `internal/dump` — shells out to `mysqldump` / `pg_dump` / `mongodump` over TCP, streaming stdout through gzip into a tmp file. MySQL password is passed via `MYSQL_PWD` env var, never on the command line.
- `internal/storage` — destination-agnostic `Provider` interface (`Upload(ctx, dbname, date, filename, localPath)`). `storage.New`/`storage.Build` read a `storage_targets` row and dispatch on `Kind` (`"gdrive"` or `"s3"`) to build the concrete provider. **Adding a new storage kind = one new package implementing `Provider` + one new `case` in `storage.Build`.**
  - `internal/storage/gdrive` — uploads into a `{rootFolder}/{dbname}/{date}/` folder tree. OAuth tokens live in the registry (`storage_targets.config`, JSON), not on disk — only the OAuth *app* credentials (`google/credentials.json`) are a file, shared across every connected Google account. Token refreshes are persisted back to the registry automatically via a `TokenStore` implementation (`storage.registryTokenStore`).
  - `internal/storage/s3store` — talks to any S3-compatible endpoint via `minio-go`.
- `internal/admin` — `net/http` server (Go 1.22+ method+pattern mux, e.g. `"POST /edit/{id}"`), `html/template` views embedded via `go:embed` (`templates/*.html`, `static/*`). Optional Basic Auth (disabled with a startup warning if `ADMIN_USERNAME`/`ADMIN_PASSWORD` are blank). Google account linking is done via a copy-paste verification code flow (`gdrive.AuthURL` → user authorizes in browser → pastes the code shown at the unreachable `localhost` redirect back into the form) rather than a callback server, since admin may not be reachable from the internet.
- `internal/notify` — posts backup success/failure to Telegram via an external relay endpoint (`telegram-pusher`), using separate bot/chat pairs for errors vs. success logs.

### Data flow for a backup

1. A job is enqueued either by `scheduler` (registry schedule fires), `backup` with no args (all enabled databases), `backup` with args (one ad-hoc job), or the admin UI's "Backup now" button — all converge on `queue.NewBackupJob` / `queue.Client.Push`.
2. `consumer` pops the job, dumps + gzips to `cfg.TmpDir`, resolves the job's `storage_target_id` via the registry, builds the matching `storage.Provider`, uploads, deletes the local tmp file, and notifies Telegram (success or failure — a job with no storage target configured fails only that job, not the whole consumer).

### Known limitations (intentional, not bugs to silently fix)

- Database passwords and S3 secret keys are stored as plaintext in SQLite — the admin port must stay off the public internet regardless of Basic Auth.
- No schema migrations: a breaking schema change means wiping `sqlite-data` or hand-writing a migration, not expecting in-place upgrade.
- `storage_target_id = 0` means "unset" and has no FK constraint (SQLite FK enforcement can't express "0 or a valid row"); a deleted-but-still-referenced target is instead caught at upload time with a clear error.
