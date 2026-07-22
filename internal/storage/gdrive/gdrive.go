// Package gdrive implements storage.Provider by uploading dump files into a
// {rootFolder}/{dbname}/{date}/ folder tree on Google Drive. OAuth tokens
// are not stored on disk — the caller supplies a TokenStore (the admin
// registry backs one per connected Google account) so multiple accounts
// can be connected at once, each database picking which one to use.
package gdrive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// defaultRootFolderName is used when the caller doesn't specify one (e.g.
// PROJECT_NAME is unset).
const defaultRootFolderName = "Backups"

var scopes = []string{drive.DriveScope, "https://www.googleapis.com/auth/userinfo.email"}

// TokenStore persists the OAuth token for one connected Google account.
// Save is called whenever the token is refreshed, so the caller can write
// it back wherever it keeps it (the admin registry, in this app's case).
type TokenStore interface {
	Load() (*oauth2.Token, error)
	Save(tok *oauth2.Token) error
}

type Client struct {
	svc    *drive.Service
	rootID string
}

// New builds a Drive client from an OAuth client config (credentialsFile,
// the app's identity — shared across every connected account) and a
// TokenStore holding one specific account's token. It refreshes and
// persists the token automatically as it expires. rootFolder is the
// top-level Drive folder everything is nested under; an empty string falls
// back to defaultRootFolderName.
func New(credentialsFile string, store TokenStore, rootFolder string) (*Client, error) {
	if rootFolder == "" {
		rootFolder = defaultRootFolderName
	}

	cfg, err := loadOAuthConfig(credentialsFile)
	if err != nil {
		return nil, err
	}

	tok, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("load google token: %w", err)
	}

	ctx := context.Background()
	ts := &savingTokenSource{
		wrapped: cfg.TokenSource(ctx, tok),
		store:   store,
		last:    tok.AccessToken,
	}

	svc, err := drive.NewService(ctx, option.WithTokenSource(ts))
	if err != nil {
		return nil, fmt.Errorf("create drive service: %w", err)
	}

	c := &Client{svc: svc}
	rootID, err := c.findOrCreateFolder(ctx, rootFolder, "root")
	if err != nil {
		return nil, fmt.Errorf("init root folder: %w", err)
	}
	c.rootID = rootID
	return c, nil
}

// AuthURL builds the URL the user must open in a browser to authorize
// access. It uses the "installed app" loopback redirect (http://localhost)
// registered in credentialsFile, so the browser will land on an
// unreachable page after authorizing — the verification code is in that
// page's URL (?code=...) and must be copied from the address bar. This
// works the same whether the caller is a local terminal or a remote admin
// web UI, since it never requires Google to actually reach our server.
func AuthURL(credentialsFile string) (string, error) {
	cfg, err := loadOAuthConfig(credentialsFile)
	if err != nil {
		return "", err
	}
	return cfg.AuthCodeURL("state-token",
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "select_account consent"),
	), nil
}

// Exchange trades a verification code (from the AuthURL flow) for a token.
// The caller is responsible for persisting it (see TokenStore).
func Exchange(credentialsFile, code string) (*oauth2.Token, error) {
	cfg, err := loadOAuthConfig(credentialsFile)
	if err != nil {
		return nil, err
	}
	tok, err := cfg.Exchange(context.Background(), code)
	if err != nil {
		return nil, fmt.Errorf("exchange auth code: %w", err)
	}
	return tok, nil
}

// FetchEmail is a best-effort lookup of which Google account a token
// belongs to, for display in the admin UI. Errors are non-fatal — callers
// should proceed without an email if this fails.
func FetchEmail(ctx context.Context, tok *oauth2.Token) (string, error) {
	client := oauth2.NewClient(ctx, oauth2.StaticTokenSource(tok))
	resp, err := client.Get("https://www.googleapis.com/oauth2/v2/userinfo")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("userinfo: %s: %s", resp.Status, string(body))
	}
	var info struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", err
	}
	return info.Email, nil
}

// Upload implements storage.Provider. The returned remoteRef is the Drive
// file ID, which Download below needs to fetch it back later.
func (c *Client) Upload(ctx context.Context, dbname, date, filename, localPath string) (string, int64, error) {
	dbFolderID, err := c.findOrCreateFolder(ctx, dbname, c.rootID)
	if err != nil {
		return "", 0, fmt.Errorf("find/create db folder: %w", err)
	}
	dateFolderID, err := c.findOrCreateFolder(ctx, date, dbFolderID)
	if err != nil {
		return "", 0, fmt.Errorf("find/create date folder: %w", err)
	}

	f, err := os.Open(localPath)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	mimeType := mime.TypeByExtension(filepath.Ext(filename))
	if mimeType == "" {
		mimeType = "application/gzip"
	}

	file := &drive.File{
		Name:                  filename,
		MimeType:              mimeType,
		Parents:               []string{dateFolderID},
		ViewersCanCopyContent: false,
		WritersCanShare:       false,
	}

	// Chunked (resumable) upload keeps memory flat for large dumps.
	created, err := c.svc.Files.Create(file).
		Media(f, googleapi.ChunkSize(1<<20)).
		Fields("id, size").
		Context(ctx).
		Do()
	if err != nil {
		return "", 0, fmt.Errorf("upload %s: %w", filename, err)
	}
	return created.Id, created.Size, nil
}

// Download implements storage.Provider. Drive files aren't made publicly
// linkable (WritersCanShare is false on upload), so the only way to fetch
// one back is to stream it through our own OAuth token — there is no
// redirectURL to hand out, unlike S3's presigned URLs.
func (c *Client) Download(ctx context.Context, remoteRef string) (string, io.ReadCloser, string, error) {
	resp, err := c.svc.Files.Get(remoteRef).Context(ctx).Download()
	if err != nil {
		return "", nil, "", fmt.Errorf("download %s: %w", remoteRef, err)
	}
	return "", resp.Body, resp.Header.Get("Content-Type"), nil
}

func (c *Client) findOrCreateFolder(ctx context.Context, name, parentID string) (string, error) {
	q := fmt.Sprintf(
		"'%s' in parents and name = '%s' and mimeType = 'application/vnd.google-apps.folder' and trashed = false",
		parentID, escapeQueryValue(name),
	)
	res, err := c.svc.Files.List().Q(q).Fields("files(id, name)").PageSize(10).Context(ctx).Do()
	if err != nil {
		return "", err
	}
	if len(res.Files) > 0 {
		return res.Files[0].Id, nil
	}

	folder := &drive.File{
		Name:     name,
		MimeType: "application/vnd.google-apps.folder",
		Parents:  []string{parentID},
	}
	created, err := c.svc.Files.Create(folder).Context(ctx).Do()
	if err != nil {
		return "", err
	}
	return created.Id, nil
}

func escapeQueryValue(s string) string {
	return strings.ReplaceAll(s, "'", "\\'")
}

func loadOAuthConfig(credentialsFile string) (*oauth2.Config, error) {
	b, err := os.ReadFile(credentialsFile)
	if err != nil {
		return nil, fmt.Errorf("read google credentials: %w", err)
	}
	cfg, err := google.ConfigFromJSON(b, scopes...)
	if err != nil {
		return nil, fmt.Errorf("parse google credentials: %w", err)
	}
	return cfg, nil
}

// savingTokenSource wraps an oauth2.TokenSource and persists the token via
// TokenStore whenever the underlying source refreshes it.
type savingTokenSource struct {
	wrapped oauth2.TokenSource
	store   TokenStore
	last    string
}

func (s *savingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := s.wrapped.Token()
	if err != nil {
		return nil, err
	}
	if tok.AccessToken != s.last {
		if err := s.store.Save(tok); err != nil {
			return nil, err
		}
		s.last = tok.AccessToken
	}
	return tok, nil
}
