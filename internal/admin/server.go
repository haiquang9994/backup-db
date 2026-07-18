// Package admin serves a small Basic-Auth-protected web UI for managing the
// list of databases to back up, their backup schedules, and the storage
// destinations (Google Drive accounts, S3 buckets) they upload to.
package admin

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"backupdb/internal/queue"
	"backupdb/internal/registry"
	"backupdb/internal/storage/gdrive"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

var tmpl = template.Must(template.ParseFS(templatesFS, "templates/*.html"))

type Server struct {
	reg                   *registry.Registry
	q                     *queue.Client
	username              string
	password              string
	googleCredentialsFile string
}

func NewServer(reg *registry.Registry, q *queue.Client, username, password, googleCredentialsFile string) *Server {
	return &Server{
		reg:                   reg,
		q:                     q,
		username:              username,
		password:              password,
		googleCredentialsFile: googleCredentialsFile,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	staticSub, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	mux.HandleFunc("GET /{$}", s.handleList)
	mux.HandleFunc("GET /new", s.handleNewForm)
	mux.HandleFunc("POST /new", s.handleCreate)
	mux.HandleFunc("GET /edit/{id}", s.handleEditForm)
	mux.HandleFunc("POST /edit/{id}", s.handleUpdate)
	mux.HandleFunc("POST /delete/{id}", s.handleDelete)
	mux.HandleFunc("POST /toggle/{id}", s.handleToggle)
	mux.HandleFunc("POST /backup-now/{id}", s.handleBackupNow)
	mux.HandleFunc("POST /databases/{id}/schedules", s.handleAddSchedule)
	mux.HandleFunc("POST /schedules/{id}/delete", s.handleDeleteSchedule)
	mux.HandleFunc("GET /storage", s.handleStorageList)
	mux.HandleFunc("POST /storage/google", s.handleStorageAddGoogle)
	mux.HandleFunc("POST /storage/s3", s.handleStorageAddS3)
	mux.HandleFunc("POST /storage/{id}/delete", s.handleStorageDelete)

	return s.basicAuth(mux)
}

func (s *Server) basicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.username == "" || s.password == "" {
			next.ServeHTTP(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.username)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(s.password)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="backupdb admin"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type formData struct {
	Action         string
	Editing        bool
	Database       registry.Database
	Schedules      []registry.Schedule
	StorageTargets []registry.StorageTarget
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	dbs, err := s.reg.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tmpl.ExecuteTemplate(w, "list.html", dbs); err != nil {
		log.Println("render list:", err)
	}
}

func (s *Server) handleNewForm(w http.ResponseWriter, r *http.Request) {
	targets, err := s.reg.ListStorageTargets(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := formData{Action: "/new", Database: registry.Database{Driver: "mysql", Enabled: true}, StorageTargets: targets}
	if err := tmpl.ExecuteTemplate(w, "form.html", data); err != nil {
		log.Println("render form:", err)
	}
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	d, err := parseForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := s.reg.Create(r.Context(), d); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleEditForm(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	d, err := s.reg.Get(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if d == nil {
		http.NotFound(w, r)
		return
	}
	schedules, err := s.reg.ListSchedulesByDatabase(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	targets, err := s.reg.ListStorageTargets(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := formData{Action: "/edit/" + r.PathValue("id"), Database: *d, Editing: true, Schedules: schedules, StorageTargets: targets}
	if err := tmpl.ExecuteTemplate(w, "form.html", data); err != nil {
		log.Println("render form:", err)
	}
}

func (s *Server) handleAddSchedule(w http.ResponseWriter, r *http.Request) {
	dbID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	timeOfDay := r.FormValue("time_of_day")
	if _, err := time.Parse("15:04", timeOfDay); err != nil {
		http.Error(w, "invalid time, expected HH:MM", http.StatusBadRequest)
		return
	}
	if _, err := s.reg.CreateSchedule(r.Context(), dbID, timeOfDay); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/edit/%d", dbID), http.StatusSeeOther)
}

func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	sched, err := s.reg.GetSchedule(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if sched == nil {
		http.NotFound(w, r)
		return
	}
	if err := s.reg.DeleteSchedule(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/edit/%d", sched.DatabaseID), http.StatusSeeOther)
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	d, err := parseForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d.ID = id
	if err := s.reg.Update(r.Context(), d); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := s.reg.Delete(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleToggle(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	d, err := s.reg.Get(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if d == nil {
		http.NotFound(w, r)
		return
	}
	if err := s.reg.SetEnabled(r.Context(), id, !d.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleBackupNow(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	d, err := s.reg.Get(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if d == nil {
		http.NotFound(w, r)
		return
	}
	job := queue.NewBackupJob(d.Name, d.Driver, d.Host, d.Port, d.Username, d.Password, d.AuthDB, d.StorageTargetID)
	if err := s.q.Push(r.Context(), job); err != nil {
		http.Error(w, fmt.Sprintf("enqueue backup: %v", err), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// gdriveConfig mirrors internal/storage's shape for kind="gdrive" — kept in
// sync manually since duplicating it here avoids the admin package
// depending on internal/storage (which would create an import cycle back
// through registry).
type gdriveConfig struct {
	Token string `json:"token"`
	Email string `json:"email"`
}

type storageData struct {
	Targets []registry.StorageTarget
	AuthURL string
	Error   string
}

func (s *Server) handleStorageList(w http.ResponseWriter, r *http.Request) {
	s.renderStorage(w, r, "")
}

func (s *Server) renderStorage(w http.ResponseWriter, r *http.Request, errMsg string) {
	targets, err := s.reg.ListStorageTargets(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	authURL, err := gdrive.AuthURL(s.googleCredentialsFile)
	if err != nil {
		// Non-fatal: S3 targets can still be managed even if
		// credentials.json for Google isn't set up yet.
		authURL = ""
	}
	data := storageData{Targets: targets, AuthURL: authURL, Error: errMsg}
	if err := tmpl.ExecuteTemplate(w, "storage.html", data); err != nil {
		log.Println("render storage:", err)
	}
}

func (s *Server) handleStorageAddGoogle(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	label := strings.TrimSpace(r.FormValue("label"))
	code := strings.TrimSpace(r.FormValue("code"))
	if label == "" || code == "" {
		s.renderStorage(w, r, "Vui lòng nhập tên gợi nhớ và verification code")
		return
	}

	tok, err := gdrive.Exchange(s.googleCredentialsFile, code)
	if err != nil {
		s.renderStorage(w, r, err.Error())
		return
	}
	email, _ := gdrive.FetchEmail(r.Context(), tok) // best-effort

	tokJSON, err := json.Marshal(tok)
	if err != nil {
		s.renderStorage(w, r, err.Error())
		return
	}
	cfgJSON, err := json.Marshal(gdriveConfig{Token: string(tokJSON), Email: email})
	if err != nil {
		s.renderStorage(w, r, err.Error())
		return
	}
	if _, err := s.reg.CreateStorageTarget(r.Context(), "gdrive", label, string(cfgJSON)); err != nil {
		s.renderStorage(w, r, err.Error())
		return
	}
	http.Redirect(w, r, "/storage", http.StatusSeeOther)
}

func (s *Server) handleStorageAddS3(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	label := strings.TrimSpace(r.FormValue("label"))
	if label == "" {
		s.renderStorage(w, r, "Vui lòng nhập tên gợi nhớ cho cấu hình S3")
		return
	}

	cfg := map[string]any{
		"endpoint":   strings.TrimSpace(r.FormValue("endpoint")),
		"region":     strings.TrimSpace(r.FormValue("region")),
		"bucket":     strings.TrimSpace(r.FormValue("bucket")),
		"access_key": strings.TrimSpace(r.FormValue("access_key")),
		"secret_key": strings.TrimSpace(r.FormValue("secret_key")),
		"use_ssl":    r.FormValue("use_ssl") == "on",
		"prefix":     strings.TrimSpace(r.FormValue("prefix")),
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		s.renderStorage(w, r, err.Error())
		return
	}
	if _, err := s.reg.CreateStorageTarget(r.Context(), "s3", label, string(cfgJSON)); err != nil {
		s.renderStorage(w, r, err.Error())
		return
	}
	http.Redirect(w, r, "/storage", http.StatusSeeOther)
}

func (s *Server) handleStorageDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := s.reg.DeleteStorageTarget(context.Background(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/storage", http.StatusSeeOther)
}

func parseForm(r *http.Request) (registry.Database, error) {
	if err := r.ParseForm(); err != nil {
		return registry.Database{}, err
	}
	storageTargetID, _ := strconv.ParseInt(r.FormValue("storage_target_id"), 10, 64)
	return registry.Database{
		Name:            r.FormValue("name"),
		Driver:          r.FormValue("driver"),
		Host:            r.FormValue("host"),
		Port:            r.FormValue("port"),
		Username:        r.FormValue("username"),
		Password:        r.FormValue("password"),
		AuthDB:          r.FormValue("auth_db"),
		StorageTargetID: storageTargetID,
		Enabled:         r.FormValue("enabled") == "on",
	}, nil
}
