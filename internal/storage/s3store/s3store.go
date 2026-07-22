// Package s3store implements storage.Provider for any S3-compatible bucket
// (AWS S3, MinIO, Cloudflare R2, DigitalOcean Spaces, ...) via a custom
// endpoint, using the same {prefix}/{dbname}/{date}/{filename} key layout
// as gdrive's folder tree so both destinations are organized the same way.
package s3store

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// downloadURLExpiry is how long a presigned download link (handed out by
// Download below, and ultimately by the admin UI's download button) stays
// valid before it must be re-requested.
const downloadURLExpiry = 15 * time.Minute

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

// Upload implements storage.Provider. The returned remoteRef is the object
// key, which Download below needs to fetch it back later.
func (c *Client) Upload(ctx context.Context, dbname, date, filename, localPath string) (string, int64, error) {
	key := c.key(dbname, date, filename)
	info, err := c.mc.FPutObject(ctx, c.bucket, key, localPath, minio.PutObjectOptions{
		ContentType: "application/gzip",
	})
	if err != nil {
		return "", 0, fmt.Errorf("upload s3://%s/%s: %w", c.bucket, key, err)
	}
	return key, info.Size, nil
}

// Download implements storage.Provider. S3-compatible stores can hand out a
// time-limited signed URL directly, so the caller (the admin UI) can
// redirect the browser straight to the bucket instead of proxying bytes
// through our own server.
func (c *Client) Download(ctx context.Context, remoteRef string) (string, io.ReadCloser, string, error) {
	u, err := c.mc.PresignedGetObject(ctx, c.bucket, remoteRef, downloadURLExpiry, url.Values{})
	if err != nil {
		return "", nil, "", fmt.Errorf("presign s3://%s/%s: %w", c.bucket, remoteRef, err)
	}
	return u.String(), nil, "", nil
}

func (c *Client) key(dbname, date, filename string) string {
	prefix := strings.Trim(c.prefix, "/")
	if prefix == "" {
		return fmt.Sprintf("%s/%s/%s", dbname, date, filename)
	}
	return fmt.Sprintf("%s/%s/%s/%s", prefix, dbname, date, filename)
}
