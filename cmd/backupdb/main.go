// Command backupdb dumps MySQL/PostgreSQL/MongoDB databases and uploads the
// compressed dumps to a configurable storage provider (Google Drive by
// default), via a Redis-backed job queue.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "backup":
		err = runBackup(args)
	case "consumer":
		err = runConsumer(args)
	case "scheduler":
		err = runScheduler(args)
	case "admin":
		err = runAdmin(args)
	case "agent":
		err = runAgent(args)
	case "upload":
		err = runUpload(args)
	case "migrate":
		err = runMigrate(args)
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage()
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `backupdb — backup MySQL/PostgreSQL/MongoDB to Google Drive / S3 via a Redis queue

Usage:
  backupdb backup [dbname driver params]                    Enqueue jobs now: no args backs up every enabled database, with args pushes one manual job
  backupdb consumer                                         Run the worker loop (dump -> upload -> notify)
  backupdb scheduler                                        Watch the registry's per-database schedules and enqueue jobs when due
  backupdb admin                                             Run the admin web UI (manage databases, schedules, and storage destinations)
  backupdb upload <dbname> <filepath> <filename> [targetID]  Upload a single file directly (targetID overrides the database's configured destination)
  backupdb migrate <path-to-databases.txt>                   Import a legacy databases.txt into the registry

Google Drive accounts and S3 buckets are connected from the admin web UI's "Nơi lưu trữ" page, not the CLI.`)
}
