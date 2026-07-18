package dump

import (
	"context"
	"fmt"
)

// Mongo runs mongodump over TCP with --archive --gzip, which already produces
// a gzip-compressed archive on stdout, so it is written to outPath as-is.
func Mongo(ctx context.Context, dbname string, p Params, outPath string) error {
	port := p.Port
	if port == "" {
		port = "27017"
	}

	args := []string{
		fmt.Sprintf("--host=%s", p.Host),
		fmt.Sprintf("--port=%s", port),
		fmt.Sprintf("--db=%s", dbname),
		"--archive",
		"--gzip",
	}

	if p.Username != "" && p.Password != "" {
		authDB := p.AuthDB
		if authDB == "" {
			authDB = "admin"
		}
		args = append(args,
			"--username="+p.Username,
			"--password="+p.Password,
			"--authenticationDatabase="+authDB,
		)
	}

	return runToFile(ctx, "mongodump", args, nil, outPath)
}
