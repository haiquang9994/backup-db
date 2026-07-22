// Package agentproto is the small HTTP+TLS protocol between the central
// server and a `backupdb agent` running on a database server the central
// server isn't allowed to open any inbound port to reach. The central side
// only ever dials out — it pushes a job with POST /run, then polls
// GET /run/{jobID} until the agent reports it done. Every request carries a
// Bearer token (a shared secret configured on both sides) and every
// connection is TLS-pinned to the agent's self-signed certificate
// fingerprint, since there is no public CA in front of either side.
package agentproto

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// RunRequest is what the central server POSTs to an agent's /run — enough
// for the agent to dump and upload without ever touching the central
// registry itself.
type RunRequest struct {
	DBName  string        `json:"dbname"`
	Driver  string        `json:"driver"`
	Params  string        `json:"params"` // pipe-delimited, see dump.ParseParams
	Storage StorageConfig `json:"storage"`
}

// StorageConfig mirrors registry.StorageTarget's Kind+Config shape, resolved
// by the central server before sending — the agent has no registry of its
// own to look this up from.
type StorageConfig struct {
	Kind   string `json:"kind"`  // "gdrive" | "s3"
	Label  string `json:"label"`
	Config string `json:"config"` // kind-specific JSON, same shape as storage_targets.config
}

// RunAccepted is the immediate reply to POST /run — the job runs in the
// background from here; check back with GET /run/{JobID}.
type RunAccepted struct {
	JobID string `json:"job_id"`
}

// RunStatus is what GET /run/{jobID} returns. Status is "pending" or
// "done"; the rest is only meaningful once Status is "done".
type RunStatus struct {
	Status     string `json:"status"`
	Success    bool   `json:"success,omitempty"`
	Message    string `json:"message,omitempty"` // error text, only set on failure
	Filename   string `json:"filename,omitempty"`
	RemoteRef  string `json:"remote_ref,omitempty"`
	SizeBytes  int64  `json:"size_bytes,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

// Client talks to one agent: fixed endpoint, token, and pinned certificate.
type Client struct {
	endpoint string
	token    string
	http     *http.Client
}

// NewClient builds a Client that pins the agent's TLS certificate by
// fingerprint (hex SHA-256 of the DER-encoded leaf) instead of trusting a
// public CA — agents use a self-signed certificate generated on first run,
// so normal chain verification would always fail.
func NewClient(endpoint, token, certFingerprint string) *Client {
	fp := strings.ToLower(strings.ReplaceAll(certFingerprint, ":", ""))
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, // we verify the fingerprint ourselves below
			VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
				if len(rawCerts) == 0 {
					return fmt.Errorf("agent presented no certificate")
				}
				got := Fingerprint(rawCerts[0])
				if got != fp {
					return fmt.Errorf("agent certificate fingerprint mismatch: got %s, want %s", got, fp)
				}
				return nil
			},
		},
	}
	return &Client{
		endpoint: strings.TrimRight(endpoint, "/"),
		token:    token,
		http:     &http.Client{Transport: transport, Timeout: 30 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("agent returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Run pushes a job to the agent and returns its job ID. The agent starts
// running it in the background and replies immediately — it does not wait
// for the dump+upload to finish, since that can take minutes and holding a
// connection open that long across the public internet is fragile.
func (c *Client) Run(ctx context.Context, req RunRequest) (string, error) {
	var accepted RunAccepted
	if err := c.do(ctx, http.MethodPost, "/run", req, &accepted); err != nil {
		return "", err
	}
	return accepted.JobID, nil
}

// Health checks that the agent is reachable, its TLS certificate still
// matches the pinned fingerprint, and the configured token is accepted —
// backs the admin UI's "Kiểm tra kết nối" button. A nil error means all
// three checked out; any failure (network, fingerprint mismatch, 401, ...)
// comes back as a non-nil error with a message fit to show the user as-is.
func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/health", nil, nil)
}

// Status polls one job's current state.
func (c *Client) Status(ctx context.Context, jobID string) (RunStatus, error) {
	var status RunStatus
	err := c.do(ctx, http.MethodGet, "/run/"+jobID, nil, &status)
	return status, err
}

// Fingerprint computes the pinning value NewClient expects, from a leaf
// certificate's raw DER bytes — used by the agent subcommand to print its
// own fingerprint on startup, so an operator can copy it into the central
// admin UI when registering this agent.
func Fingerprint(derCert []byte) string {
	sum := sha256.Sum256(derCert)
	return hex.EncodeToString(sum[:])
}

// RequireToken wraps an http.Handler, rejecting any request whose
// "Authorization: Bearer <token>" header doesn't match token.
func RequireToken(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
