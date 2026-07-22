// Package storage defines the destination-agnostic upload interface used by
// the backup worker. Each database picks a storage_targets row (in
// internal/registry) as its destination — a Google Drive account or an
// S3-compatible bucket today. Adding a new kind means adding one case to
// New and one package implementing Provider; no changes anywhere else.
package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"golang.org/x/oauth2"

	"backupdb/internal/config"
	"backupdb/internal/registry"
	"backupdb/internal/storage/gdrive"
	"backupdb/internal/storage/s3store"
)

// Provider uploads a local dump file to a remote destination, organized by
// database name and backup date, and fetches it back for the admin UI's
// per-database file list.
type Provider interface {
	// Upload returns remoteRef — a kind-specific opaque identifier (Google
	// Drive file ID, S3 object key) — and the uploaded size in bytes.
	// Callers persist both (internal/registry's backup_files table) so a
	// later Download doesn't need to re-list the destination.
	Upload(ctx context.Context, dbname, date, filename, localPath string) (remoteRef string, sizeBytes int64, err error)

	// Download resolves remoteRef (as returned by Upload) back to the file
	// content. Exactly one of redirectURL/body is set: a kind that can hand
	// out a direct link (S3's presigned URL) sets redirectURL and leaves
	// body nil; a kind that must stream through our own credentials
	// (Google Drive's OAuth token) sets body instead, which the caller must
	// Close.
	Download(ctx context.Context, remoteRef string) (redirectURL string, body io.ReadCloser, contentType string, err error)
}

// New resolves targetID (a storage_targets row) and builds the matching
// Provider. It is called once per job — construction is cheap enough
// (one Drive API round trip, or none for S3) that there is no need to cache
// providers across jobs, and building fresh avoids stale-token edge cases.
func New(ctx context.Context, cfg *config.Config, reg *registry.Registry, targetID int64) (Provider, error) {
	if targetID == 0 {
		return nil, fmt.Errorf("no storage destination selected for this database — assign one in the admin UI")
	}

	target, err := reg.GetStorageTarget(ctx, targetID)
	if err != nil {
		return nil, fmt.Errorf("load storage destination #%d: %w", targetID, err)
	}
	if target == nil {
		return nil, fmt.Errorf("storage destination #%d not found (was it deleted?)", targetID)
	}

	return Build(cfg, reg, *target)
}

// Build constructs the Provider for an already-loaded storage_targets row.
// Callers that already have the row (e.g. to log its Kind/Label) should use
// this instead of New to avoid looking it up twice.
func Build(cfg *config.Config, reg *registry.Registry, target registry.StorageTarget) (Provider, error) {
	switch target.Kind {
	case "gdrive":
		return newGDrive(cfg, reg, target)
	case "s3":
		return newS3(target)
	default:
		return nil, fmt.Errorf("unknown storage destination kind: %q", target.Kind)
	}
}

// gdriveConfig is the JSON shape stored in storage_targets.config for
// kind="gdrive".
type gdriveConfig struct {
	Token string `json:"token"` // marshaled oauth2.Token
	Email string `json:"email"`
}

func newGDrive(cfg *config.Config, reg *registry.Registry, target registry.StorageTarget) (Provider, error) {
	var gc gdriveConfig
	if err := json.Unmarshal([]byte(target.Config), &gc); err != nil {
		return nil, fmt.Errorf("parse google account config: %w", err)
	}

	store := &registryTokenStore{reg: reg, targetID: target.ID, email: gc.Email, tokenJSON: gc.Token}
	return gdrive.New(cfg.GoogleCredentialsFile, store, cfg.ProjectName)
}

// registryTokenStore adapts internal/registry's storage_targets table to
// gdrive.TokenStore, so a refreshed token gets written straight back to
// this account's row.
type registryTokenStore struct {
	reg       *registry.Registry
	targetID  int64
	email     string
	tokenJSON string
}

func (s *registryTokenStore) Load() (*oauth2.Token, error) {
	tok := &oauth2.Token{}
	if err := json.Unmarshal([]byte(s.tokenJSON), tok); err != nil {
		return nil, err
	}
	return tok, nil
}

func (s *registryTokenStore) Save(tok *oauth2.Token) error {
	b, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	cfg, err := json.Marshal(gdriveConfig{Token: string(b), Email: s.email})
	if err != nil {
		return err
	}
	return s.reg.UpdateStorageTargetConfig(context.Background(), s.targetID, string(cfg))
}

func newS3(target registry.StorageTarget) (Provider, error) {
	var sc s3store.Config
	if err := json.Unmarshal([]byte(target.Config), &sc); err != nil {
		return nil, fmt.Errorf("parse s3 config: %w", err)
	}
	return s3store.New(sc)
}
