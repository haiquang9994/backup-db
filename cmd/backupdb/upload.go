package main

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"backupdb/internal/config"
	"backupdb/internal/registry"
	"backupdb/internal/storage"
)

// runUpload uploads a single file directly, bypassing the queue. The
// storage destination is either dbname's configured one (looked up in the
// registry), or an explicit 4th argument overriding it.
func runUpload(args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("usage: backupdb upload <dbname> <filepath> <filename> [storage-target-id]")
	}
	dbname, filePath, filename := args[0], args[1], args[2]

	cfg := config.Load()
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return fmt.Errorf("load timezone %s: %w", cfg.Timezone, err)
	}

	reg, err := registry.Open(cfg.SQLitePath)
	if err != nil {
		return fmt.Errorf("open registry: %w", err)
	}
	defer reg.Close()

	ctx := context.Background()

	// Looked up unconditionally (even when storage-target-id is passed
	// explicitly) so the upload can still be recorded against the right
	// database_id in backup_files below.
	d, err := reg.GetByName(ctx, dbname)
	if err != nil {
		return fmt.Errorf("look up %s in registry: %w", dbname, err)
	}

	var targetID int64
	if len(args) > 3 {
		targetID, err = strconv.ParseInt(args[3], 10, 64)
		if err != nil {
			return fmt.Errorf("invalid storage-target-id %q: %w", args[3], err)
		}
	} else {
		if d == nil {
			return fmt.Errorf("%s is not in the registry — pass a storage-target-id explicitly", dbname)
		}
		targetID = d.StorageTargetID
	}

	store, err := storage.New(ctx, cfg, reg, targetID)
	if err != nil {
		return fmt.Errorf("resolve storage destination: %w", err)
	}

	date := time.Now().In(loc).Format("060102")
	remoteRef, sizeBytes, err := store.Upload(ctx, dbname, date, filename, filePath)
	if err != nil {
		return err
	}

	var databaseID int64
	if d != nil {
		databaseID = d.ID
	}
	if _, err := reg.CreateBackupFile(ctx, registry.BackupFile{
		DatabaseID: databaseID, DBName: dbname, StorageTargetID: targetID,
		Filename: filename, RemoteRef: remoteRef, SizeBytes: sizeBytes,
	}); err != nil {
		return fmt.Errorf("record backup file: %w", err)
	}
	return nil
}
