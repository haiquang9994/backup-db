package dump

import "context"

// Postgres runs pg_dump over TCP and writes a gzip-compressed .sql.gz to outPath.
// The password is passed via the PGPASSWORD env var so it never shows up in
// the process list.
func Postgres(ctx context.Context, dbname string, p Params, outPath string) error {
	port := p.Port
	if port == "" {
		port = "5432"
	}
	user := p.Username
	if user == "" {
		user = "postgres"
	}

	args := []string{
		"-h", p.Host,
		"-p", port,
		"-U", user,
		dbname,
	}

	var env []string
	if p.Password != "" {
		env = append(env, "PGPASSWORD="+p.Password)
	}

	return runToGzipFile(ctx, "pg_dump", args, env, outPath)
}
