// Package queue implements a small Redis list-backed job queue.
package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const QueueName = "backup_db_queue"

type Job struct {
	Cmd             string `json:"cmd"`
	DBName          string `json:"dbname"`
	Driver          string `json:"driver"`
	Params          string `json:"params"`
	StorageTargetID int64  `json:"storage_target_id"`
	// AgentID is 0 for the common case (dump+upload locally, in this
	// consumer). A non-zero value means the database is only reachable
	// from a different server: the consumer dispatches the job to that
	// remote_agents row over HTTPS instead of running it itself, and only
	// polls for the result. See internal/agentproto.
	AgentID int64 `json:"agent_id"`
}

// NewBackupJob builds the Job for one database's backup, packing its
// connection details into the pipe-delimited Params string that
// dump.ParseParams expects on the consumer side.
func NewBackupJob(dbname, driver, host, port, username, password, authDB string, storageTargetID, agentID int64) Job {
	params := fmt.Sprintf("%s|%s|%s|%s|%s", host, port, username, password, authDB)
	return Job{Cmd: "backup", DBName: dbname, Driver: driver, Params: params, StorageTargetID: storageTargetID, AgentID: agentID}
}

type Client struct {
	rdb *redis.Client
}

func New(host, port string) *Client {
	return &Client{
		rdb: redis.NewClient(&redis.Options{
			Addr: fmt.Sprintf("%s:%s", host, port),
		}),
	}
}

func (c *Client) Close() error {
	return c.rdb.Close()
}

func (c *Client) Push(ctx context.Context, job Job) error {
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return c.rdb.RPush(ctx, QueueName, data).Err()
}

// Pop blocks up to timeout waiting for the next job. It returns (nil, nil)
// on timeout so callers can loop and re-check for shutdown.
func (c *Client) Pop(ctx context.Context, timeout time.Duration) (*Job, error) {
	res, err := c.rdb.BLPop(ctx, timeout, QueueName).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// res[0] is the key name, res[1] is the value.
	var job Job
	if err := json.Unmarshal([]byte(res[1]), &job); err != nil {
		return nil, err
	}
	return &job, nil
}
