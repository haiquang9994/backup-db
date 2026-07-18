package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"backupdb/internal/config"
	"backupdb/internal/registry"
)

// runMigrate imports a legacy PHP-era databases.txt file into the SQLite
// registry. The old format only carried a container name (for docker_*
// drivers) with no port, so Host is set from that field and Port is left
// blank — dump.MySQL/Postgres/Mongo fall back to the driver's default port
// in that case. Entries should be reviewed in the admin UI afterwards.
func runMigrate(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: backupdb migrate <path-to-databases.txt>")
	}

	f, err := os.Open(args[0])
	if err != nil {
		return err
	}
	defer f.Close()

	cfg := config.Load()
	reg, err := registry.Open(cfg.SQLitePath)
	if err != nil {
		return fmt.Errorf("open registry: %w", err)
	}
	defer reg.Close()

	ctx := context.Background()
	scanner := bufio.NewScanner(f)
	imported := 0

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, ":", 3)
		name := parts[0]
		driver := "mysql"
		if len(parts) > 1 && parts[1] != "" {
			driver = parts[1]
		}
		var rawParams string
		if len(parts) > 2 {
			rawParams = parts[2]
		}

		p := strings.Split(rawParams, "|")
		get := func(i int) string {
			if i < len(p) {
				return p[i]
			}
			return ""
		}

		d := registry.Database{Name: name, Enabled: true}
		switch driver {
		case "docker_mysql", "mysql":
			d.Driver = "mysql"
			d.Host, d.Username, d.Password = get(0), get(1), get(2)
		case "docker_postgres", "postgres":
			d.Driver = "postgres"
			d.Host, d.Username, d.Password = get(0), get(1), get(2)
		case "docker_mongo", "mongo":
			d.Driver = "mongo"
			d.Host, d.Username, d.Password, d.AuthDB = get(0), get(1), get(2), get(3)
		default:
			fmt.Printf("skip %s: unknown driver %q\n", name, driver)
			continue
		}

		if _, err := reg.Create(ctx, d); err != nil {
			fmt.Printf("skip %s: %v\n", name, err)
			continue
		}
		imported++
		fmt.Printf("imported %s (%s)\n", name, d.Driver)
	}

	fmt.Printf("\nDone: %d database(s) imported. Old entries only carried a container name, no port/host — please open the admin UI and fill in host/port for each one.\n", imported)
	return scanner.Err()
}
