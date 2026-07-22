package main

import (
	"fmt"
	"log"
	"net/http"

	"backupdb/internal/admin"
	"backupdb/internal/config"
	"backupdb/internal/queue"
	"backupdb/internal/registry"
)

func runAdmin(args []string) error {
	cfg := config.Load()

	reg, err := registry.Open(cfg.SQLitePath)
	if err != nil {
		return fmt.Errorf("open registry: %w", err)
	}
	defer reg.Close()

	q := queue.New(cfg.RedisHost, cfg.RedisPort)
	defer q.Close()

	if cfg.AdminUsername == "" || cfg.AdminPassword == "" {
		log.Println("WARNING: ADMIN_USERNAME/ADMIN_PASSWORD not set — admin UI is running WITHOUT authentication")
	}

	srv := admin.NewServer(cfg, reg, q, cfg.AdminUsername, cfg.AdminPassword, cfg.GoogleCredentialsFile, cfg.Timezone)
	addr := ":" + cfg.AdminPort
	log.Println("Admin UI listening on", addr)
	return http.ListenAndServe(addr, srv.Handler())
}
