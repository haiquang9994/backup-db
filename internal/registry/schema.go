package registry

// storage_targets holds every configured upload destination (one Google
// Drive account, or one S3-compatible bucket, per row) — `kind` picks which,
// `config` is a kind-specific JSON blob. Each database picks one via
// storage_target_id, so different databases can back up to different
// destinations.
const schema = `
CREATE TABLE IF NOT EXISTS storage_targets (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	kind       TEXT NOT NULL,               -- 'gdrive' | 's3'
	label      TEXT NOT NULL,
	config     TEXT NOT NULL,               -- kind-specific JSON
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS databases (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	-- Not UNIQUE on its own: the same name legitimately shows up twice, e.g.
	-- "shop" on two different agents, or a mysql "shop" and a mongo "shop"
	-- on the same agent. What must stay unique is the (name, agent_id,
	-- driver) triple — see idx_databases_name_agent_driver below, and
	-- queue.Job/Registry.GetByNameAgentDriver which resolve a job back to
	-- its row using exactly these three fields (a job only ever carries
	-- name, not the database's numeric id).
	name              TEXT NOT NULL,
	driver            TEXT NOT NULL,
	host              TEXT NOT NULL,
	port              TEXT NOT NULL DEFAULT '',
	username          TEXT NOT NULL DEFAULT '',
	password          TEXT NOT NULL DEFAULT '',
	auth_db           TEXT NOT NULL DEFAULT '',
	-- 0 means "not set yet". No REFERENCES here on purpose: SQLite's FK
	-- enforcement only exempts NULL, not a 0 sentinel with no matching
	-- storage_targets row, so a real FK would reject every unassigned
	-- database. Deleting a storage target still in use is instead handled
	-- at upload time (storage.New returns a clear "not found" error).
	storage_target_id INTEGER NOT NULL DEFAULT 0,
	-- 0 means "run locally" (the consumer on this same deployment) — same
	-- sentinel convention as storage_target_id. A non-zero value routes the
	-- job to a remote_agents row instead: the consumer dumps+uploads on
	-- that other server (reachable directly, no queue involved) and only
	-- polls it for the result.
	agent_id          INTEGER NOT NULL DEFAULT 0,
	enabled           INTEGER NOT NULL DEFAULT 1,
	created_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- idx_databases_name_agent_driver (the UNIQUE(name, agent_id, driver)
-- replacement for the old UNIQUE(name)) is created in Go, in Open(), after
-- migrateAgentIDColumn — not here, since an install upgrading from before
-- agent_id existed wouldn't have that column yet at the point this schema
-- string runs.

CREATE TABLE IF NOT EXISTS schedules (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	database_id   INTEGER NOT NULL REFERENCES databases(id) ON DELETE CASCADE,
	time_of_day   TEXT NOT NULL,              -- "HH:MM", 24h, Asia/Ho_Chi_Minh
	enabled       INTEGER NOT NULL DEFAULT 1,
	last_run_date TEXT NOT NULL DEFAULT '',   -- "YYYY-MM-DD" of the last trigger, prevents double-firing
	created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_schedules_database_id ON schedules(database_id);

-- shared_schedules groups any number of databases under one enabled switch
-- — shared_schedule_databases is a many-to-many join so one shared schedule
-- can back up any number of databases, and a database can be covered by any
-- number of shared schedules on top of its own per-database ones. The
-- actual "fire at HH:MM, once per day" triggers live in shared_schedule_times
-- below, any number of them per shared schedule, same idea as schedules
-- above but decoupled from a single time per group.
CREATE TABLE IF NOT EXISTS shared_schedules (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	enabled       INTEGER NOT NULL DEFAULT 1,
	created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS shared_schedule_databases (
	shared_schedule_id INTEGER NOT NULL REFERENCES shared_schedules(id) ON DELETE CASCADE,
	database_id        INTEGER NOT NULL REFERENCES databases(id) ON DELETE CASCADE,
	PRIMARY KEY (shared_schedule_id, database_id)
);

CREATE INDEX IF NOT EXISTS idx_shared_schedule_databases_database_id ON shared_schedule_databases(database_id);

CREATE TABLE IF NOT EXISTS shared_schedule_times (
	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	shared_schedule_id INTEGER NOT NULL REFERENCES shared_schedules(id) ON DELETE CASCADE,
	time_of_day        TEXT NOT NULL,
	last_run_date      TEXT NOT NULL DEFAULT '',
	created_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_shared_schedule_times_shared_schedule_id ON shared_schedule_times(shared_schedule_id);

-- notify_channels holds every configured notification destination (a
-- Telegram bot/chat today, more kinds later) — same kind/label/config shape
-- as storage_targets. A channel gets every event (success and failure) for
-- every database it's assigned to via database_notify_channels, a
-- many-to-many join so one database can notify through any number of
-- channels, and one channel can be reused across databases.
CREATE TABLE IF NOT EXISTS notify_channels (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	kind       TEXT NOT NULL,               -- 'telegram'
	label      TEXT NOT NULL,
	config     TEXT NOT NULL,               -- kind-specific JSON
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS database_notify_channels (
	database_id       INTEGER NOT NULL REFERENCES databases(id) ON DELETE CASCADE,
	notify_channel_id INTEGER NOT NULL REFERENCES notify_channels(id) ON DELETE CASCADE,
	PRIMARY KEY (database_id, notify_channel_id)
);

CREATE INDEX IF NOT EXISTS idx_database_notify_channels_channel_id ON database_notify_channels(notify_channel_id);

-- backup_runs is a history log of every job the consumer has finished
-- processing (success or error), shown on the admin "Nhật ký" page in place
-- of reading container stdout logs directly. database_id has no FK (same
-- reasoning as databases.storage_target_id) since a run must stay visible
-- even after its database is renamed or deleted; dbname/driver are captured
-- as of run time for that reason.
CREATE TABLE IF NOT EXISTS backup_runs (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	database_id INTEGER NOT NULL DEFAULT 0,
	dbname      TEXT NOT NULL,
	driver      TEXT NOT NULL,
	status      TEXT NOT NULL,               -- 'success' | 'error'
	message     TEXT NOT NULL DEFAULT '',
	duration_ms INTEGER NOT NULL DEFAULT 0,
	started_at  TEXT NOT NULL,
	created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_backup_runs_id_desc ON backup_runs(id DESC);

-- backup_files records one successfully uploaded backup file per row,
-- written right after the consumer's (or CLI upload command's) upload
-- succeeds, so the admin UI can list/download past backups without
-- querying the storage provider live for a directory listing. No FK on
-- database_id (same reasoning as backup_runs above). remote_ref is opaque
-- per storage kind (gdrive file ID | s3 object key) — meaningful only
-- together with storage_target_id, which picks which Provider.Download to
-- hand it to.
CREATE TABLE IF NOT EXISTS backup_files (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	database_id       INTEGER NOT NULL DEFAULT 0,
	dbname            TEXT NOT NULL,
	storage_target_id INTEGER NOT NULL DEFAULT 0,
	filename          TEXT NOT NULL,
	remote_ref        TEXT NOT NULL,
	size_bytes        INTEGER NOT NULL DEFAULT 0,
	created_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_backup_files_database_id_id_desc ON backup_files(database_id, id DESC);

-- remote_agents holds every configured "backupdb agent" endpoint — a
-- standalone HTTPS server (backupdb agent subcommand) running on a
-- different server that owns dump+upload for databases only it can reach
-- directly, when this deployment isn't allowed to expose any inbound port
-- of its own to reach that server's database over the network. token is a
-- shared secret sent as a Bearer header; cert_fingerprint pins the agent's
-- self-signed TLS certificate (SHA-256 of the DER-encoded leaf, hex) since
-- there is no public CA involved.
CREATE TABLE IF NOT EXISTS remote_agents (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	label            TEXT NOT NULL,
	endpoint         TEXT NOT NULL,
	token            TEXT NOT NULL,
	cert_fingerprint TEXT NOT NULL,
	created_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
`
