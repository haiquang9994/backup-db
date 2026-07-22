// Package dump shells out to mysqldump/pg_dump/mongodump over TCP and
// writes a gzip-compressed dump file.
package dump

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Extension returns the dump file extension for driver (before the .gz
// suffix every dump gets), shared by the consumer and the `agent`
// subcommand so both name files the same way.
func Extension(driver string) (string, error) {
	switch driver {
	case "mysql", "postgres":
		return "sql", nil
	case "mongo":
		return "archive", nil
	default:
		return "", fmt.Errorf("unknown driver: %s", driver)
	}
}

// Params describes how to reach a target database over TCP.
type Params struct {
	Host     string
	Port     string
	Username string
	Password string
	AuthDB   string // mongo only
}

// ParseParams parses the pipe-delimited "host|port|user|pass|authDB" job
// param string used on the Redis queue and stored in the registry, rewriting
// localhost/127.0.0.1 for the consumer's own Docker container (see
// resolveHost). Callers that don't run in that same container — namely the
// `agent` subcommand, commonly run as a bare process directly on the target
// database's own host, where "localhost" already means exactly that host —
// should use ParseParamsRaw instead.
func ParseParams(raw string) Params {
	p := ParseParamsRaw(raw)
	p.Host = resolveHost(p.Host)
	return p
}

// ParseParamsRaw is ParseParams without the localhost/127.0.0.1 rewrite.
func ParseParamsRaw(raw string) Params {
	parts := strings.Split(raw, "|")
	get := func(i int) string {
		if i < len(parts) {
			return parts[i]
		}
		return ""
	}
	return Params{
		Host:     get(0),
		Port:     get(1),
		Username: get(2),
		Password: get(3),
		AuthDB:   get(4),
	}
}

// resolveHost rewrites "localhost"/"127.0.0.1" to host.docker.internal: the
// consumer always runs inside its own container, where those addresses only
// ever reach the container itself, never a database installed directly on
// the Docker host — which is what a user entering "localhost" actually
// means. docker-compose.yml maps host.docker.internal via extra_hosts.
func resolveHost(host string) string {
	if host == "localhost" || host == "127.0.0.1" {
		return "host.docker.internal"
	}
	return host
}

// runToGzipFile executes name(args) with the given extra env vars and
// gzip-compresses its stdout into outPath. Use for tools that emit plain
// text (mysqldump, pg_dump).
func runToGzipFile(ctx context.Context, name string, args []string, env []string, outPath string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()

	gz := gzip.NewWriter(out)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}

	if _, err := io.Copy(gz, stdout); err != nil {
		return fmt.Errorf("copy %s output: %w", name, err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("close gzip writer: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%s failed: %w: %s", name, err, stderr.String())
	}
	return nil
}

// runToFile executes name(args) and writes its stdout directly to outPath.
// Use for tools that already produce compressed output (mongodump --gzip).
func runToFile(ctx context.Context, name string, args []string, env []string, outPath string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()
	cmd.Stdout = out

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w: %s", name, err, stderr.String())
	}
	return nil
}
