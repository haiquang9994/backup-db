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

	return &Registry{db: db}, nil
}

func (r *Registry) Close() error {
	return r.db.Close()
}

const columns = "id, name, driver, host, port, username, password, auth_db, storage_target_id, enabled, created_at, updated_at"

func scanDatabase(row interface{ Scan(...any) error }) (Database, error) {
	var d Database
	var enabled int
	err := row.Scan(&d.ID, &d.Name, &d.Driver, &d.Host, &d.Port, &d.Username, &d.Password, &d.AuthDB, &d.StorageTargetID, &enabled, &d.CreatedAt, &d.UpdatedAt)
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
		`INSERT INTO databases (name, driver, host, port, username, password, auth_db, storage_target_id, enabled)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.Name, d.Driver, d.Host, d.Port, d.Username, d.Password, d.AuthDB, d.StorageTargetID, boolToInt(d.Enabled),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (r *Registry) Update(ctx context.Context, d Database) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE databases
		 SET name = ?, driver = ?, host = ?, port = ?, username = ?, password = ?, auth_db = ?, storage_target_id = ?, enabled = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE id = ?`,
		d.Name, d.Driver, d.Host, d.Port, d.Username, d.Password, d.AuthDB, d.StorageTargetID, boolToInt(d.Enabled), d.ID,
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
		SELECT s.id, d.id, d.name, d.driver, d.host, d.port, d.username, d.password, d.auth_db, d.storage_target_id, d.enabled, d.created_at, d.updated_at
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
		if err := rows.Scan(&scheduleID, &d.ID, &d.Name, &d.Driver, &d.Host, &d.Port, &d.Username, &d.Password, &d.AuthDB, &d.StorageTargetID, &enabled, &d.CreatedAt, &d.UpdatedAt); err != nil {
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

func (r *Registry) DeleteStorageTarget(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM storage_targets WHERE id = ?", id)
	return err
}
