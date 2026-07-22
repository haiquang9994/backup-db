package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/oauth2"

	"backupdb/internal/agentproto"
	"backupdb/internal/config"
	"backupdb/internal/dump"
	"backupdb/internal/storage"
	"backupdb/internal/storage/gdrive"
	"backupdb/internal/storage/s3store"
)

// runAgent runs a standalone HTTPS server for one database server this
// deployment's central admin/scheduler can't reach any other way (no
// inbound port allowed on the central side). It never touches the central
// SQLite registry or Redis queue — every request carries everything it
// needs (internal/agentproto.RunRequest), and every connection is
// TLS-pinned + Bearer-token authenticated since there's no shared network
// or public CA between the two sides. See CLAUDE.md for the full design.
func runAgent(args []string) error {
	cfg := config.Load()
	if cfg.AgentToken == "" {
		return fmt.Errorf("AGENT_TOKEN is required to run the agent — set a long random shared secret, the same value registered in the central admin UI")
	}
	if err := os.MkdirAll(cfg.TmpDir, 0o755); err != nil {
		return fmt.Errorf("prepare tmp dir: %w", err)
	}

	cert, fingerprint, err := loadOrCreateAgentCert(cfg.AgentCertFile, cfg.AgentKeyFile)
	if err != nil {
		return fmt.Errorf("prepare TLS certificate: %w", err)
	}
	log.Println("Agent certificate fingerprint (register this in the central admin UI):", fingerprint)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	store := newJobStore(ctx, cfg)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /run", store.handleRun)
	mux.HandleFunc("GET /run/{id}", store.handleStatus)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	server := &http.Server{
		Addr:      ":" + cfg.AgentPort,
		Handler:   agentproto.RequireToken(cfg.AgentToken, mux),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
	}

	go func() {
		<-ctx.Done()
		log.Println("Shutting down agent...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Println("Agent listening on", server.Addr, "(HTTPS)")
	if err := server.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// loadOrCreateAgentCert loads a persisted self-signed certificate, or
// generates and persists a new one on first run. Persisting it (instead of
// regenerating on every restart) matters because the central admin UI pins
// this certificate's fingerprint — a fresh one every restart would break
// every registered database until re-pinned.
func loadOrCreateAgentCert(certFile, keyFile string) (tls.Certificate, string, error) {
	if cert, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil {
		return cert, agentproto.Fingerprint(cert.Certificate[0]), nil
	}

	if dir := filepath.Dir(certFile); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return tls.Certificate{}, "", err
		}
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, "", err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "backupdb-agent"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(20, 0, 0), // pinned by fingerprint, not a CA chain — no rotation pressure
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, "", err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	if err := os.WriteFile(certFile, certPEM, 0o644); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, "", err
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return cert, agentproto.Fingerprint(cert.Certificate[0]), nil
}

// jobStore tracks in-flight and recently finished jobs in memory only — the
// agent has no database. A job's result simply disappears if the agent
// restarts before the central server polls it, same as a crash losing an
// in-flight job on the local consumer today; entries are swept 24h after
// completion so the map doesn't grow unbounded.
// One agent commonly serves several databases on the same server, so jobs
// run one at a time (queued, not concurrent) to avoid several
// mysqldump/pg_dump/mongodump processes competing for that server's own
// CPU/network — the same reasoning the central consumer already applies to
// its own local jobs. queue is a plain buffered channel: a single worker
// goroutine drains it in order, so accepting a job (POST /run) and actually
// running it are decoupled — the caller gets its 202 immediately either way.
type jobStore struct {
	mu    sync.Mutex
	jobs  map[string]*jobEntry
	cfg   *config.Config
	queue chan queuedJob
}

type queuedJob struct {
	id  string
	req agentproto.RunRequest
}

type jobEntry struct {
	done        bool
	completedAt time.Time
	status      agentproto.RunStatus
}

// queueCapacity is generous on purpose: a queued-but-not-yet-running job is
// just a small struct sitting in a channel, and this is a backup tool, not
// a high-throughput system — no real deployment should ever get close to
// this many databases waiting on one agent at once.
const queueCapacity = 256

func newJobStore(ctx context.Context, cfg *config.Config) *jobStore {
	s := &jobStore{jobs: map[string]*jobEntry{}, cfg: cfg, queue: make(chan queuedJob, queueCapacity)}
	go s.worker(ctx)
	return s
}

// worker is the sole goroutine that ever calls run — this is what makes
// jobs execute one at a time. It exits once ctx is canceled; run() itself
// also unwinds quickly on cancellation since ctx is threaded through to the
// dump subprocess (exec.CommandContext kills it).
func (s *jobStore) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-s.queue:
			s.run(ctx, job.id, job.req)
		}
	}
}

func (s *jobStore) handleRun(w http.ResponseWriter, r *http.Request) {
	var req agentproto.RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	jobID := newJobID()
	s.mu.Lock()
	s.jobs[jobID] = &jobEntry{}
	s.sweepLocked()
	s.mu.Unlock()

	select {
	case s.queue <- queuedJob{id: jobID, req: req}:
	default:
		s.mu.Lock()
		delete(s.jobs, jobID)
		s.mu.Unlock()
		http.Error(w, "agent is overloaded (too many queued jobs), try again later", http.StatusServiceUnavailable)
		return
	}

	log.Printf("[%s] queued job %s (%s)", jobID, req.DBName, req.Driver)
	writeJSON(w, http.StatusAccepted, agentproto.RunAccepted{JobID: jobID})
}

func (s *jobStore) handleStatus(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")

	s.mu.Lock()
	entry, ok := s.jobs[jobID]
	var status agentproto.RunStatus
	if ok {
		status = entry.status
		if !entry.done {
			status.Status = "pending"
		}
	}
	s.mu.Unlock()

	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *jobStore) run(ctx context.Context, jobID string, req agentproto.RunRequest) {
	start := time.Now()
	result, err := runBackupJob(ctx, s.cfg, req)

	status := agentproto.RunStatus{Status: "done", DurationMS: time.Since(start).Milliseconds()}
	if err != nil {
		status.Success = false
		status.Message = err.Error()
		log.Printf("[%s] %s: FAILED: %v", jobID, req.DBName, err)
	} else {
		status.Success = true
		status.Filename = result.Filename
		status.RemoteRef = result.RemoteRef
		status.SizeBytes = result.SizeBytes
		log.Printf("[%s] %s: done, %s", jobID, req.DBName, result.Filename)
	}

	s.mu.Lock()
	if entry, ok := s.jobs[jobID]; ok {
		entry.done = true
		entry.completedAt = time.Now()
		entry.status = status
	}
	s.mu.Unlock()
}

// sweepLocked removes finished jobs older than 24h. Callers must hold s.mu.
func (s *jobStore) sweepLocked() {
	cutoff := time.Now().Add(-24 * time.Hour)
	for id, entry := range s.jobs {
		if entry.done && entry.completedAt.Before(cutoff) {
			delete(s.jobs, id)
		}
	}
}

func newJobID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type backupResult struct {
	Filename  string
	RemoteRef string
	SizeBytes int64
}

// runBackupJob mirrors consumer.go's backupAndUpload, minus everything that
// needs the central registry: no storage_target_id lookup (the caller
// resolves and sends the full config), no backup_runs/backup_files
// recording (the central server does that once it polls this result back).
func runBackupJob(ctx context.Context, cfg *config.Config, req agentproto.RunRequest) (*backupResult, error) {
	params := dump.ParseParams(req.Params)

	ext, err := dump.Extension(req.Driver)
	if err != nil {
		return nil, err
	}

	// The agent runs on a different machine, possibly in a different OS
	// timezone — filenames must use the central deployment's configured
	// zone (sent along on every request), not this host's local clock.
	loc, err := time.LoadLocation(req.Timezone)
	if err != nil {
		loc = time.UTC
	}
	date := time.Now().In(loc).Format("060102")
	stamp := time.Now().In(loc).Format("15h04")
	filename := fmt.Sprintf("%s_%s_%s.%s.gz", req.DBName, date, stamp, ext)
	outPath := filepath.Join(cfg.TmpDir, filename)

	switch req.Driver {
	case "mysql":
		err = dump.MySQL(ctx, req.DBName, params, outPath)
	case "postgres":
		err = dump.Postgres(ctx, req.DBName, params, outPath)
	case "mongo":
		err = dump.Mongo(ctx, req.DBName, params, outPath)
	}
	if err != nil {
		return nil, fmt.Errorf("dump failed: %w", err)
	}

	provider, err := buildAgentProvider(cfg, req.Storage)
	if err != nil {
		return nil, fmt.Errorf("init storage destination: %w", err)
	}

	remoteRef, sizeBytes, err := provider.Upload(ctx, req.DBName, date, filename, outPath)
	if err != nil {
		return nil, fmt.Errorf("upload failed: %w", err)
	}

	_ = os.Remove(outPath)
	return &backupResult{Filename: filename, RemoteRef: remoteRef, SizeBytes: sizeBytes}, nil
}

// buildAgentProvider mirrors internal/storage.Build, but the agent has no
// *registry.Registry to read a storage_targets row from — the central
// server resolves it and sends the Kind+Config directly in the request.
func buildAgentProvider(cfg *config.Config, sc agentproto.StorageConfig) (storage.Provider, error) {
	switch sc.Kind {
	case "gdrive":
		var gc struct {
			Token string `json:"token"`
			Email string `json:"email"`
		}
		if err := json.Unmarshal([]byte(sc.Config), &gc); err != nil {
			return nil, fmt.Errorf("parse google account config: %w", err)
		}
		return gdrive.New(cfg.GoogleCredentialsFile, &discardTokenStore{tokenJSON: gc.Token}, cfg.ProjectName)
	case "s3":
		var s3c s3store.Config
		if err := json.Unmarshal([]byte(sc.Config), &s3c); err != nil {
			return nil, fmt.Errorf("parse s3 config: %w", err)
		}
		return s3store.New(s3c)
	default:
		return nil, fmt.Errorf("unknown storage destination kind: %q", sc.Kind)
	}
}

// discardTokenStore satisfies gdrive.TokenStore for the agent. There is
// nowhere durable to persist a refreshed OAuth access token here, so Save
// is a no-op — the next job just refreshes again from the refresh_token the
// central server sent along, a negligible extra API call rather than a
// correctness problem.
type discardTokenStore struct {
	tokenJSON string
}

func (s *discardTokenStore) Load() (*oauth2.Token, error) {
	tok := &oauth2.Token{}
	if err := json.Unmarshal([]byte(s.tokenJSON), tok); err != nil {
		return nil, err
	}
	return tok, nil
}

func (s *discardTokenStore) Save(*oauth2.Token) error {
	return nil
}
