// Package s3store implements storage.Provider for any S3-compatible bucket
// (AWS S3, MinIO, Cloudflare R2, DigitalOcean Spaces, ...) via a custom
// endpoint, using the same {prefix}/{dbname}/{date}/{filename} key layout
// as gdrive's folder tree so both destinations are organized the same way.
package s3store

import (
	"context"
	"fmt"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Config is the JSON shape stored in storage_targets.config for kind="s3".
type Config struct {
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	UseSSL    bool   `json:"use_ssl"`
	Prefix    string `json:"prefix"`
}

type Client struct {
	mc     *minio.Client
	bucket string
	prefix string
}

func New(cfg Config) (*Client, error) {
	mc, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("create s3 client: %w", err)
	}
	return &Client{mc: mc, bucket: cfg.Bucket, prefix: cfg.Prefix}, nil
}

// Upload implements storage.Provider.
func (c *Client) Upload(ctx context.Context, dbname, date, filename, localPath string) error {
	key := c.key(dbname, date, filename)
	if _, err := c.mc.FPutObject(ctx, c.bucket, key, localPath, minio.PutObjectOptions{
		ContentType: "application/gzip",
	}); err != nil {
		return fmt.Errorf("upload s3://%s/%s: %w", c.bucket, key, err)
	}
	return nil
}

func (c *Client) key(dbname, date, filename string) string {
	prefix := strings.Trim(c.prefix, "/")
	if prefix == "" {
		return fmt.Sprintf("%s/%s/%s", dbname, date, filename)
	}
	return fmt.Sprintf("%s/%s/%s/%s", prefix, dbname, date, filename)
}
