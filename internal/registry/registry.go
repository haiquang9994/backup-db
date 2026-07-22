// Package registry stores the list of databases to back up, their backup
// schedules, and their upload destinations in SQLite, replacing the old
// databases.txt file. It is read by `backup`/`consumer`/`scheduler` and
// read-written by the `admin` web UI.
package registry

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Database struct {
	ID              int64
	Name            string
	Driver          string
	Host            string
	Port            string
	Username        string
	Password        string
	AuthDB          string
	StorageTargetID int64 // 0 = none selected yet; see StorageTarget
	AgentID         int64 // 0 = run locally; see RemoteAgent
	Enabled         bool
	CreatedAt       string
	UpdatedAt       string
}

type Registry struct {
	db *sql.DB
}

// Open creates the SQLite file (and its parent directory) if needed, applies
// the schema, and returns a Registry. WAL mode lets the admin UI (writer)
// and scheduler/consumer (readers) run as separate processes against the
// same file without blocking each other.
func Open(path string) (*Registry, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create sqlite dir: %w", err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Serialize access from this process; cross-process concurrency is
	// handled by WAL mode + busy_timeout instead.
	db.SetMaxOpenConns(1)

	pragmas := []string{"PRAGMA journal_mode=WAL;", "PRAGMA busy_timeout=5000;", "PRAGMA foreign_keys=ON;"}
	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrateSharedScheduleTimes(db); err != nil {
		return nil, fmt.Errorf("migrate shared schedule times: %w", err)
	}
	if err := migrateNotifyChannelEvents(db); err != nil {
		return nil, fmt.Errorf("migrate notify channel events: %w", err)
	}
	if err := migrateAgentIDColumn(db); err != nil {
		return nil, fmt.Errorf("migrate agent_id column: %w", err)
	}

	return &Registry{db: db}, nil
}

// hasColumn checks sqlite_master via PRAGMA table_info — used to detect an
// old table shape left over from before a schema change, since CREATE TABLE
// IF NOT EXISTS never touches an already-existing table.
func hasColumn(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false, fmt.Errorf("check %s columns: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// migrateSharedScheduleTimes is a one-time, idempotent migration for
// installs that still have the old shared_schedules shape (one row = one
// time_of_day). It copies each row's time into the new shared_schedule_times
// table, then drops the now-redundant columns so ListSharedSchedules etc.
// (which no longer select them) work against the old table shape too.
func migrateSharedScheduleTimes(db *sql.DB) error {
	has, err := hasColumn(db, "shared_schedules", "time_of_day")
	if err != nil {
		return err
	}
	if !has {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		INSERT INTO shared_schedule_times (shared_schedule_id, time_of_day, last_run_date)
		SELECT id, time_of_day, last_run_date FROM shared_schedules
	`); err != nil {
		return fmt.Errorf("copy existing times: %w", err)
	}
	if _, err := tx.Exec("ALTER TABLE shared_schedules DROP COLUMN time_of_day"); err != nil {
		return fmt.Errorf("drop time_of_day: %w", err)
	}
	if _, err := tx.Exec("ALTER TABLE shared_schedules DROP COLUMN last_run_date"); err != nil {
		return fmt.Errorf("drop last_run_date: %w", err)
	}
	return tx.Commit()
}

// migrateNotifyChannelEvents is a one-time, idempotent migration dropping
// notify_channels.notify_on_success/notify_on_error — an initial version of
// the feature had per-channel event filtering, simplified away before it
// ever had real data: every channel now gets every event unconditionally.
func migrateNotifyChannelEvents(db *sql.DB) error {
	has, err := hasColumn(db, "notify_channels", "notify_on_success")
	if err != nil {
		return err
	}
	if !has {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec("ALTER TABLE notify_channels DROP COLUMN notify_on_success"); err != nil {
		return fmt.Errorf("drop notify_on_success: %w", err)
	}
	if _, err := tx.Exec("ALTER TABLE notify_channels DROP COLUMN notify_on_error"); err != nil {
		return fmt.Errorf("drop notify_on_error: %w", err)
	}
	return tx.Commit()
}

// migrateAgentIDColumn adds databases.agent_id for installs that predate
// remote agent support — CREATE TABLE IF NOT EXISTS never touches an
// already-existing databases table, so this ALTER TABLE is the only way an
// existing deployment picks up the new column.
func migrateAgentIDColumn(db *sql.DB) error {
	has, err := hasColumn(db, "databases", "agent_id")
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	_, err = db.Exec("ALTER TABLE databases ADD COLUMN agent_id INTEGER NOT NULL DEFAULT 0")
	return err
}

func (r *Registry) Close() error {
	return r.db.Close()
}

const columns = "id, name, driver, host, port, username, password, auth_db, storage_target_id, agent_id, enabled, created_at, updated_at"

func scanDatabase(row interface{ Scan(...any) error }) (Database, error) {
	var d Database
	var enabled int
	err := row.Scan(&d.ID, &d.Name, &d.Driver, &d.Host, &d.Port, &d.Username, &d.Password, &d.AuthDB, &d.StorageTargetID, &d.AgentID, &enabled, &d.CreatedAt, &d.UpdatedAt)
	d.Enabled = enabled != 0
	return d, err
}

func (r *Registry) List(ctx context.Context) ([]Database, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT "+columns+" FROM databases ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Database
	for rows.Next() {
		d, err := scanDatabase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (r *Registry) ListEnabled(ctx context.Context) ([]Database, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT "+columns+" FROM databases WHERE enabled = 1 ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Database
	for rows.Next() {
		d, err := scanDatabase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (r *Registry) Get(ctx context.Context, id int64) (*Database, error) {
	row := r.db.QueryRowContext(ctx, "SELECT "+columns+" FROM databases WHERE id = ?", id)
	d, err := scanDatabase(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (r *Registry) GetByName(ctx context.Context, name string) (*Database, error) {
	row := r.db.QueryRowContext(ctx, "SELECT "+columns+" FROM databases WHERE name = ?", name)
	d, err := scanDatabase(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (r *Registry) Create(ctx context.Context, d Database) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`INSERT INTO databases (name, driver, host, port, username, password, auth_db, storage_target_id, agent_id, enabled)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.Name, d.Driver, d.Host, d.Port, d.Username, d.Password, d.AuthDB, d.StorageTargetID, d.AgentID, boolToInt(d.Enabled),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (r *Registry) Update(ctx context.Context, d Database) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE databases
		 SET name = ?, driver = ?, host = ?, port = ?, username = ?, password = ?, auth_db = ?, storage_target_id = ?, agent_id = ?, enabled = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE id = ?`,
		d.Name, d.Driver, d.Host, d.Port, d.Username, d.Password, d.AuthDB, d.StorageTargetID, d.AgentID, boolToInt(d.Enabled), d.ID,
	)
	return err
}

func (r *Registry) SetEnabled(ctx context.Context, id int64, enabled bool) error {
	_, err := r.db.ExecContext(ctx,
		"UPDATE databases SET enabled = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		boolToInt(enabled), id,
	)
	return err
}

func (r *Registry) Delete(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM databases WHERE id = ?", id)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Schedule is one "run this database's backup at HH:MM every day" entry.
// A database can have any number of schedules, giving it multiple backups
// per day.
type Schedule struct {
	ID          int64
	DatabaseID  int64
	TimeOfDay   string // "HH:MM", 24h, interpreted in Asia/Ho_Chi_Minh
	Enabled     bool
	LastRunDate string // "YYYY-MM-DD"
	CreatedAt   string
}

// DueJob pairs a schedule with the database it belongs to, ready to enqueue.
type DueJob struct {
	ScheduleID int64
	Database   Database
}

func (r *Registry) ListSchedulesByDatabase(ctx context.Context, databaseID int64) ([]Schedule, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT id, database_id, time_of_day, enabled, last_run_date, created_at FROM schedules WHERE database_id = ? ORDER BY time_of_day",
		databaseID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Schedule
	for rows.Next() {
		var s Schedule
		var enabled int
		if err := rows.Scan(&s.ID, &s.DatabaseID, &s.TimeOfDay, &enabled, &s.LastRunDate, &s.CreatedAt); err != nil {
			return nil, err
		}
		s.Enabled = enabled != 0
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *Registry) GetSchedule(ctx context.Context, id int64) (*Schedule, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT id, database_id, time_of_day, enabled, last_run_date, created_at FROM schedules WHERE id = ?", id,
	)
	var s Schedule
	var enabled int
	err := row.Scan(&s.ID, &s.DatabaseID, &s.TimeOfDay, &enabled, &s.LastRunDate, &s.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.Enabled = enabled != 0
	return &s, nil
}

func (r *Registry) CreateSchedule(ctx context.Context, databaseID int64, timeOfDay string) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		"INSERT INTO schedules (database_id, time_of_day, enabled) VALUES (?, ?, 1)",
		databaseID, timeOfDay,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (r *Registry) DeleteSchedule(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM schedules WHERE id = ?", id)
	return err
}

// ListDueSchedules returns every enabled schedule, on an enabled database,
// whose time_of_day matches now and that has not already fired today.
func (r *Registry) ListDueSchedules(ctx context.Context, now time.Time) ([]DueJob, error) {
	hhmm := now.Format("15:04")
	today := now.Format("2006-01-02")

	rows, err := r.db.QueryContext(ctx, `
		SELECT s.id, d.id, d.name, d.driver, d.host, d.port, d.username, d.password, d.auth_db, d.storage_target_id, d.agent_id, d.enabled, d.created_at, d.updated_at
		FROM schedules s
		JOIN databases d ON d.id = s.database_id
		WHERE s.enabled = 1 AND d.enabled = 1 AND s.time_of_day = ? AND s.last_run_date != ?
	`, hhmm, today)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DueJob
	for rows.Next() {
		var scheduleID int64
		var d Database
		var enabled int
		if err := rows.Scan(&scheduleID, &d.ID, &d.Name, &d.Driver, &d.Host, &d.Port, &d.Username, &d.Password, &d.AuthDB, &d.StorageTargetID, &d.AgentID, &enabled, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		d.Enabled = enabled != 0
		out = append(out, DueJob{ScheduleID: scheduleID, Database: d})
	}
	return out, rows.Err()
}

func (r *Registry) MarkScheduleRun(ctx context.Context, id int64, date string) error {
	_, err := r.db.ExecContext(ctx, "UPDATE schedules SET last_run_date = ? WHERE id = ?", date, id)
	return err
}

// SharedSchedule groups any number of databases (see
// shared_schedule_databases) under one enabled switch; the actual
// "fire at HH:MM, once per day" triggers are its Times, any number of them.
type SharedSchedule struct {
	ID        int64
	Enabled   bool
	CreatedAt string
	Times     []SharedScheduleTime
	Databases []Database // member databases, loaded via join
}

// SharedScheduleTime is one HH:MM trigger belonging to a SharedSchedule,
// like Schedule but for a group of databases instead of just one.
type SharedScheduleTime struct {
	ID               int64
	SharedScheduleID int64
	TimeOfDay        string
	LastRunDate      string
	CreatedAt        string
}

func (r *Registry) ListSharedSchedules(ctx context.Context) ([]SharedSchedule, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT id, enabled, created_at FROM shared_schedules ORDER BY id",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SharedSchedule
	for rows.Next() {
		var s SharedSchedule
		var enabled int
		if err := rows.Scan(&s.ID, &enabled, &s.CreatedAt); err != nil {
			return nil, err
		}
		s.Enabled = enabled != 0
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range out {
		times, err := r.ListSharedScheduleTimes(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Times = times
		dbs, err := r.ListDatabasesForSharedSchedule(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Databases = dbs
	}
	return out, nil
}

func (r *Registry) GetSharedSchedule(ctx context.Context, id int64) (*SharedSchedule, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT id, enabled, created_at FROM shared_schedules WHERE id = ?", id,
	)
	var s SharedSchedule
	var enabled int
	err := row.Scan(&s.ID, &enabled, &s.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.Enabled = enabled != 0

	times, err := r.ListSharedScheduleTimes(ctx, id)
	if err != nil {
		return nil, err
	}
	s.Times = times

	dbs, err := r.ListDatabasesForSharedSchedule(ctx, id)
	if err != nil {
		return nil, err
	}
	s.Databases = dbs
	return &s, nil
}

// ListSharedScheduleTimes returns every HH:MM trigger belonging to a shared
// schedule, ordered for stable display.
func (r *Registry) ListSharedScheduleTimes(ctx context.Context, sharedScheduleID int64) ([]SharedScheduleTime, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT id, shared_schedule_id, time_of_day, last_run_date, created_at FROM shared_schedule_times WHERE shared_schedule_id = ? ORDER BY time_of_day",
		sharedScheduleID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SharedScheduleTime
	for rows.Next() {
		var t SharedScheduleTime
		if err := rows.Scan(&t.ID, &t.SharedScheduleID, &t.TimeOfDay, &t.LastRunDate, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *Registry) GetSharedScheduleTime(ctx context.Context, id int64) (*SharedScheduleTime, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT id, shared_schedule_id, time_of_day, last_run_date, created_at FROM shared_schedule_times WHERE id = ?", id,
	)
	var t SharedScheduleTime
	err := row.Scan(&t.ID, &t.SharedScheduleID, &t.TimeOfDay, &t.LastRunDate, &t.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (r *Registry) CreateSharedScheduleTime(ctx context.Context, sharedScheduleID int64, timeOfDay string) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		"INSERT INTO shared_schedule_times (shared_schedule_id, time_of_day) VALUES (?, ?)",
		sharedScheduleID, timeOfDay,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (r *Registry) DeleteSharedScheduleTime(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM shared_schedule_times WHERE id = ?", id)
	return err
}

// ListDatabasesForSharedSchedule returns the databases a shared schedule
// currently applies to.
func (r *Registry) ListDatabasesForSharedSchedule(ctx context.Context, sharedScheduleID int64) ([]Database, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT d.id, d.name, d.driver, d.host, d.port, d.username, d.password, d.auth_db, d.storage_target_id, d.agent_id, d.enabled, d.created_at, d.updated_at
		FROM databases d
		JOIN shared_schedule_databases ssd ON ssd.database_id = d.id
		WHERE ssd.shared_schedule_id = ?
		ORDER BY d.name
	`, sharedScheduleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Database
	for rows.Next() {
		d, err := scanDatabase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (r *Registry) CreateSharedSchedule(ctx context.Context, databaseIDs []int64) (int64, error) {
	res, err := r.db.ExecContext(ctx, "INSERT INTO shared_schedules DEFAULT VALUES")
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := r.setSharedScheduleDatabases(ctx, id, databaseIDs); err != nil {
		return 0, err
	}
	return id, nil
}

// UpdateSharedSchedule overwrites the full set of member databases (times
// are managed separately via CreateSharedScheduleTime/DeleteSharedScheduleTime).
func (r *Registry) UpdateSharedSchedule(ctx context.Context, id int64, databaseIDs []int64) error {
	return r.setSharedScheduleDatabases(ctx, id, databaseIDs)
}

func (r *Registry) setSharedScheduleDatabases(ctx context.Context, sharedScheduleID int64, databaseIDs []int64) error {
	if _, err := r.db.ExecContext(ctx, "DELETE FROM shared_schedule_databases WHERE shared_schedule_id = ?", sharedScheduleID); err != nil {
		return err
	}
	for _, dbID := range databaseIDs {
		if _, err := r.db.ExecContext(ctx,
			"INSERT INTO shared_schedule_databases (shared_schedule_id, database_id) VALUES (?, ?)",
			sharedScheduleID, dbID,
		); err != nil {
			return err
		}
	}
	return nil
}

func (r *Registry) SetSharedScheduleEnabled(ctx context.Context, id int64, enabled bool) error {
	_, err := r.db.ExecContext(ctx, "UPDATE shared_schedules SET enabled = ? WHERE id = ?", boolToInt(enabled), id)
	return err
}

func (r *Registry) DeleteSharedSchedule(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM shared_schedules WHERE id = ?", id)
	return err
}

// ListDueSharedSchedules mirrors ListDueSchedules, but returns one row per
// (shared schedule time, member database) pair — callers mark that time's
// run once per row, which is idempotent (same date each time).
func (r *Registry) ListDueSharedSchedules(ctx context.Context, now time.Time) ([]DueJob, error) {
	hhmm := now.Format("15:04")
	today := now.Format("2006-01-02")

	rows, err := r.db.QueryContext(ctx, `
		SELECT sst.id, d.id, d.name, d.driver, d.host, d.port, d.username, d.password, d.auth_db, d.storage_target_id, d.agent_id, d.enabled, d.created_at, d.updated_at
		FROM shared_schedule_times sst
		JOIN shared_schedules ss ON ss.id = sst.shared_schedule_id
		JOIN shared_schedule_databases ssd ON ssd.shared_schedule_id = ss.id
		JOIN databases d ON d.id = ssd.database_id
		WHERE ss.enabled = 1 AND d.enabled = 1 AND sst.time_of_day = ? AND sst.last_run_date != ?
	`, hhmm, today)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DueJob
	for rows.Next() {
		var scheduleTimeID int64
		var d Database
		var enabled int
		if err := rows.Scan(&scheduleTimeID, &d.ID, &d.Name, &d.Driver, &d.Host, &d.Port, &d.Username, &d.Password, &d.AuthDB, &d.StorageTargetID, &d.AgentID, &enabled, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		d.Enabled = enabled != 0
		out = append(out, DueJob{ScheduleID: scheduleTimeID, Database: d})
	}
	return out, rows.Err()
}

func (r *Registry) MarkSharedScheduleRun(ctx context.Context, id int64, date string) error {
	_, err := r.db.ExecContext(ctx, "UPDATE shared_schedule_times SET last_run_date = ? WHERE id = ?", date, id)
	return err
}

// StorageTarget is one configured upload destination: a Google Drive
// account (Kind "gdrive") or an S3-compatible bucket (Kind "s3"). Config is
// a kind-specific JSON blob — internal/storage knows how to interpret it
// for each kind.
type StorageTarget struct {
	ID        int64
	Kind      string
	Label     string
	Config    string
	CreatedAt string
	UpdatedAt string
}

func (r *Registry) ListStorageTargets(ctx context.Context) ([]StorageTarget, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT id, kind, label, config, created_at, updated_at FROM storage_targets ORDER BY kind, label",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StorageTarget
	for rows.Next() {
		var t StorageTarget
		if err := rows.Scan(&t.ID, &t.Kind, &t.Label, &t.Config, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *Registry) GetStorageTarget(ctx context.Context, id int64) (*StorageTarget, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT id, kind, label, config, created_at, updated_at FROM storage_targets WHERE id = ?", id,
	)
	var t StorageTarget
	err := row.Scan(&t.ID, &t.Kind, &t.Label, &t.Config, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (r *Registry) CreateStorageTarget(ctx context.Context, kind, label, config string) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		"INSERT INTO storage_targets (kind, label, config) VALUES (?, ?, ?)",
		kind, label, config,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateStorageTargetConfig overwrites just the config blob, e.g. to persist
// a refreshed Google OAuth token.
func (r *Registry) UpdateStorageTargetConfig(ctx context.Context, id int64, config string) error {
	_, err := r.db.ExecContext(ctx,
		"UPDATE storage_targets SET config = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		config, id,
	)
	return err
}

// UpdateStorageTargetLabel renames a target in place — used for a
// Google Drive "rename only" edit that doesn't touch its OAuth token.
func (r *Registry) UpdateStorageTargetLabel(ctx context.Context, id int64, label string) error {
	_, err := r.db.ExecContext(ctx,
		"UPDATE storage_targets SET label = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		label, id,
	)
	return err
}

// UpdateStorageTargetLabelConfig overwrites both the label and config blob —
// used by the S3 edit form, which resubmits every field at once.
func (r *Registry) UpdateStorageTargetLabelConfig(ctx context.Context, id int64, label, config string) error {
	_, err := r.db.ExecContext(ctx,
		"UPDATE storage_targets SET label = ?, config = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		label, config, id,
	)
	return err
}

func (r *Registry) DeleteStorageTarget(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM storage_targets WHERE id = ?", id)
	return err
}

// NotifyChannel is one configured notification destination (a Telegram
// bot/chat today) — same kind/label/config shape as StorageTarget. A channel
// gets every event (success and failure) for every database it's assigned
// to via database_notify_channels; a database picks any number of channels.
type NotifyChannel struct {
	ID        int64
	Kind      string
	Label     string
	Config    string
	CreatedAt string
	UpdatedAt string
}

func scanNotifyChannel(row interface{ Scan(...any) error }) (NotifyChannel, error) {
	var c NotifyChannel
	err := row.Scan(&c.ID, &c.Kind, &c.Label, &c.Config, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

const notifyChannelColumns = "id, kind, label, config, created_at, updated_at"

func (r *Registry) ListNotifyChannels(ctx context.Context) ([]NotifyChannel, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+notifyChannelColumns+" FROM notify_channels ORDER BY kind, label",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []NotifyChannel
	for rows.Next() {
		c, err := scanNotifyChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *Registry) GetNotifyChannel(ctx context.Context, id int64) (*NotifyChannel, error) {
	row := r.db.QueryRowContext(ctx, "SELECT "+notifyChannelColumns+" FROM notify_channels WHERE id = ?", id)
	c, err := scanNotifyChannel(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ListNotifyChannelsForDatabase returns the channels a database currently
// notifies through.
func (r *Registry) ListNotifyChannelsForDatabase(ctx context.Context, databaseID int64) ([]NotifyChannel, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT nc.id, nc.kind, nc.label, nc.config, nc.created_at, nc.updated_at
		FROM notify_channels nc
		JOIN database_notify_channels dnc ON dnc.notify_channel_id = nc.id
		WHERE dnc.database_id = ?
		ORDER BY nc.kind, nc.label
	`, databaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []NotifyChannel
	for rows.Next() {
		c, err := scanNotifyChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *Registry) CreateNotifyChannel(ctx context.Context, kind, label, config string) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		"INSERT INTO notify_channels (kind, label, config) VALUES (?, ?, ?)",
		kind, label, config,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateNotifyChannel overwrites the label and config blob — used by each
// kind's edit form, which resubmits every field at once.
func (r *Registry) UpdateNotifyChannel(ctx context.Context, id int64, label, config string) error {
	_, err := r.db.ExecContext(ctx,
		"UPDATE notify_channels SET label = ?, config = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		label, config, id,
	)
	return err
}

func (r *Registry) DeleteNotifyChannel(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM notify_channels WHERE id = ?", id)
	return err
}

// SetDatabaseNotifyChannels overwrites the full set of channels a database
// notifies through (the database form resubmits the whole set at once, same
// as setSharedScheduleDatabases).
func (r *Registry) SetDatabaseNotifyChannels(ctx context.Context, databaseID int64, channelIDs []int64) error {
	if _, err := r.db.ExecContext(ctx, "DELETE FROM database_notify_channels WHERE database_id = ?", databaseID); err != nil {
		return err
	}
	for _, channelID := range channelIDs {
		if _, err := r.db.ExecContext(ctx,
			"INSERT INTO database_notify_channels (database_id, notify_channel_id) VALUES (?, ?)",
			databaseID, channelID,
		); err != nil {
			return err
		}
	}
	return nil
}

// BackupRun is one finished consumer job, success or error, shown on the
// admin "Nhật ký" page. DatabaseID is 0 if the database has since been
// deleted or renamed; DBName/Driver are captured as of run time so the log
// stays meaningful either way.
type BackupRun struct {
	ID         int64
	DatabaseID int64
	DBName     string
	Driver     string
	Status     string // "success" | "error"
	Message    string
	DurationMS int64
	StartedAt  string
	CreatedAt  string
}

// CreateBackupRun records one finished job. Called by the consumer after
// every job, success or failure.
func (r *Registry) CreateBackupRun(ctx context.Context, run BackupRun) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`INSERT INTO backup_runs (database_id, dbname, driver, status, message, duration_ms, started_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		run.DatabaseID, run.DBName, run.Driver, run.Status, run.Message, run.DurationMS, run.StartedAt,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListBackupRuns returns one page of runs, newest first — backs the admin
// UI's "Nhật ký" page.
func (r *Registry) ListBackupRuns(ctx context.Context, limit, offset int) ([]BackupRun, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT id, database_id, dbname, driver, status, message, duration_ms, started_at, created_at FROM backup_runs ORDER BY id DESC LIMIT ? OFFSET ?",
		limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []BackupRun
	for rows.Next() {
		var run BackupRun
		if err := rows.Scan(&run.ID, &run.DatabaseID, &run.DBName, &run.Driver, &run.Status, &run.Message, &run.DurationMS, &run.StartedAt, &run.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

// CountBackupRuns returns the total number of rows in backup_runs, used to
// compute the "Nhật ký" page's total page count.
func (r *Registry) CountBackupRuns(ctx context.Context) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM backup_runs").Scan(&count)
	return count, err
}

// DeleteAllBackupRuns clears the entire log — backs the admin UI's "Xóa
// toàn bộ nhật ký" button.
func (r *Registry) DeleteAllBackupRuns(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM backup_runs")
	return err
}

// BackupFile is one file successfully uploaded to a storage destination,
// recorded right after upload so the admin UI can list/download past
// backups per database without querying the storage provider live.
// DatabaseID is 0 if the database has since been deleted or renamed; DBName
// is captured as of upload time so the record stays meaningful either way.
// RemoteRef is opaque per storage kind (Google Drive file ID, or S3 object
// key) — pass it back to the matching storage_target_id's Provider.Download
// to fetch the file.
type BackupFile struct {
	ID              int64
	DatabaseID      int64
	DBName          string
	StorageTargetID int64
	Filename        string
	RemoteRef       string
	SizeBytes       int64
	CreatedAt       string
}

// CreateBackupFile records one successful upload.
func (r *Registry) CreateBackupFile(ctx context.Context, f BackupFile) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`INSERT INTO backup_files (database_id, dbname, storage_target_id, filename, remote_ref, size_bytes)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		f.DatabaseID, f.DBName, f.StorageTargetID, f.Filename, f.RemoteRef, f.SizeBytes,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListBackupFilesByDatabase returns files uploaded for one database, newest
// first, capped at limit.
func (r *Registry) ListBackupFilesByDatabase(ctx context.Context, databaseID int64, limit int) ([]BackupFile, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, database_id, dbname, storage_target_id, filename, remote_ref, size_bytes, created_at
		 FROM backup_files WHERE database_id = ? ORDER BY id DESC LIMIT ?`,
		databaseID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []BackupFile
	for rows.Next() {
		var f BackupFile
		if err := rows.Scan(&f.ID, &f.DatabaseID, &f.DBName, &f.StorageTargetID, &f.Filename, &f.RemoteRef, &f.SizeBytes, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetBackupFile looks up one file record, used by the download handler to
// resolve which storage target + remote_ref to fetch.
func (r *Registry) GetBackupFile(ctx context.Context, id int64) (*BackupFile, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT id, database_id, dbname, storage_target_id, filename, remote_ref, size_bytes, created_at
		 FROM backup_files WHERE id = ?`, id)
	var f BackupFile
	err := row.Scan(&f.ID, &f.DatabaseID, &f.DBName, &f.StorageTargetID, &f.Filename, &f.RemoteRef, &f.SizeBytes, &f.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// RemoteAgent is one configured `backupdb agent` endpoint — a database
// picking a non-zero AgentID routes its backup through that server instead
// of the local consumer. Token is the shared secret sent as a Bearer
// header; CertFingerprint pins the agent's self-signed TLS certificate
// (SHA-256 of the DER-encoded leaf, hex) since there's no public CA here.
type RemoteAgent struct {
	ID              int64
	Label           string
	Endpoint        string
	Token           string
	CertFingerprint string
	CreatedAt       string
	UpdatedAt       string
}

func (r *Registry) ListRemoteAgents(ctx context.Context) ([]RemoteAgent, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT id, label, endpoint, token, cert_fingerprint, created_at, updated_at FROM remote_agents ORDER BY label",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RemoteAgent
	for rows.Next() {
		var a RemoteAgent
		if err := rows.Scan(&a.ID, &a.Label, &a.Endpoint, &a.Token, &a.CertFingerprint, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *Registry) GetRemoteAgent(ctx context.Context, id int64) (*RemoteAgent, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT id, label, endpoint, token, cert_fingerprint, created_at, updated_at FROM remote_agents WHERE id = ?", id,
	)
	var a RemoteAgent
	err := row.Scan(&a.ID, &a.Label, &a.Endpoint, &a.Token, &a.CertFingerprint, &a.CreatedAt, &a.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (r *Registry) CreateRemoteAgent(ctx context.Context, a RemoteAgent) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		"INSERT INTO remote_agents (label, endpoint, token, cert_fingerprint) VALUES (?, ?, ?, ?)",
		a.Label, a.Endpoint, a.Token, a.CertFingerprint,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (r *Registry) UpdateRemoteAgent(ctx context.Context, a RemoteAgent) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE remote_agents SET label = ?, endpoint = ?, token = ?, cert_fingerprint = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE id = ?`,
		a.Label, a.Endpoint, a.Token, a.CertFingerprint, a.ID,
	)
	return err
}

func (r *Registry) DeleteRemoteAgent(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM remote_agents WHERE id = ?", id)
	return err
}
