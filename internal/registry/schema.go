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
	name              TEXT NOT NULL UNIQUE,
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
	enabled           INTEGER NOT NULL DEFAULT 1,
	created_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

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
`
