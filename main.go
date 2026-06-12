package main

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/yuin/goldmark"
)

//go:embed templates/* static/* docs/*
var embeddedFiles embed.FS

const (
	StatusBacklog    = "backlog"
	StatusInProgress = "in_progress"
	StatusDone       = "done"
	StatusClosed     = "closed"
)

var validStatuses = map[string]bool{
	StatusBacklog:    true,
	StatusInProgress: true,
	StatusDone:       true,
	StatusClosed:     true,
}

var botWritableStatuses = map[string]bool{
	StatusBacklog:    true,
	StatusInProgress: true,
	StatusDone:       true,
}

type App struct {
	db        *sql.DB
	templates *template.Template
}

type Project struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Prefix          string    `json:"prefix"`
	NextStoryNumber int       `json:"nextStoryNumber"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

type Epic struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"projectId"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type Story struct {
	ID            string     `json:"id"`
	ProjectID     string     `json:"projectId"`
	ProjectName   string     `json:"projectName,omitempty"`
	ProjectPrefix string     `json:"projectPrefix,omitempty"`
	EpicID        *string    `json:"epicId"`
	EpicName      *string    `json:"epicName,omitempty"`
	Title         string     `json:"title"`
	Description   string     `json:"description"`
	Status        string     `json:"status"`
	CloseComment  string     `json:"closeComment,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	ClosedAt      *time.Time `json:"closedAt"`
}

type StoryEvent struct {
	ID        int64     `json:"id"`
	StoryID   string    `json:"storyId"`
	Type      string    `json:"type"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"createdAt"`
}

type BoardData struct {
	Projects        []Project
	Epics           []Epic
	StoriesByCol    map[string][]Story
	SelectedProject string
	SelectedEpic    string
	ShowClosed      bool
	StatusColumns   []string
}

func main() {
	var (
		addr   = flag.String("addr", defaultEnv("TASKMANAGER_ADDR", ":8080"), "HTTP listen address")
		dbPath = flag.String("db", defaultEnv("TASKMANAGER_DB", "taskmanager.db"), "SQLite database path")
	)
	flag.Parse()

	if err := os.MkdirAll(filepath.Dir(dbFilePath(*dbPath)), 0755); err != nil {
		log.Fatal(err)
	}

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	app, err := NewApp(db)
	if err != nil {
		log.Fatal(err)
	}
	if err := app.migrate(context.Background()); err != nil {
		log.Fatal(err)
	}

	log.Printf("task manager listening on http://localhost%s", strings.TrimPrefix(*addr, "0.0.0.0"))
	log.Fatal(http.ListenAndServe(*addr, app.routes()))
}

func NewApp(db *sql.DB) (*App, error) {
	funcs := template.FuncMap{
		"statusTitle": statusTitle,
		"markdown": func(s string) template.HTML {
			var buf bytes.Buffer
			if err := goldmark.Convert([]byte(s), &buf); err != nil {
				return template.HTML(template.HTMLEscapeString(s))
			}
			return template.HTML(buf.String())
		},
		"coalesce": func(v *string, fallback string) string {
			if v == nil || *v == "" {
				return fallback
			}
			return *v
		},
	}
	tpl, err := template.New("").Funcs(funcs).ParseFS(embeddedFiles, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &App{db: db, templates: tpl}, nil
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServerFS(embeddedFiles))

	mux.HandleFunc("GET /", a.handleBoard)
	mux.HandleFunc("GET /stories/{id}/panel", a.handleStoryPanel)
	mux.HandleFunc("POST /stories/{id}/status", a.handleUIStatus)
	mux.HandleFunc("POST /stories/{id}/description", a.handleUIDescription)
	mux.HandleFunc("POST /stories/{id}/close", a.handleUIClose)
	mux.HandleFunc("POST /stories/close-done", a.handleUICloseDone)

	mux.HandleFunc("GET /api", a.handleAPIRoot)
	mux.HandleFunc("GET /api/docs", a.handleBotDocs)
	mux.HandleFunc("GET /api/openapi.yaml", a.handleOpenAPI)
	mux.HandleFunc("GET /api/projects", a.handleAPIProjects)
	mux.HandleFunc("POST /api/projects", a.handleAPIProjects)
	mux.HandleFunc("GET /api/epics", a.handleAPIEpics)
	mux.HandleFunc("POST /api/epics", a.handleAPIEpics)
	mux.HandleFunc("GET /api/stories", a.handleAPIStories)
	mux.HandleFunc("POST /api/stories", a.handleAPIStories)
	mux.HandleFunc("GET /api/stories/{id}", a.handleAPIStory)
	mux.HandleFunc("PATCH /api/stories/{id}", a.handleAPIStory)
	mux.HandleFunc("PATCH /api/stories/{id}/status", a.handleAPIStoryStatus)
	mux.HandleFunc("GET /api/stories/{id}/events", a.handleAPIStoryEvents)

	return logging(mux)
}

func (a *App) migrate(ctx context.Context) error {
	stmts := []string{
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE IF NOT EXISTS projects (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			prefix TEXT NOT NULL UNIQUE,
			next_story_number INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS epics (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL REFERENCES projects(id),
			name TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE(project_id, name)
		)`,
		`CREATE TABLE IF NOT EXISTS stories (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL REFERENCES projects(id),
			epic_id TEXT REFERENCES epics(id),
			title TEXT NOT NULL,
			description TEXT NOT NULL,
			status TEXT NOT NULL,
			close_comment TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			closed_at TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS story_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			story_id TEXT NOT NULL REFERENCES stories(id),
			type TEXT NOT NULL,
			message TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := a.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) handleBoard(w http.ResponseWriter, r *http.Request) {
	projectID, epicID, showClosed := boardParams(r)
	data, err := a.boardData(r.Context(), projectID, epicID, showClosed)
	if err != nil {
		httpError(w, err)
		return
	}
	a.render(w, "layout.html", data)
}

func (a *App) handleStoryPanel(w http.ResponseWriter, r *http.Request) {
	story, err := a.getStory(r.Context(), r.PathValue("id"))
	if err != nil {
		httpError(w, err)
		return
	}
	events, err := a.listEvents(r.Context(), story.ID)
	if err != nil {
		httpError(w, err)
		return
	}
	a.render(w, "story_panel.html", struct {
		Story  Story
		Events []StoryEvent
	}{story, events})
}

func (a *App) handleUIStatus(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpError(w, err)
		return
	}
	status := r.FormValue("status")
	if !botWritableStatuses[status] {
		httpError(w, badRequest("UI status changes may use backlog, in_progress, or done; use close for closed"))
		return
	}
	if err := a.changeStoryStatus(r.Context(), r.PathValue("id"), status, false, "Updated from board UI"); err != nil {
		httpError(w, err)
		return
	}
	a.handleBoard(w, r)
}

func (a *App) handleUIDescription(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpError(w, err)
		return
	}
	if err := a.updateStory(r.Context(), r.PathValue("id"), "", r.FormValue("description")); err != nil {
		httpError(w, err)
		return
	}
	a.handleStoryPanel(w, r)
}

func (a *App) handleUIClose(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpError(w, err)
		return
	}
	if err := a.closeStory(r.Context(), r.PathValue("id"), r.FormValue("closeComment")); err != nil {
		httpError(w, err)
		return
	}
	a.handleBoard(w, r)
}

func (a *App) handleUICloseDone(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpError(w, err)
		return
	}
	if err := a.closeDoneStories(r.Context(), storyFilters{
		ProjectID:  r.FormValue("projectId"),
		EpicID:     r.FormValue("epicId"),
		Status:     StatusDone,
		ShowClosed: true,
	}, r.FormValue("closeComment")); err != nil {
		httpError(w, err)
		return
	}
	a.handleBoard(w, r)
}

func (a *App) handleAPIRoot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    "TheTaskManager API",
		"docs":    "/api/docs",
		"openapi": "/api/openapi.yaml",
		"rules": map[string]any{
			"intendedStatusFlow":  []string{StatusBacklog, StatusInProgress, StatusDone},
			"botWritableStatuses": []string{StatusBacklog, StatusInProgress, StatusDone},
			"closed":              "Closed is manual-only. Bots should move finished work to done.",
			"projectRequired":     true,
			"epicRequired":        false,
			"descriptions":        "Markdown",
		},
		"resources": map[string]string{
			"projects": "/api/projects",
			"epics":    "/api/epics",
			"stories":  "/api/stories",
		},
	})
}

func (a *App) handleBotDocs(w http.ResponseWriter, r *http.Request) {
	b, err := embeddedFiles.ReadFile("docs/bot-api.md")
	if err != nil {
		httpError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (a *App) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	b, err := embeddedFiles.ReadFile("docs/openapi.yaml")
	if err != nil {
		httpError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (a *App) handleAPIProjects(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		projects, err := a.listProjects(r.Context())
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, projects)
	case http.MethodPost:
		var req struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Prefix string `json:"prefix"`
		}
		if err := decodeJSON(r, &req); err != nil {
			httpError(w, err)
			return
		}
		project, err := a.createProject(r.Context(), req.ID, req.Name, req.Prefix)
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, project)
	}
}

func (a *App) handleAPIEpics(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		epics, err := a.listEpics(r.Context(), r.URL.Query().Get("projectId"))
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, epics)
	case http.MethodPost:
		var req struct {
			ID          string `json:"id"`
			ProjectID   string `json:"projectId"`
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if err := decodeJSON(r, &req); err != nil {
			httpError(w, err)
			return
		}
		epic, err := a.createEpic(r.Context(), req.ID, req.ProjectID, req.Name, req.Description)
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, epic)
	}
}

func (a *App) handleAPIStories(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		stories, err := a.listStories(r.Context(), storyFilters{
			ProjectID:  r.URL.Query().Get("projectId"),
			EpicID:     r.URL.Query().Get("epicId"),
			Status:     r.URL.Query().Get("status"),
			ShowClosed: r.URL.Query().Get("showClosed") == "1",
		})
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, stories)
	case http.MethodPost:
		var req createStoryRequest
		if err := decodeJSON(r, &req); err != nil {
			httpError(w, err)
			return
		}
		story, err := a.createStory(r.Context(), req)
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, story)
	}
}

func (a *App) handleAPIStory(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		story, err := a.getStory(r.Context(), r.PathValue("id"))
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, story)
	case http.MethodPatch:
		var req struct {
			Title       *string `json:"title"`
			Description *string `json:"description"`
			EpicID      *string `json:"epicId"`
		}
		if err := decodeJSON(r, &req); err != nil {
			httpError(w, err)
			return
		}
		if req.EpicID != nil {
			if err := a.setStoryEpic(r.Context(), r.PathValue("id"), *req.EpicID); err != nil {
				httpError(w, err)
				return
			}
		}
		title, desc := "", ""
		if req.Title != nil {
			title = *req.Title
		}
		if req.Description != nil {
			desc = *req.Description
		}
		if title != "" || desc != "" {
			if err := a.updateStory(r.Context(), r.PathValue("id"), title, desc); err != nil {
				httpError(w, err)
				return
			}
		}
		story, err := a.getStory(r.Context(), r.PathValue("id"))
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, story)
	}
}

func (a *App) handleAPIStoryStatus(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Status string `json:"status"`
	}
	if err := decodeJSON(r, &req); err != nil {
		httpError(w, err)
		return
	}
	if !botWritableStatuses[req.Status] {
		httpError(w, badRequest("bots may only set status to backlog, in_progress, or done; closed is manual-only"))
		return
	}
	if err := a.changeStoryStatus(r.Context(), r.PathValue("id"), req.Status, false, "Updated through bot API"); err != nil {
		httpError(w, err)
		return
	}
	story, err := a.getStory(r.Context(), r.PathValue("id"))
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, story)
}

func (a *App) handleAPIStoryEvents(w http.ResponseWriter, r *http.Request) {
	events, err := a.listEvents(r.Context(), r.PathValue("id"))
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

type createStoryRequest struct {
	Title         string `json:"title"`
	Description   string `json:"description"`
	Status        string `json:"status"`
	ProjectID     string `json:"projectId"`
	ProjectName   string `json:"projectName"`
	ProjectPrefix string `json:"projectPrefix"`
	EpicID        string `json:"epicId"`
	EpicName      string `json:"epicName"`
}

func (a *App) createStory(ctx context.Context, req createStoryRequest) (Story, error) {
	req.Title = strings.TrimSpace(req.Title)
	req.Description = strings.TrimSpace(req.Description)
	if req.Title == "" {
		return Story{}, badRequest("title is required")
	}
	if req.Description == "" {
		return Story{}, badRequest("description is required")
	}
	if req.Status == "" {
		req.Status = StatusBacklog
	}
	if !botWritableStatuses[req.Status] {
		return Story{}, badRequest("status must be backlog, in_progress, or done")
	}

	project, err := a.ensureProject(ctx, req.ProjectID, req.ProjectName, req.ProjectPrefix)
	if err != nil {
		return Story{}, err
	}
	var epicID *string
	if strings.TrimSpace(req.EpicID) != "" || strings.TrimSpace(req.EpicName) != "" {
		epic, err := a.ensureEpic(ctx, strings.TrimSpace(req.EpicID), project.ID, strings.TrimSpace(req.EpicName))
		if err != nil {
			return Story{}, err
		}
		epicID = &epic.ID
	}

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return Story{}, err
	}
	defer tx.Rollback()

	var next int
	if err := tx.QueryRowContext(ctx, `SELECT next_story_number FROM projects WHERE id = ?`, project.ID).Scan(&next); err != nil {
		return Story{}, err
	}
	id := fmt.Sprintf("%s-%03d", project.Prefix, next)
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET next_story_number = ?, updated_at = ? WHERE id = ?`, next+1, formatTime(now), project.ID); err != nil {
		return Story{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO stories (id, project_id, epic_id, title, description, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, project.ID, epicID, req.Title, req.Description, req.Status, formatTime(now), formatTime(now)); err != nil {
		return Story{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO story_events (story_id, type, message, created_at) VALUES (?, ?, ?, ?)`,
		id, "created", "Created story", formatTime(now)); err != nil {
		return Story{}, err
	}
	if err := tx.Commit(); err != nil {
		return Story{}, err
	}
	return a.getStory(ctx, id)
}

func (a *App) createProject(ctx context.Context, id, name, prefix string) (Project, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Project{}, badRequest("name is required")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		id = slugify(name)
	}
	prefix = normalizePrefix(prefix)
	if prefix == "" {
		prefix = prefixFromName(name)
	}
	now := time.Now().UTC()
	_, err := a.db.ExecContext(ctx, `INSERT INTO projects (id, name, prefix, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		id, name, prefix, formatTime(now), formatTime(now))
	if err != nil {
		if strings.Contains(err.Error(), "constraint") {
			return Project{}, badRequest("project id or prefix already exists")
		}
		return Project{}, err
	}
	return a.getProject(ctx, id)
}

func (a *App) ensureProject(ctx context.Context, id, name, prefix string) (Project, error) {
	id = strings.TrimSpace(id)
	if id != "" {
		project, err := a.getProject(ctx, id)
		if err == nil {
			return project, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Project{}, err
		}
		if strings.TrimSpace(name) == "" {
			return Project{}, badRequest("projectId was not found; provide projectName to create it")
		}
		return a.createProject(ctx, id, name, prefix)
	}
	if strings.TrimSpace(name) == "" {
		return Project{}, badRequest("projectId or projectName is required")
	}
	generatedID := slugify(name)
	project, err := a.getProject(ctx, generatedID)
	if err == nil {
		return project, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Project{}, err
	}
	return a.createProject(ctx, generatedID, name, prefix)
}

func (a *App) createEpic(ctx context.Context, id, projectID, name, description string) (Epic, error) {
	name = strings.TrimSpace(name)
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return Epic{}, badRequest("projectId is required")
	}
	if name == "" {
		return Epic{}, badRequest("name is required")
	}
	if _, err := a.getProject(ctx, projectID); err != nil {
		return Epic{}, err
	}
	if strings.TrimSpace(id) == "" {
		id = projectID + "-" + slugify(name)
	}
	now := time.Now().UTC()
	_, err := a.db.ExecContext(ctx, `INSERT INTO epics (id, project_id, name, description, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, projectID, name, description, formatTime(now), formatTime(now))
	if err != nil {
		if strings.Contains(err.Error(), "constraint") {
			return Epic{}, badRequest("epic id or project/name already exists")
		}
		return Epic{}, err
	}
	return a.getEpic(ctx, id)
}

func (a *App) ensureEpic(ctx context.Context, id, projectID, name string) (Epic, error) {
	if id != "" {
		epic, err := a.getEpic(ctx, id)
		if err == nil {
			return epic, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Epic{}, err
		}
		if name == "" {
			return Epic{}, badRequest("epicId was not found; provide epicName to create it")
		}
		return a.createEpic(ctx, id, projectID, name, "")
	}
	if name == "" {
		return Epic{}, badRequest("epicName is required when epicId is omitted")
	}
	epics, err := a.listEpics(ctx, projectID)
	if err != nil {
		return Epic{}, err
	}
	for _, epic := range epics {
		if strings.EqualFold(epic.Name, name) {
			return epic, nil
		}
	}
	return a.createEpic(ctx, "", projectID, name, "")
}

func (a *App) changeStoryStatus(ctx context.Context, id, status string, manualClose bool, message string) error {
	if !validStatuses[status] {
		return badRequest("invalid status")
	}
	current, err := a.getStory(ctx, id)
	if err != nil {
		return err
	}
	if status == StatusClosed && !manualClose {
		return badRequest("closed is manual-only")
	}
	if current.Status == status {
		return nil
	}
	now := time.Now().UTC()
	closedAt := any(nil)
	if status == StatusClosed {
		closedAt = formatTime(now)
	}
	_, err = a.db.ExecContext(ctx, `UPDATE stories SET status = ?, updated_at = ?, closed_at = ? WHERE id = ?`,
		status, formatTime(now), closedAt, id)
	if err != nil {
		return err
	}
	return a.addEvent(ctx, id, "status_changed", fmt.Sprintf("%s: %s -> %s", message, current.Status, status))
}

func (a *App) closeStory(ctx context.Context, id, comment string) error {
	if err := a.changeStoryStatus(ctx, id, StatusClosed, true, "Closed manually"); err != nil {
		return err
	}
	comment = strings.TrimSpace(comment)
	if comment != "" {
		now := time.Now().UTC()
		if _, err := a.db.ExecContext(ctx, `UPDATE stories SET close_comment = ?, updated_at = ? WHERE id = ?`, comment, formatTime(now), id); err != nil {
			return err
		}
		return a.addEvent(ctx, id, "comment", "Close comment: "+comment)
	}
	return nil
}

func (a *App) closeDoneStories(ctx context.Context, filters storyFilters, comment string) error {
	filters.Status = StatusDone
	filters.ShowClosed = true
	stories, err := a.listStories(ctx, filters)
	if err != nil {
		return err
	}
	for _, story := range stories {
		if story.Status == StatusDone {
			if err := a.closeStory(ctx, story.ID, comment); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *App) updateStory(ctx context.Context, id, title, description string) error {
	current, err := a.getStory(ctx, id)
	if err != nil {
		return err
	}
	if strings.TrimSpace(title) == "" {
		title = current.Title
	}
	if strings.TrimSpace(description) == "" {
		description = current.Description
	}
	now := time.Now().UTC()
	_, err = a.db.ExecContext(ctx, `UPDATE stories SET title = ?, description = ?, updated_at = ? WHERE id = ?`,
		strings.TrimSpace(title), strings.TrimSpace(description), formatTime(now), id)
	if err != nil {
		return err
	}
	return a.addEvent(ctx, id, "updated", "Updated story details")
}

func (a *App) setStoryEpic(ctx context.Context, storyID, epicID string) error {
	if strings.TrimSpace(epicID) == "" {
		_, err := a.db.ExecContext(ctx, `UPDATE stories SET epic_id = NULL, updated_at = ? WHERE id = ?`, formatTime(time.Now().UTC()), storyID)
		if err != nil {
			return err
		}
		return a.addEvent(ctx, storyID, "updated", "Removed epic")
	}
	if _, err := a.getEpic(ctx, epicID); err != nil {
		return err
	}
	_, err := a.db.ExecContext(ctx, `UPDATE stories SET epic_id = ?, updated_at = ? WHERE id = ?`, epicID, formatTime(time.Now().UTC()), storyID)
	if err != nil {
		return err
	}
	return a.addEvent(ctx, storyID, "updated", "Updated epic")
}

type storyFilters struct {
	ProjectID  string
	EpicID     string
	Status     string
	ShowClosed bool
}

func (a *App) boardData(ctx context.Context, projectID, epicID string, showClosed bool) (BoardData, error) {
	projects, err := a.listProjects(ctx)
	if err != nil {
		return BoardData{}, err
	}
	epics, err := a.listEpics(ctx, projectID)
	if err != nil {
		return BoardData{}, err
	}
	stories, err := a.listStories(ctx, storyFilters{ProjectID: projectID, EpicID: epicID, ShowClosed: showClosed})
	if err != nil {
		return BoardData{}, err
	}
	columns := []string{StatusBacklog, StatusInProgress, StatusDone}
	if showClosed {
		columns = append(columns, StatusClosed)
	}
	byCol := make(map[string][]Story)
	for _, col := range columns {
		byCol[col] = []Story{}
	}
	for _, story := range stories {
		byCol[story.Status] = append(byCol[story.Status], story)
	}
	return BoardData{
		Projects: projects, Epics: epics, StoriesByCol: byCol,
		SelectedProject: projectID, SelectedEpic: epicID,
		ShowClosed: showClosed, StatusColumns: columns,
	}, nil
}

func (a *App) listProjects(ctx context.Context) ([]Project, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT id, name, prefix, next_story_number, created_at, updated_at FROM projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := []Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

func (a *App) getProject(ctx context.Context, id string) (Project, error) {
	row := a.db.QueryRowContext(ctx, `SELECT id, name, prefix, next_story_number, created_at, updated_at FROM projects WHERE id = ?`, id)
	return scanProject(row)
}

func (a *App) listEpics(ctx context.Context, projectID string) ([]Epic, error) {
	query := `SELECT id, project_id, name, description, created_at, updated_at FROM epics`
	args := []any{}
	if projectID != "" {
		query += ` WHERE project_id = ?`
		args = append(args, projectID)
	}
	query += ` ORDER BY name`
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	epics := []Epic{}
	for rows.Next() {
		e, err := scanEpic(rows)
		if err != nil {
			return nil, err
		}
		epics = append(epics, e)
	}
	return epics, rows.Err()
}

func (a *App) getEpic(ctx context.Context, id string) (Epic, error) {
	row := a.db.QueryRowContext(ctx, `SELECT id, project_id, name, description, created_at, updated_at FROM epics WHERE id = ?`, id)
	return scanEpic(row)
}

func (a *App) listStories(ctx context.Context, f storyFilters) ([]Story, error) {
	query := `SELECT s.id, s.project_id, p.name, p.prefix, s.epic_id, e.name, s.title, s.description, s.status, s.close_comment, s.created_at, s.updated_at, s.closed_at
		FROM stories s
		JOIN projects p ON p.id = s.project_id
		LEFT JOIN epics e ON e.id = s.epic_id`
	var where []string
	var args []any
	if f.ProjectID != "" {
		where = append(where, `s.project_id = ?`)
		args = append(args, f.ProjectID)
	}
	if f.EpicID != "" {
		where = append(where, `s.epic_id = ?`)
		args = append(args, f.EpicID)
	}
	if f.Status != "" {
		if !validStatuses[f.Status] {
			return nil, badRequest("invalid status filter")
		}
		where = append(where, `s.status = ?`)
		args = append(args, f.Status)
	}
	if !f.ShowClosed && f.Status == "" {
		where = append(where, `s.status != ?`)
		args = append(args, StatusClosed)
	}
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += ` ORDER BY s.created_at ASC`
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stories := []Story{}
	for rows.Next() {
		story, err := scanStory(rows)
		if err != nil {
			return nil, err
		}
		stories = append(stories, story)
	}
	return stories, rows.Err()
}

func (a *App) getStory(ctx context.Context, id string) (Story, error) {
	row := a.db.QueryRowContext(ctx, `SELECT s.id, s.project_id, p.name, p.prefix, s.epic_id, e.name, s.title, s.description, s.status, s.close_comment, s.created_at, s.updated_at, s.closed_at
		FROM stories s
		JOIN projects p ON p.id = s.project_id
		LEFT JOIN epics e ON e.id = s.epic_id
		WHERE s.id = ?`, id)
	return scanStory(row)
}

func (a *App) addEvent(ctx context.Context, storyID, typ, message string) error {
	_, err := a.db.ExecContext(ctx, `INSERT INTO story_events (story_id, type, message, created_at) VALUES (?, ?, ?, ?)`,
		storyID, typ, message, formatTime(time.Now().UTC()))
	return err
}

func (a *App) listEvents(ctx context.Context, storyID string) ([]StoryEvent, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT id, story_id, type, message, created_at FROM story_events WHERE story_id = ? ORDER BY created_at DESC, id DESC`, storyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []StoryEvent{}
	for rows.Next() {
		var e StoryEvent
		var created string
		if err := rows.Scan(&e.ID, &e.StoryID, &e.Type, &e.Message, &created); err != nil {
			return nil, err
		}
		e.CreatedAt = parseTime(created)
		events = append(events, e)
	}
	return events, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanProject(row rowScanner) (Project, error) {
	var p Project
	var created, updated string
	if err := row.Scan(&p.ID, &p.Name, &p.Prefix, &p.NextStoryNumber, &created, &updated); err != nil {
		return Project{}, err
	}
	p.CreatedAt = parseTime(created)
	p.UpdatedAt = parseTime(updated)
	return p, nil
}

func scanEpic(row rowScanner) (Epic, error) {
	var e Epic
	var created, updated string
	if err := row.Scan(&e.ID, &e.ProjectID, &e.Name, &e.Description, &created, &updated); err != nil {
		return Epic{}, err
	}
	e.CreatedAt = parseTime(created)
	e.UpdatedAt = parseTime(updated)
	return e, nil
}

func scanStory(row rowScanner) (Story, error) {
	var s Story
	var epicID, epicName, closedAt sql.NullString
	var created, updated string
	if err := row.Scan(&s.ID, &s.ProjectID, &s.ProjectName, &s.ProjectPrefix, &epicID, &epicName, &s.Title, &s.Description, &s.Status, &s.CloseComment, &created, &updated, &closedAt); err != nil {
		return Story{}, err
	}
	if epicID.Valid {
		s.EpicID = &epicID.String
	}
	if epicName.Valid {
		s.EpicName = &epicName.String
	}
	s.CreatedAt = parseTime(created)
	s.UpdatedAt = parseTime(updated)
	if closedAt.Valid {
		t := parseTime(closedAt.String)
		s.ClosedAt = &t
	}
	return s, nil
}

func (a *App) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.templates.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template error: %v", err)
	}
}

func decodeJSON(r *http.Request, dest any) error {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return badRequest("request body is required")
	}
	if err := json.Unmarshal(body, dest); err != nil {
		return badRequest("invalid JSON: " + err.Error())
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type clientError struct {
	status  int
	message string
}

func (e clientError) Error() string { return e.message }

func badRequest(message string) error {
	return clientError{status: http.StatusBadRequest, message: message}
}

func httpError(w http.ResponseWriter, err error) {
	var ce clientError
	switch {
	case errors.As(err, &ce):
		writeJSON(w, ce.status, map[string]string{"error": ce.message})
	case errors.Is(err, sql.ErrNoRows):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	default:
		log.Printf("server error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error"})
	}
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func statusTitle(status string) string {
	switch status {
	case StatusBacklog:
		return "Backlog"
	case StatusInProgress:
		return "In Progress"
	case StatusDone:
		return "Done"
	case StatusClosed:
		return "Closed"
	default:
		return status
	}
}

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func normalizePrefix(s string) string {
	re := regexp.MustCompile(`[^A-Za-z0-9]`)
	return strings.ToUpper(re.ReplaceAllString(strings.TrimSpace(s), ""))
}

func prefixFromName(name string) string {
	parts := strings.FieldsFunc(name, func(r rune) bool {
		return r == ' ' || r == '-' || r == '_' || r == '.'
	})
	var b strings.Builder
	for _, part := range parts {
		if part == "" {
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]))
	}
	prefix := normalizePrefix(b.String())
	if prefix == "" {
		prefix = "TASK"
	}
	if len(prefix) > 5 {
		prefix = prefix[:5]
	}
	return prefix
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func defaultEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func dbFilePath(path string) string {
	if filepath.Dir(path) == "." {
		return "."
	}
	return path
}

func boardParams(r *http.Request) (string, string, bool) {
	_ = r.ParseForm()
	projectID := r.FormValue("projectId")
	epicID := r.FormValue("epicId")
	showClosed := r.FormValue("showClosed") == "1"
	if projectID == "" {
		projectID = r.URL.Query().Get("projectId")
	}
	if epicID == "" {
		epicID = r.URL.Query().Get("epicId")
	}
	if !showClosed {
		showClosed = r.URL.Query().Get("showClosed") == "1"
	}
	return projectID, epicID, showClosed
}
