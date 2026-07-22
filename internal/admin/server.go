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
	schedulerTimezone     string
}

func NewServer(reg *registry.Registry, q *queue.Client, username, password, googleCredentialsFile, schedulerTimezone string) *Server {
	return &Server{
		reg:                   reg,
		q:                     q,
		username:              username,
		password:              password,
		googleCredentialsFile: googleCredentialsFile,
		schedulerTimezone:     schedulerTimezone,
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
	mux.HandleFunc("GET /shared-schedules", s.handleSharedScheduleList)
	mux.HandleFunc("GET /shared-schedules/new", s.handleSharedScheduleNewForm)
	mux.HandleFunc("POST /shared-schedules", s.handleSharedScheduleCreate)
	mux.HandleFunc("GET /shared-schedules/{id}/edit", s.handleSharedScheduleEditForm)
	mux.HandleFunc("POST /shared-schedules/{id}", s.handleSharedScheduleUpdate)
	mux.HandleFunc("POST /shared-schedules/{id}/toggle", s.handleSharedScheduleToggle)
	mux.HandleFunc("POST /shared-schedules/{id}/delete", s.handleSharedScheduleDelete)
	mux.HandleFunc("POST /shared-schedules/{id}/times", s.handleAddSharedScheduleTime)
	mux.HandleFunc("POST /shared-schedule-times/{id}/delete", s.handleDeleteSharedScheduleTime)
	mux.HandleFunc("GET /storage", s.handleStorageList)
	mux.HandleFunc("GET /storage/google/new", s.handleStorageGoogleNewForm)
	mux.HandleFunc("POST /storage/google", s.handleStorageAddGoogle)
	mux.HandleFunc("GET /storage/google/{id}/edit", s.handleStorageGoogleEditForm)
	mux.HandleFunc("POST /storage/google/{id}", s.handleStorageUpdateGoogle)
	mux.HandleFunc("POST /storage/google/{id}/delete", s.handleStorageDelete)
	mux.HandleFunc("GET /storage/s3/new", s.handleStorageS3NewForm)
	mux.HandleFunc("POST /storage/s3", s.handleStorageAddS3)
	mux.HandleFunc("GET /storage/s3/{id}/edit", s.handleStorageS3EditForm)
	mux.HandleFunc("POST /storage/s3/{id}", s.handleStorageUpdateS3)
	mux.HandleFunc("POST /storage/s3/{id}/delete", s.handleStorageDelete)
	mux.HandleFunc("GET /notify", s.handleNotifyList)
	mux.HandleFunc("GET /notify/telegram/new", s.handleNotifyTelegramNewForm)
	mux.HandleFunc("POST /notify/telegram", s.handleNotifyAddTelegram)
	mux.HandleFunc("GET /notify/telegram/{id}/edit", s.handleNotifyTelegramEditForm)
	mux.HandleFunc("POST /notify/telegram/{id}", s.handleNotifyUpdateTelegram)
	mux.HandleFunc("POST /notify-channels/{id}/delete", s.handleNotifyDelete)
	mux.HandleFunc("GET /logs", s.handleLogs)
	mux.HandleFunc("POST /logs/clear", s.handleLogsClear)

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
	Action           string
	Editing          bool
	Database         registry.Database
	StorageTargets   []registry.StorageTarget
	NotifyChannels   []registry.NotifyChannel // every channel, to render as checkboxes
	SelectedChannels map[int64]bool           // which of NotifyChannels are currently assigned
	Timezone         string
	TimesCard        scheduleTimesCard
}

// scheduleTimesCard is the data schedule_times.html's shared
// "schedule-times-card" block renders — the same UI for a single database's
// own schedules (form.html) and for a shared schedule's group times
// (shared_schedule_form.html), since both are just a list of
// {ID, TimeOfDay, LastRunDate} rows with an add/delete flow.
type scheduleTimesCard struct {
	Title        string
	Hint         string
	Times        any
	EmptyMsg     string
	AddAction    string
	DeletePrefix string
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
	channels, err := s.reg.ListNotifyChannels(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := formData{
		Action: "/new", Database: registry.Database{Driver: "mysql", Enabled: true}, StorageTargets: targets,
		NotifyChannels: channels, SelectedChannels: map[int64]bool{}, Timezone: s.schedulerTimezone,
	}
	if err := tmpl.ExecuteTemplate(w, "form.html", data); err != nil {
		log.Println("render form:", err)
	}
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	d, notifyChannelIDs, err := parseForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id, err := s.reg.Create(r.Context(), d)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.reg.SetDatabaseNotifyChannels(r.Context(), id, notifyChannelIDs); err != nil {
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
	channels, err := s.reg.ListNotifyChannels(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	assigned, err := s.reg.ListNotifyChannelsForDatabase(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	selectedChannels := make(map[int64]bool, len(assigned))
	for _, c := range assigned {
		selectedChannels[c.ID] = true
	}
	data := formData{
		Action: "/edit/" + r.PathValue("id"), Database: *d, Editing: true, StorageTargets: targets, Timezone: s.schedulerTimezone,
		NotifyChannels: channels, SelectedChannels: selectedChannels,
		TimesCard: scheduleTimesCard{
			Title:        fmt.Sprintf("Lịch backup tự động (giờ %s)", s.schedulerTimezone),
			Hint:         "Có thể thêm nhiều giờ trong ngày — mỗi giờ sẽ tự đẩy 1 job backup riêng cho database này.",
			Times:        schedules,
			EmptyMsg:     "Chưa có lịch nào — database sẽ không tự động backup.",
			AddAction:    fmt.Sprintf("/databases/%d/schedules", id),
			DeletePrefix: "/schedules/",
		},
	}
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

func (s *Server) handleSharedScheduleList(w http.ResponseWriter, r *http.Request) {
	schedules, err := s.reg.ListSharedSchedules(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tmpl.ExecuteTemplate(w, "shared_schedules.html", schedules); err != nil {
		log.Println("render shared schedules:", err)
	}
}

// sharedScheduleFormData backs shared_schedule_form.html, used for both the
// "add new" and "edit existing" pages.
type sharedScheduleFormData struct {
	Editing   bool
	Action    string
	Databases []registry.Database // every database, to render as checkboxes
	Selected  map[int64]bool      // which of Databases are currently members
	Timezone  string
	TimesCard scheduleTimesCard
	Error     string
}

func (s *Server) renderSharedScheduleForm(w http.ResponseWriter, data sharedScheduleFormData) {
	if err := tmpl.ExecuteTemplate(w, "shared_schedule_form.html", data); err != nil {
		log.Println("render shared schedule form:", err)
	}
}

// renderSharedScheduleError re-fetches the database list (needed to render
// the checkbox group again) and re-renders the form with an error, keeping
// whatever the user had checked.
func (s *Server) renderSharedScheduleError(w http.ResponseWriter, r *http.Request, editing bool, action string, id int64, times []registry.SharedScheduleTime, databaseIDs []int64, errMsg string) {
	dbs, err := s.reg.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := sharedScheduleFormData{
		Editing: editing, Action: action, Databases: dbs, Selected: selectedSet(databaseIDs), Timezone: s.schedulerTimezone, Error: errMsg,
	}
	if editing {
		data.TimesCard = s.sharedScheduleTimesCard(id, times)
	}
	s.renderSharedScheduleForm(w, data)
}

// sharedScheduleTimesCard builds the shared "schedule-times-card" data for a
// shared schedule's Times, used by both the edit page and its error re-render.
func (s *Server) sharedScheduleTimesCard(id int64, times []registry.SharedScheduleTime) scheduleTimesCard {
	return scheduleTimesCard{
		Title:        fmt.Sprintf("Khung giờ backup (giờ %s)", s.schedulerTimezone),
		Hint:         "Có thể thêm nhiều khung giờ trong ngày — mỗi giờ tự đẩy 1 job backup riêng cho tất cả database trong nhóm này.",
		Times:        times,
		EmptyMsg:     "Chưa có khung giờ nào — lịch chung sẽ không tự động chạy.",
		AddAction:    fmt.Sprintf("/shared-schedules/%d/times", id),
		DeletePrefix: "/shared-schedule-times/",
	}
}

func selectedSet(ids []int64) map[int64]bool {
	m := make(map[int64]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func (s *Server) handleSharedScheduleNewForm(w http.ResponseWriter, r *http.Request) {
	dbs, err := s.reg.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderSharedScheduleForm(w, sharedScheduleFormData{Action: "/shared-schedules", Databases: dbs, Selected: map[int64]bool{}, Timezone: s.schedulerTimezone})
}

func (s *Server) handleSharedScheduleEditForm(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	sched, err := s.reg.GetSharedSchedule(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if sched == nil {
		http.NotFound(w, r)
		return
	}
	dbs, err := s.reg.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	selected := make(map[int64]bool, len(sched.Databases))
	for _, d := range sched.Databases {
		selected[d.ID] = true
	}
	s.renderSharedScheduleForm(w, sharedScheduleFormData{
		Editing:   true,
		Action:    fmt.Sprintf("/shared-schedules/%d", id),
		Databases: dbs,
		Selected:  selected,
		Timezone:  s.schedulerTimezone,
		TimesCard: s.sharedScheduleTimesCard(id, sched.Times),
	})
}

// parseSharedScheduleForm reads the set of checked database checkboxes
// shared by the add and edit forms.
func parseSharedScheduleForm(r *http.Request) (databaseIDs []int64, err error) {
	if err = r.ParseForm(); err != nil {
		return
	}
	for _, v := range r.Form["database_ids"] {
		id, convErr := strconv.ParseInt(v, 10, 64)
		if convErr != nil {
			continue
		}
		databaseIDs = append(databaseIDs, id)
	}
	return
}

func (s *Server) handleSharedScheduleCreate(w http.ResponseWriter, r *http.Request) {
	databaseIDs, err := parseSharedScheduleForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(databaseIDs) == 0 {
		s.renderSharedScheduleError(w, r, false, "/shared-schedules", 0, nil, databaseIDs, "Chọn ít nhất 1 database")
		return
	}
	id, err := s.reg.CreateSharedSchedule(r.Context(), databaseIDs)
	if err != nil {
		s.renderSharedScheduleError(w, r, false, "/shared-schedules", 0, nil, databaseIDs, err.Error())
		return
	}
	// Straight to the edit page — that's where khung giờ backup get added,
	// same flow as saving a new database first, then adding its schedules.
	http.Redirect(w, r, fmt.Sprintf("/shared-schedules/%d/edit", id), http.StatusSeeOther)
}

func (s *Server) handleSharedScheduleUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	existing, err := s.reg.GetSharedSchedule(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if existing == nil {
		http.NotFound(w, r)
		return
	}

	databaseIDs, err := parseSharedScheduleForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	action := fmt.Sprintf("/shared-schedules/%d", id)

	if len(databaseIDs) == 0 {
		s.renderSharedScheduleError(w, r, true, action, id, existing.Times, databaseIDs, "Chọn ít nhất 1 database")
		return
	}
	if err := s.reg.UpdateSharedSchedule(r.Context(), id, databaseIDs); err != nil {
		s.renderSharedScheduleError(w, r, true, action, id, existing.Times, databaseIDs, err.Error())
		return
	}
	http.Redirect(w, r, "/shared-schedules", http.StatusSeeOther)
}

func (s *Server) handleAddSharedScheduleTime(w http.ResponseWriter, r *http.Request) {
	sharedScheduleID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
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
	if _, err := s.reg.CreateSharedScheduleTime(r.Context(), sharedScheduleID, timeOfDay); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/shared-schedules/%d/edit", sharedScheduleID), http.StatusSeeOther)
}

func (s *Server) handleDeleteSharedScheduleTime(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	t, err := s.reg.GetSharedScheduleTime(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if t == nil {
		http.NotFound(w, r)
		return
	}
	if err := s.reg.DeleteSharedScheduleTime(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/shared-schedules/%d/edit", t.SharedScheduleID), http.StatusSeeOther)
}

func (s *Server) handleSharedScheduleToggle(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	sched, err := s.reg.GetSharedSchedule(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if sched == nil {
		http.NotFound(w, r)
		return
	}
	if err := s.reg.SetSharedScheduleEnabled(r.Context(), id, !sched.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/shared-schedules", http.StatusSeeOther)
}

func (s *Server) handleSharedScheduleDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := s.reg.DeleteSharedSchedule(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/shared-schedules", http.StatusSeeOther)
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	d, notifyChannelIDs, err := parseForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d.ID = id
	if err := s.reg.Update(r.Context(), d); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.reg.SetDatabaseNotifyChannels(r.Context(), id, notifyChannelIDs); err != nil {
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

// s3Config mirrors internal/storage/s3store's shape for kind="s3" — same
// reasoning as gdriveConfig above.
type s3Config struct {
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	UseSSL    bool   `json:"use_ssl"`
	Prefix    string `json:"prefix"`
}

func (s *Server) handleStorageList(w http.ResponseWriter, r *http.Request) {
	targets, err := s.reg.ListStorageTargets(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tmpl.ExecuteTemplate(w, "storage.html", struct{ Targets []registry.StorageTarget }{targets}); err != nil {
		log.Println("render storage:", err)
	}
}

// googleFormData backs storage_google.html, used for both the "connect new
// account" and "edit existing account" pages.
type googleFormData struct {
	Editing bool
	Action  string
	Target  registry.StorageTarget
	AuthURL string
	Error   string
}

func (s *Server) renderGoogleForm(w http.ResponseWriter, data googleFormData) {
	if err := tmpl.ExecuteTemplate(w, "storage_google.html", data); err != nil {
		log.Println("render storage google form:", err)
	}
}

func (s *Server) handleStorageGoogleNewForm(w http.ResponseWriter, r *http.Request) {
	authURL, _ := gdrive.AuthURL(s.googleCredentialsFile) // non-fatal: page still explains what's missing
	s.renderGoogleForm(w, googleFormData{Action: "/storage/google", AuthURL: authURL})
}

func (s *Server) handleStorageGoogleEditForm(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	target, err := s.reg.GetStorageTarget(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if target == nil || target.Kind != "gdrive" {
		http.NotFound(w, r)
		return
	}
	authURL, _ := gdrive.AuthURL(s.googleCredentialsFile)
	s.renderGoogleForm(w, googleFormData{
		Editing: true,
		Action:  fmt.Sprintf("/storage/google/%d", id),
		Target:  *target,
		AuthURL: authURL,
	})
}

func (s *Server) handleStorageAddGoogle(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	label := strings.TrimSpace(r.FormValue("label"))
	code := strings.TrimSpace(r.FormValue("code"))
	authURL, _ := gdrive.AuthURL(s.googleCredentialsFile)
	data := googleFormData{Action: "/storage/google", AuthURL: authURL, Target: registry.StorageTarget{Label: label}}

	if label == "" || code == "" {
		data.Error = "Vui lòng nhập tên gợi nhớ và verification code"
		s.renderGoogleForm(w, data)
		return
	}

	tok, err := gdrive.Exchange(s.googleCredentialsFile, code)
	if err != nil {
		data.Error = err.Error()
		s.renderGoogleForm(w, data)
		return
	}
	email, _ := gdrive.FetchEmail(r.Context(), tok) // best-effort

	tokJSON, err := json.Marshal(tok)
	if err != nil {
		data.Error = err.Error()
		s.renderGoogleForm(w, data)
		return
	}
	cfgJSON, err := json.Marshal(gdriveConfig{Token: string(tokJSON), Email: email})
	if err != nil {
		data.Error = err.Error()
		s.renderGoogleForm(w, data)
		return
	}
	if _, err := s.reg.CreateStorageTarget(r.Context(), "gdrive", label, string(cfgJSON)); err != nil {
		data.Error = err.Error()
		s.renderGoogleForm(w, data)
		return
	}
	http.Redirect(w, r, "/storage", http.StatusSeeOther)
}

// handleStorageUpdateGoogle always renames the target; it only re-runs the
// OAuth exchange (replacing the stored token/email) when a verification
// code was actually submitted, so a plain rename doesn't require logging in
// again.
func (s *Server) handleStorageUpdateGoogle(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	target, err := s.reg.GetStorageTarget(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if target == nil || target.Kind != "gdrive" {
		http.NotFound(w, r)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	label := strings.TrimSpace(r.FormValue("label"))
	code := strings.TrimSpace(r.FormValue("code"))
	authURL, _ := gdrive.AuthURL(s.googleCredentialsFile)
	action := fmt.Sprintf("/storage/google/%d", id)
	data := googleFormData{Editing: true, Action: action, Target: *target, AuthURL: authURL}

	if label == "" {
		data.Error = "Vui lòng nhập tên gợi nhớ"
		s.renderGoogleForm(w, data)
		return
	}
	if err := s.reg.UpdateStorageTargetLabel(r.Context(), id, label); err != nil {
		data.Error = err.Error()
		s.renderGoogleForm(w, data)
		return
	}
	data.Target.Label = label

	if code != "" {
		tok, err := gdrive.Exchange(s.googleCredentialsFile, code)
		if err != nil {
			data.Error = err.Error()
			s.renderGoogleForm(w, data)
			return
		}
		email, _ := gdrive.FetchEmail(r.Context(), tok) // best-effort

		tokJSON, err := json.Marshal(tok)
		if err != nil {
			data.Error = err.Error()
			s.renderGoogleForm(w, data)
			return
		}
		cfgJSON, err := json.Marshal(gdriveConfig{Token: string(tokJSON), Email: email})
		if err != nil {
			data.Error = err.Error()
			s.renderGoogleForm(w, data)
			return
		}
		if err := s.reg.UpdateStorageTargetConfig(r.Context(), id, string(cfgJSON)); err != nil {
			data.Error = err.Error()
			s.renderGoogleForm(w, data)
			return
		}
	}

	http.Redirect(w, r, "/storage", http.StatusSeeOther)
}

// s3FormData backs storage_s3.html, used for both the "add new" and "edit
// existing" pages.
type s3FormData struct {
	Editing bool
	Action  string
	Target  registry.StorageTarget
	Config  s3Config
	Error   string
}

func (s *Server) renderS3Form(w http.ResponseWriter, data s3FormData) {
	if err := tmpl.ExecuteTemplate(w, "storage_s3.html", data); err != nil {
		log.Println("render storage s3 form:", err)
	}
}

func (s *Server) handleStorageS3NewForm(w http.ResponseWriter, r *http.Request) {
	s.renderS3Form(w, s3FormData{Action: "/storage/s3"})
}

func (s *Server) handleStorageS3EditForm(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	target, err := s.reg.GetStorageTarget(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if target == nil || target.Kind != "s3" {
		http.NotFound(w, r)
		return
	}
	var cfg s3Config
	if err := json.Unmarshal([]byte(target.Config), &cfg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderS3Form(w, s3FormData{Editing: true, Action: fmt.Sprintf("/storage/s3/%d", id), Target: *target, Config: cfg})
}

// parseS3Form reads the label and s3Config fields shared by the add and
// edit forms.
func parseS3Form(r *http.Request) (label string, cfg s3Config, err error) {
	if err = r.ParseForm(); err != nil {
		return
	}
	label = strings.TrimSpace(r.FormValue("label"))
	cfg = s3Config{
		Endpoint:  strings.TrimSpace(r.FormValue("endpoint")),
		Region:    strings.TrimSpace(r.FormValue("region")),
		Bucket:    strings.TrimSpace(r.FormValue("bucket")),
		AccessKey: strings.TrimSpace(r.FormValue("access_key")),
		SecretKey: strings.TrimSpace(r.FormValue("secret_key")),
		UseSSL:    r.FormValue("use_ssl") == "on",
		Prefix:    strings.TrimSpace(r.FormValue("prefix")),
	}
	return
}

func (s *Server) handleStorageAddS3(w http.ResponseWriter, r *http.Request) {
	label, cfg, err := parseS3Form(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	data := s3FormData{Action: "/storage/s3", Config: cfg, Target: registry.StorageTarget{Label: label}}

	if label == "" {
		data.Error = "Vui lòng nhập tên gợi nhớ cho cấu hình S3"
		s.renderS3Form(w, data)
		return
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		data.Error = err.Error()
		s.renderS3Form(w, data)
		return
	}
	if _, err := s.reg.CreateStorageTarget(r.Context(), "s3", label, string(cfgJSON)); err != nil {
		data.Error = err.Error()
		s.renderS3Form(w, data)
		return
	}
	http.Redirect(w, r, "/storage", http.StatusSeeOther)
}

func (s *Server) handleStorageUpdateS3(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	target, err := s.reg.GetStorageTarget(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if target == nil || target.Kind != "s3" {
		http.NotFound(w, r)
		return
	}

	label, cfg, err := parseS3Form(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	action := fmt.Sprintf("/storage/s3/%d", id)
	data := s3FormData{Editing: true, Action: action, Target: *target, Config: cfg}

	if label == "" {
		data.Error = "Vui lòng nhập tên gợi nhớ"
		s.renderS3Form(w, data)
		return
	}
	data.Target.Label = label
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		data.Error = err.Error()
		s.renderS3Form(w, data)
		return
	}
	if err := s.reg.UpdateStorageTargetLabelConfig(r.Context(), id, label, string(cfgJSON)); err != nil {
		data.Error = err.Error()
		s.renderS3Form(w, data)
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

func (s *Server) handleNotifyList(w http.ResponseWriter, r *http.Request) {
	channels, err := s.reg.ListNotifyChannels(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tmpl.ExecuteTemplate(w, "notify_channels.html", struct {
		Channels []registry.NotifyChannel
	}{channels}); err != nil {
		log.Println("render notify channels:", err)
	}
}

// telegramConfig is the JSON shape stored in notify_channels.config for
// kind="telegram" — mirrors internal/notify's own copy of this shape
// (same convention as s3Config here vs s3store.Config).
type telegramConfig struct {
	BotToken string `json:"bot_token"`
	ChatID   string `json:"chat_id"`
}

// telegramFormData backs notify_telegram.html, used for both the "add new"
// and "edit existing" pages.
type telegramFormData struct {
	Editing bool
	Action  string
	Channel registry.NotifyChannel
	Config  telegramConfig
	Error   string
}

func (s *Server) renderTelegramForm(w http.ResponseWriter, data telegramFormData) {
	if err := tmpl.ExecuteTemplate(w, "notify_telegram.html", data); err != nil {
		log.Println("render notify telegram form:", err)
	}
}

func (s *Server) handleNotifyTelegramNewForm(w http.ResponseWriter, r *http.Request) {
	s.renderTelegramForm(w, telegramFormData{Action: "/notify/telegram"})
}

func (s *Server) handleNotifyTelegramEditForm(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	channel, err := s.reg.GetNotifyChannel(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if channel == nil || channel.Kind != "telegram" {
		http.NotFound(w, r)
		return
	}
	var cfg telegramConfig
	if err := json.Unmarshal([]byte(channel.Config), &cfg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderTelegramForm(w, telegramFormData{
		Editing: true, Action: fmt.Sprintf("/notify/telegram/%d", id), Channel: *channel, Config: cfg,
	})
}

// parseTelegramForm reads the label and bot token/chat id shared by the add
// and edit forms.
func parseTelegramForm(r *http.Request) (label string, cfg telegramConfig, err error) {
	if err = r.ParseForm(); err != nil {
		return
	}
	label = strings.TrimSpace(r.FormValue("label"))
	cfg = telegramConfig{
		BotToken: strings.TrimSpace(r.FormValue("bot_token")),
		ChatID:   strings.TrimSpace(r.FormValue("chat_id")),
	}
	return
}

func (s *Server) handleNotifyAddTelegram(w http.ResponseWriter, r *http.Request) {
	label, cfg, err := parseTelegramForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	data := telegramFormData{Action: "/notify/telegram", Config: cfg, Channel: registry.NotifyChannel{Label: label}}
	if label == "" || cfg.BotToken == "" || cfg.ChatID == "" {
		data.Error = "Vui lòng nhập đủ tên gợi nhớ, bot token và chat id"
		s.renderTelegramForm(w, data)
		return
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		data.Error = err.Error()
		s.renderTelegramForm(w, data)
		return
	}
	if _, err := s.reg.CreateNotifyChannel(r.Context(), "telegram", label, string(cfgJSON)); err != nil {
		data.Error = err.Error()
		s.renderTelegramForm(w, data)
		return
	}
	http.Redirect(w, r, "/notify", http.StatusSeeOther)
}

func (s *Server) handleNotifyUpdateTelegram(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	channel, err := s.reg.GetNotifyChannel(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if channel == nil || channel.Kind != "telegram" {
		http.NotFound(w, r)
		return
	}

	label, cfg, err := parseTelegramForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	action := fmt.Sprintf("/notify/telegram/%d", id)
	data := telegramFormData{Editing: true, Action: action, Channel: *channel, Config: cfg}
	if label == "" || cfg.BotToken == "" || cfg.ChatID == "" {
		data.Error = "Vui lòng nhập đủ tên gợi nhớ, bot token và chat id"
		s.renderTelegramForm(w, data)
		return
	}
	data.Channel.Label = label
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		data.Error = err.Error()
		s.renderTelegramForm(w, data)
		return
	}
	if err := s.reg.UpdateNotifyChannel(r.Context(), id, label, string(cfgJSON)); err != nil {
		data.Error = err.Error()
		s.renderTelegramForm(w, data)
		return
	}
	http.Redirect(w, r, "/notify", http.StatusSeeOther)
}

func (s *Server) handleNotifyDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := s.reg.DeleteNotifyChannel(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/notify", http.StatusSeeOther)
}

// logListLimit caps how far back the "Nhật ký" page looks — it's meant for
// "what happened recently", not a full audit trail with pagination.
const logListLimit = 300

// logRunView adds a human-readable duration ("2.3s" instead of a raw
// millisecond count) to a BackupRun for display.
type logRunView struct {
	registry.BackupRun
	Duration string
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	runs, err := s.reg.ListBackupRuns(r.Context(), logListLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	views := make([]logRunView, len(runs))
	for i, run := range runs {
		d := (time.Duration(run.DurationMS) * time.Millisecond).Round(100 * time.Millisecond)
		views[i] = logRunView{BackupRun: run, Duration: d.String()}
	}
	data := struct {
		Runs  []logRunView
		Limit int
	}{views, logListLimit}
	if err := tmpl.ExecuteTemplate(w, "logs.html", data); err != nil {
		log.Println("render logs:", err)
	}
}

func (s *Server) handleLogsClear(w http.ResponseWriter, r *http.Request) {
	if err := s.reg.DeleteAllBackupRuns(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/logs", http.StatusSeeOther)
}

func parseForm(r *http.Request) (registry.Database, []int64, error) {
	if err := r.ParseForm(); err != nil {
		return registry.Database{}, nil, err
	}
	storageTargetID, _ := strconv.ParseInt(r.FormValue("storage_target_id"), 10, 64)
	var notifyChannelIDs []int64
	for _, v := range r.Form["notify_channel_ids"] {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			continue
		}
		notifyChannelIDs = append(notifyChannelIDs, id)
	}
	d := registry.Database{
		Name:            r.FormValue("name"),
		Driver:          r.FormValue("driver"),
		Host:            r.FormValue("host"),
		Port:            r.FormValue("port"),
		Username:        r.FormValue("username"),
		Password:        r.FormValue("password"),
		AuthDB:          r.FormValue("auth_db"),
		StorageTargetID: storageTargetID,
		Enabled:         r.FormValue("enabled") == "on",
	}
	return d, notifyChannelIDs, nil
}
