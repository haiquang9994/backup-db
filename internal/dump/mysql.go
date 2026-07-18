package dump

import "context"

// MySQL runs mysqldump over TCP and writes a gzip-compressed .sql.gz to outPath.
// The password is passed via the MYSQL_PWD env var so it never shows up in
// the process list.
func MySQL(ctx context.Context, dbname string, p Params, outPath string) error {
	port := p.Port
	if port == "" {
		port = "3306"
	}
	user := p.Username
	if user == "" {
		user = "root"
	}

	args := []string{
		"-h", p.Host,
		"-P", port,
		"-u", user,
		"--skip-tz-utc",
		"--default-character-set=utf8mb4",
		"--opt",
		"--no-autocommit",
		"--single-transaction",
		"--routines",
		"--no-tablespaces",
		"--force",
		dbname,
	}

	var env []string
	if p.Password != "" {
		env = append(env, "MYSQL_PWD="+p.Password)
	}

	return runToGzipFile(ctx, "mysqldump", args, env, outPath)
}
