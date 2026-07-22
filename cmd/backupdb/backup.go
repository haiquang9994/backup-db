package main

import (
	"context"
	"fmt"

	"backupdb/internal/config"
	"backupdb/internal/queue"
	"backupdb/internal/registry"
)

// runBackup enqueues jobs onto the Redis queue. With no arguments it reads
// the enabled databases from the registry (the scheduled path); with
// arguments it pushes a single ad-hoc job (dbname [driver] [params]).
func runBackup(args []string) error {
	cfg := config.Load()

	q := queue.New(cfg.RedisHost, cfg.RedisPort)
	defer q.Close()

	ctx := context.Background()

	if len(args) > 0 {
		dbname := args[0]
		driver := "mysql"
		if len(args) > 1 {
			driver = args[1]
		}
		params := ""
		if len(args) > 2 {
			params = args[2]
		}
		if err := q.Push(ctx, queue.Job{Cmd: "backup", DBName: dbname, Driver: driver, Params: params}); err != nil {
			return fmt.Errorf("enqueue %s: %w", dbname, err)
		}
		fmt.Printf("Enqueued %s (%s)\n", dbname, driver)
		return nil
	}

	reg, err := registry.Open(cfg.SQLitePath)
	if err != nil {
		return fmt.Errorf("open registry: %w", err)
	}
	defer reg.Close()

	dbs, err := reg.ListEnabled(ctx)
	if err != nil {
		return fmt.Errorf("list enabled databases: %w", err)
	}

	for _, d := range dbs {
		job := queue.NewBackupJob(d.Name, d.Driver, d.Host, d.Port, d.Username, d.Password, d.AuthDB, d.StorageTargetID, d.AgentID)
		if err := q.Push(ctx, job); err != nil {
			return fmt.Errorf("enqueue %s: %w", d.Name, err)
		}
		fmt.Printf("Enqueued %s (%s)\n", d.Name, d.Driver)
	}
	return nil
}
