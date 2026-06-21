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
	"net/url"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"github.com/yuin/goldmark"
)

//go:embed templates/* static/* docs/*
var embeddedFiles embed.FS

const (
	StatusBacklog    = "backlog"
	StatusQueued     = "queued"
	StatusInProgress = "in_progress"
	StatusDone       = "done"
	StatusClosed     = "closed"
)

var validStatuses = map[string]bool{
	StatusBacklog:    true,
	StatusQueued:     true,
	StatusInProgress: true,
	StatusDone:       true,
	StatusClosed:     true,
}

var uiWritableStatuses = map[string]bool{
	StatusBacklog:    true,
	StatusQueued:     true,
	StatusInProgress: true,
	StatusDone:       true,
}

var botWritableStatuses = map[string]bool{
	StatusBacklog:    true,
	StatusInProgress: true,
	StatusDone:       true,
}

type App struct {
	db          *sql.DB
	templates   *template.Template
	agentMu     sync.Mutex
	agentStatus AgentStatus
	agentCancel context.CancelFunc
	streamMu    sync.Mutex
	streams     map[chan string]bool
}

type Project struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Prefix           string    `json:"prefix"`
	WorkingDirectory string    `json:"workingDirectory"`
	NextStoryNumber  int       `json:"nextStoryNumber"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
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
	HasDetailStory  bool
	Detail          StoryPanelData
	Dashboard       DashboardData
	Agent           AgentPanelData
}

type StoryPanelData struct {
	Story  Story
	Events []StoryEvent
}

type DashboardData struct {
	Scope        string
	ProjectCount int
	EpicCount    int
	Counts       StatusCounts
	Projects     []ProjectDashboard
	Project      Project
	Epic         Epic
	Heatmap      []HeatmapDay
	HeatmapTotal int
}

type AgentStatus struct {
	Running        bool
	QueueRunID     int64
	CurrentStoryID string
	Message        string
	LastError      string
	StartedAt      time.Time
	FinishedAt     time.Time
	Completed      int
	Total          int
}

type AgentPanelData struct {
	Status              AgentStatus
	QueuedCount         int
	MissingPathProjects []Project
	Activity            AgentActivityData
}

type FolderPickerData struct {
	Project     Project
	Path        string
	Parent      string
	Home        string
	Directories []FolderEntry
	Error       string
}

type FolderEntry struct {
	Name string
	Path string
}

type AgentActivityData struct {
	LatestRun QueueRunSummary
	StoryRuns []AgentRunSummary
}

type QueueRunSummary struct {
	ID         int64
	Status     string
	ProjectID  string
	EpicID     string
	Total      int
	Completed  int
	Message    string
	Error      string
	StartedAt  time.Time
	FinishedAt *time.Time
}

type AgentRunSummary struct {
	ID               int64
	QueueRunID       int64
	StoryID          string
	StoryTitle       string
	RunKind          string
	Status           string
	WorkingDirectory string
	Branch           string
	PRNumber         int
	PRURL            string
	Stdout           string
	Stderr           string
	FinalMessage     string
	ExitError        string
	StartedAt        time.Time
	FinishedAt       *time.Time
	LogItems         []AgentLogItem
}

type AgentLogItem struct {
	Kind string
	Text string
}

type ProjectDashboard struct {
	Project   Project
	EpicCount int
	Counts    StatusCounts
}

type StatusCounts struct {
	Backlog    int
	Queued     int
	InProgress int
	Done       int
	Closed     int
	Total      int
}

type HeatmapDay struct {
	Date  string
	Count int
	Level int
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
	db.SetMaxOpenConns(1)
	funcs := template.FuncMap{
		"statusTitle":   statusTitle,
		"runKindTitle":  runKindTitle,
		"runKindSource": runKindSource,
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
		"urlquery": url.QueryEscape,
	}
	tpl, err := template.New("").Funcs(funcs).ParseFS(embeddedFiles, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &App{db: db, templates: tpl, streams: make(map[chan string]bool)}, nil
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServerFS(embeddedFiles))

	mux.HandleFunc("GET /", a.handleBoard)
	mux.HandleFunc("GET /board", a.handleBoardPartial)
	mux.HandleFunc("GET /stories/{id}/panel", a.handleStoryPanel)
	mux.HandleFunc("POST /stories/{id}/status", a.handleUIStatus)
	mux.HandleFunc("POST /stories/{id}/description", a.handleUIDescription)
	mux.HandleFunc("POST /stories/{id}/close", a.handleUIClose)
	mux.HandleFunc("POST /stories/close-done", a.handleUICloseDone)
	mux.HandleFunc("POST /stories/queue-backlog", a.handleUIQueueBacklog)
	mux.HandleFunc("POST /projects/{id}/working-directory", a.handleUIProjectWorkingDirectory)
	mux.HandleFunc("GET /folder-picker", a.handleUIFolderPicker)
	mux.HandleFunc("POST /agent/run-queue", a.handleUIRunQueue)
	mux.HandleFunc("POST /agent/stop", a.handleUIStopAgent)
	mux.HandleFunc("GET /agent/status", a.handleUIAgentStatus)
	mux.HandleFunc("GET /agent/activity", a.handleUIAgentActivity)
	mux.HandleFunc("GET /agent/events", a.handleUIAgentEvents)

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
		`PRAGMA busy_timeout = 5000`,
		`CREATE TABLE IF NOT EXISTS projects (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			prefix TEXT NOT NULL UNIQUE,
			working_directory TEXT NOT NULL DEFAULT '',
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
		`CREATE TABLE IF NOT EXISTS queue_runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			status TEXT NOT NULL,
			project_id TEXT NOT NULL DEFAULT '',
			epic_id TEXT NOT NULL DEFAULT '',
			total INTEGER NOT NULL DEFAULT 0,
			completed INTEGER NOT NULL DEFAULT 0,
			message TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT '',
			started_at TEXT NOT NULL,
			finished_at TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS agent_runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			queue_run_id INTEGER NOT NULL REFERENCES queue_runs(id),
			story_id TEXT NOT NULL REFERENCES stories(id),
			project_id TEXT NOT NULL REFERENCES projects(id),
			working_directory TEXT NOT NULL,
			status TEXT NOT NULL,
			run_kind TEXT NOT NULL DEFAULT 'codex_implement',
			branch TEXT NOT NULL DEFAULT '',
			pr_number INTEGER NOT NULL DEFAULT 0,
			pr_url TEXT NOT NULL DEFAULT '',
			prompt TEXT NOT NULL DEFAULT '',
			stdout TEXT NOT NULL DEFAULT '',
			stderr TEXT NOT NULL DEFAULT '',
			final_message TEXT NOT NULL DEFAULT '',
			exit_error TEXT NOT NULL DEFAULT '',
			started_at TEXT NOT NULL,
			finished_at TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS story_pipelines (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			queue_run_id INTEGER NOT NULL REFERENCES queue_runs(id),
			story_id TEXT NOT NULL REFERENCES stories(id),
			phase TEXT NOT NULL DEFAULT '',
			branch TEXT NOT NULL DEFAULT '',
			default_branch TEXT NOT NULL DEFAULT '',
			pr_number INTEGER NOT NULL DEFAULT 0,
			pr_url TEXT NOT NULL DEFAULT '',
			review_json TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL,
			UNIQUE(queue_run_id, story_id)
		)`,
	}
	for _, stmt := range stmts {
		if _, err := a.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	if err := a.ensureColumn(ctx, "projects", "working_directory", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	agentColumns := map[string]string{
		"run_kind":  "TEXT NOT NULL DEFAULT 'codex_implement'",
		"branch":    "TEXT NOT NULL DEFAULT ''",
		"pr_number": "INTEGER NOT NULL DEFAULT 0",
		"pr_url":    "TEXT NOT NULL DEFAULT ''",
	}
	for column, definition := range agentColumns {
		if err := a.ensureColumn(ctx, "agent_runs", column, definition); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) ensureColumn(ctx context.Context, table, column, definition string) error {
	rows, err := a.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = a.db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+column+` `+definition)
	return err
}

func (a *App) handleBoard(w http.ResponseWriter, r *http.Request) {
	projectID, epicID, showClosed := boardParams(r)
	data, err := a.boardData(r.Context(), projectID, epicID, showClosed, r.URL.Query().Get("storyId"))
	if err != nil {
		httpError(w, err)
		return
	}
	a.render(w, "layout.html", data)
}

func (a *App) handleBoardPartial(w http.ResponseWriter, r *http.Request) {
	a.renderBoardPartial(w, r)
}

func (a *App) renderBoardPartial(w http.ResponseWriter, r *http.Request) {
	projectID, epicID, showClosed := boardParams(r)
	data, err := a.boardData(r.Context(), projectID, epicID, showClosed, "")
	if err != nil {
		httpError(w, err)
		return
	}
	a.render(w, "board.html", data)
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
	a.render(w, "story_panel.html", StoryPanelData{story, events})
}

func (a *App) handleUIStatus(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpError(w, err)
		return
	}
	status := r.FormValue("status")
	if !uiWritableStatuses[status] {
		httpError(w, badRequest("UI status changes may use backlog, queued, in_progress, or done; use close for closed"))
		return
	}
	if err := a.changeStoryStatus(r.Context(), r.PathValue("id"), status, false, "Updated from board UI"); err != nil {
		httpError(w, err)
		return
	}
	a.renderBoardPartial(w, r)
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
	a.renderBoardPartial(w, r)
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
	a.renderBoardPartial(w, r)
}

func (a *App) handleUIQueueBacklog(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpError(w, err)
		return
	}
	filters := storyFilters{
		ProjectID:  r.FormValue("projectId"),
		EpicID:     r.FormValue("epicId"),
		ShowClosed: true,
	}
	if r.FormValue("scope") == "epic" {
		if filters.EpicID == "" {
			httpError(w, badRequest("epic filter is required"))
			return
		}
		if err := a.queueBacklogStories(r.Context(), nil, filters); err != nil {
			httpError(w, err)
			return
		}
		a.renderBoardPartial(w, r)
		return
	}
	if len(r.Form["storyId"]) == 0 {
		httpError(w, badRequest("select at least one backlog story"))
		return
	}
	if err := a.queueBacklogStories(r.Context(), r.Form["storyId"], storyFilters{}); err != nil {
		httpError(w, err)
		return
	}
	a.renderBoardPartial(w, r)
}

func (a *App) handleUIProjectWorkingDirectory(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpError(w, err)
		return
	}
	if err := a.updateProjectWorkingDirectory(r.Context(), r.PathValue("id"), r.FormValue("workingDirectory")); err != nil {
		httpError(w, err)
		return
	}
	a.renderBoardPartial(w, r)
}

func (a *App) handleUIFolderPicker(w http.ResponseWriter, r *http.Request) {
	projectID := strings.TrimSpace(r.URL.Query().Get("projectId"))
	if projectID == "" {
		httpError(w, badRequest("projectId is required"))
		return
	}
	project, err := a.getProject(r.Context(), projectID)
	if err != nil {
		httpError(w, err)
		return
	}
	data, err := a.folderPickerData(project, r.URL.Query().Get("path"))
	if err != nil {
		httpError(w, err)
		return
	}
	a.render(w, "folder_picker.html", data)
}

func (a *App) handleUIAgentStatus(w http.ResponseWriter, r *http.Request) {
	panel, err := a.agentPanelDataForRequest(r)
	if err != nil {
		httpError(w, err)
		return
	}
	a.render(w, "agent_run_controls.html", panel)
}

func (a *App) handleUIAgentActivity(w http.ResponseWriter, r *http.Request) {
	projectID, epicID, _ := boardParams(r)
	activity, err := a.agentActivityData(r.Context(), projectID, epicID)
	if err != nil {
		httpError(w, err)
		return
	}
	a.render(w, "agent_activity.html", AgentPanelData{
		Status:   a.currentAgentStatus(),
		Activity: activity,
	})
}

func (a *App) handleUIAgentEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, fmt.Errorf("streaming is not supported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	events := make(chan string, 16)
	a.addAgentStream(events)
	defer a.removeAgentStream(events)

	writeSSE := func(event string) {
		_, _ = fmt.Fprintf(w, "event: %s\ndata: {}\n\n", event)
		flusher.Flush()
	}
	writeSSE("activity")

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case event := <-events:
			writeSSE(event)
		case <-ticker.C:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (a *App) handleUIRunQueue(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpError(w, err)
		return
	}
	filters := storyFilters{
		ProjectID:  r.FormValue("projectId"),
		EpicID:     r.FormValue("epicId"),
		Status:     StatusQueued,
		ShowClosed: true,
	}
	baseURL := requestBaseURL(r)
	if err := a.startAgentQueueRun(r.Context(), filters, baseURL); err != nil {
		httpError(w, err)
		return
	}
	a.renderBoardPartial(w, r)
}

func (a *App) handleUIStopAgent(w http.ResponseWriter, r *http.Request) {
	if err := a.stopAgentQueue(); err != nil {
		httpError(w, err)
		return
	}
	a.renderBoardPartial(w, r)
}

func (a *App) handleAPIRoot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    "TheTaskManager API",
		"docs":    "/api/docs",
		"openapi": "/api/openapi.yaml",
		"rules": map[string]any{
			"intendedStatusFlow":  []string{StatusBacklog, StatusQueued, StatusInProgress, StatusDone},
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
			ID               string `json:"id"`
			Name             string `json:"name"`
			Prefix           string `json:"prefix"`
			WorkingDirectory string `json:"workingDirectory"`
		}
		if err := decodeJSON(r, &req); err != nil {
			httpError(w, err)
			return
		}
		project, err := a.createProject(r.Context(), req.ID, req.Name, req.Prefix, req.WorkingDirectory)
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
	Title            string `json:"title"`
	Description      string `json:"description"`
	Status           string `json:"status"`
	ProjectID        string `json:"projectId"`
	ProjectName      string `json:"projectName"`
	ProjectPrefix    string `json:"projectPrefix"`
	WorkingDirectory string `json:"workingDirectory"`
	ProjectPath      string `json:"projectPath"`
	EpicID           string `json:"epicId"`
	EpicName         string `json:"epicName"`
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

	workingDirectory := req.WorkingDirectory
	if strings.TrimSpace(workingDirectory) == "" {
		workingDirectory = req.ProjectPath
	}
	project, err := a.ensureProject(ctx, req.ProjectID, req.ProjectName, req.ProjectPrefix, workingDirectory)
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

func (a *App) createProject(ctx context.Context, id, name, prefix, workingDirectory string) (Project, error) {
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
	workingDirectory, err := normalizeWorkingDirectory(workingDirectory)
	if err != nil {
		return Project{}, err
	}
	now := time.Now().UTC()
	_, err = a.db.ExecContext(ctx, `INSERT INTO projects (id, name, prefix, working_directory, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, name, prefix, workingDirectory, formatTime(now), formatTime(now))
	if err != nil {
		if strings.Contains(err.Error(), "constraint") {
			return Project{}, badRequest("project id or prefix already exists")
		}
		return Project{}, err
	}
	return a.getProject(ctx, id)
}

func (a *App) ensureProject(ctx context.Context, id, name, prefix, workingDirectory string) (Project, error) {
	id = strings.TrimSpace(id)
	if id != "" {
		project, err := a.getProject(ctx, id)
		if err == nil {
			if strings.TrimSpace(workingDirectory) != "" && project.WorkingDirectory == "" {
				if err := a.updateProjectWorkingDirectory(ctx, project.ID, workingDirectory); err != nil {
					return Project{}, err
				}
				return a.getProject(ctx, project.ID)
			}
			return project, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Project{}, err
		}
		if strings.TrimSpace(name) == "" {
			return Project{}, badRequest("projectId was not found; provide projectName to create it")
		}
		return a.createProject(ctx, id, name, prefix, workingDirectory)
	}
	if strings.TrimSpace(name) == "" {
		return Project{}, badRequest("projectId or projectName is required")
	}
	generatedID := slugify(name)
	project, err := a.getProject(ctx, generatedID)
	if err == nil {
		if strings.TrimSpace(workingDirectory) != "" && project.WorkingDirectory == "" {
			if err := a.updateProjectWorkingDirectory(ctx, project.ID, workingDirectory); err != nil {
				return Project{}, err
			}
			return a.getProject(ctx, project.ID)
		}
		return project, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Project{}, err
	}
	return a.createProject(ctx, generatedID, name, prefix, workingDirectory)
}

func (a *App) updateProjectWorkingDirectory(ctx context.Context, id, workingDirectory string) error {
	if _, err := a.getProject(ctx, id); err != nil {
		return err
	}
	workingDirectory, err := normalizeWorkingDirectory(workingDirectory)
	if err != nil {
		return err
	}
	_, err = a.db.ExecContext(ctx, `UPDATE projects SET working_directory = ?, updated_at = ? WHERE id = ?`,
		workingDirectory, formatTime(time.Now().UTC()), id)
	return err
}

func (a *App) folderPickerData(project Project, requestedPath string) (FolderPickerData, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return FolderPickerData{}, err
	}
	path := strings.TrimSpace(requestedPath)
	if path == "" {
		path = project.WorkingDirectory
	}
	if path == "" {
		path = home
	}
	path, err = expandUserPath(path)
	if err != nil {
		return FolderPickerData{}, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return FolderPickerData{}, err
	}
	data := FolderPickerData{
		Project: project,
		Path:    abs,
		Home:    home,
	}
	if parent := filepath.Dir(abs); parent != abs {
		data.Parent = parent
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		data.Error = "Folder does not exist or is not a directory."
		data.Path = home
		data.Parent = filepath.Dir(home)
		abs = home
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		data.Error = "This folder cannot be opened."
		return data, nil
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		data.Directories = append(data.Directories, FolderEntry{
			Name: name,
			Path: filepath.Join(abs, name),
		})
	}
	sort.Slice(data.Directories, func(i, j int) bool {
		return strings.ToLower(data.Directories[i].Name) < strings.ToLower(data.Directories[j].Name)
	})
	return data, nil
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

func (a *App) createQueueRun(ctx context.Context, filters storyFilters, total int) (int64, error) {
	res, err := a.db.ExecContext(ctx, `INSERT INTO queue_runs (status, project_id, epic_id, total, completed, message, started_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"running", filters.ProjectID, filters.EpicID, total, 0, "Starting queued run", formatTime(time.Now().UTC()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (a *App) updateQueueRun(ctx context.Context, id int64, status, message string, completed int, errValue error) error {
	errorText := ""
	if errValue != nil {
		errorText = truncate(errValue.Error(), 2000)
	}
	finishedAt := any(nil)
	if status != "running" {
		finishedAt = formatTime(time.Now().UTC())
	}
	_, err := a.db.ExecContext(ctx, `UPDATE queue_runs SET status = ?, completed = ?, message = ?, error = ?, finished_at = ? WHERE id = ?`,
		status, completed, message, errorText, finishedAt, id)
	a.publishAgentEvent("activity")
	return err
}

func (a *App) createAgentRun(ctx context.Context, queueRunID int64, project Project, story Story, prompt, runKind, branch string, prNumber int, prURL string) (int64, error) {
	if strings.TrimSpace(runKind) == "" {
		runKind = RunKindCodexImplement
	}
	res, err := a.db.ExecContext(ctx, `INSERT INTO agent_runs (queue_run_id, story_id, project_id, working_directory, status, run_kind, branch, pr_number, pr_url, prompt, started_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		queueRunID, story.ID, project.ID, project.WorkingDirectory, "running", runKind, branch, prNumber, prURL, prompt, formatTime(time.Now().UTC()))
	if err != nil {
		return 0, err
	}
	a.publishAgentEvent("activity")
	return res.LastInsertId()
}

func (a *App) finishAgentStoryRun(ctx context.Context, id int64, status, stdout, stderr, finalMessage string, errValue error) error {
	errorText := ""
	if errValue != nil {
		errorText = truncate(errValue.Error(), 4000)
	}
	_, err := a.db.ExecContext(ctx, `UPDATE agent_runs SET status = ?, stdout = ?, stderr = ?, final_message = ?, exit_error = ?, finished_at = ? WHERE id = ?`,
		status, stdout, stderr, finalMessage, errorText, formatTime(time.Now().UTC()), id)
	a.publishAgentEvent("activity")
	return err
}

func (a *App) updateAgentStoryRunOutput(ctx context.Context, id int64, stdout, stderr string) error {
	_, err := a.db.ExecContext(ctx, `UPDATE agent_runs SET stdout = ?, stderr = ? WHERE id = ?`,
		truncate(stdout, 60000), truncate(stderr, 60000), id)
	a.publishAgentEvent("activity")
	return err
}

type agentRunOutput struct {
	mu        sync.Mutex
	stdout    bytes.Buffer
	stderr    bytes.Buffer
	lastFlush time.Time
	flush     func(stdout, stderr string)
}

func (o *agentRunOutput) stdoutWriter() io.Writer {
	return agentRunOutputWriter{output: o, stream: "stdout"}
}

func (o *agentRunOutput) stderrWriter() io.Writer {
	return agentRunOutputWriter{output: o, stream: "stderr"}
}

func (o *agentRunOutput) snapshot() (string, string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return strings.TrimSpace(o.stdout.String()), strings.TrimSpace(o.stderr.String())
}

func (o *agentRunOutput) append(stream string, p []byte) (int, error) {
	o.mu.Lock()
	if stream == "stderr" {
		_, _ = o.stderr.Write(p)
	} else {
		_, _ = o.stdout.Write(p)
	}
	shouldFlush := time.Since(o.lastFlush) >= 1500*time.Millisecond
	if shouldFlush {
		o.lastFlush = time.Now()
	}
	stdoutText := strings.TrimSpace(o.stdout.String())
	stderrText := strings.TrimSpace(o.stderr.String())
	o.mu.Unlock()

	if shouldFlush && o.flush != nil {
		o.flush(stdoutText, stderrText)
	}
	return len(p), nil
}

func (o *agentRunOutput) flushNow() {
	if o.flush == nil {
		return
	}
	stdoutText, stderrText := o.snapshot()
	o.flush(stdoutText, stderrText)
}

type agentRunOutputWriter struct {
	output *agentRunOutput
	stream string
}

func (w agentRunOutputWriter) Write(p []byte) (int, error) {
	return w.output.append(w.stream, p)
}

func (a *App) agentActivityData(ctx context.Context, projectID, epicID string) (AgentActivityData, error) {
	query := `SELECT id, status, project_id, epic_id, total, completed, message, error, started_at, finished_at FROM queue_runs`
	var where []string
	var args []any
	if projectID != "" {
		where = append(where, `project_id = ?`)
		args = append(args, projectID)
	}
	if epicID != "" {
		where = append(where, `epic_id = ?`)
		args = append(args, epicID)
	}
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += ` ORDER BY id DESC LIMIT 1`

	row := a.db.QueryRowContext(ctx, query, args...)
	run, err := scanQueueRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentActivityData{}, nil
	}
	if err != nil {
		return AgentActivityData{}, err
	}
	storyRuns, err := a.listAgentStoryRuns(ctx, run.ID)
	if err != nil {
		return AgentActivityData{}, err
	}
	return AgentActivityData{LatestRun: run, StoryRuns: storyRuns}, nil
}

func (a *App) listAgentStoryRuns(ctx context.Context, queueRunID int64) ([]AgentRunSummary, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT ar.id, ar.queue_run_id, ar.story_id, s.title, ar.run_kind, ar.status, ar.working_directory, ar.branch, ar.pr_number, ar.pr_url, ar.stdout, ar.stderr, ar.final_message, ar.exit_error, ar.started_at, ar.finished_at
		FROM agent_runs ar
		LEFT JOIN stories s ON s.id = ar.story_id
		WHERE ar.queue_run_id = ?
		ORDER BY ar.id DESC
		LIMIT 24`, queueRunID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []AgentRunSummary{}
	for rows.Next() {
		run, err := scanAgentRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (a *App) agentPanelData(ctx context.Context, queued []Story, projects []Project, projectID, epicID string) (AgentPanelData, error) {
	projectByID := make(map[string]Project, len(projects))
	for _, project := range projects {
		projectByID[project.ID] = project
	}
	missingByID := map[string]Project{}
	for _, story := range queued {
		project, ok := projectByID[story.ProjectID]
		if ok && strings.TrimSpace(project.WorkingDirectory) == "" {
			missingByID[project.ID] = project
		}
	}
	missing := make([]Project, 0, len(missingByID))
	for _, project := range projects {
		if p, ok := missingByID[project.ID]; ok {
			missing = append(missing, p)
		}
	}
	activity, err := a.agentActivityData(ctx, projectID, epicID)
	if err != nil {
		return AgentPanelData{}, err
	}
	return AgentPanelData{
		Status:              a.currentAgentStatus(),
		QueuedCount:         len(queued),
		MissingPathProjects: missing,
		Activity:            activity,
	}, nil
}

func (a *App) agentPanelDataForRequest(r *http.Request) (AgentPanelData, error) {
	projectID, epicID, _ := boardParams(r)
	projects, err := a.listProjects(r.Context())
	if err != nil {
		return AgentPanelData{}, err
	}
	queued, err := a.listStories(r.Context(), storyFilters{
		ProjectID:  projectID,
		EpicID:     epicID,
		Status:     StatusQueued,
		ShowClosed: true,
	})
	if err != nil {
		return AgentPanelData{}, err
	}
	return a.agentPanelData(r.Context(), queued, projects, projectID, epicID)
}

func (a *App) currentAgentStatus() AgentStatus {
	a.agentMu.Lock()
	defer a.agentMu.Unlock()
	return a.agentStatus
}

func (a *App) addAgentStream(events chan string) {
	a.streamMu.Lock()
	defer a.streamMu.Unlock()
	a.streams[events] = true
}

func (a *App) removeAgentStream(events chan string) {
	a.streamMu.Lock()
	defer a.streamMu.Unlock()
	delete(a.streams, events)
	close(events)
}

func (a *App) publishAgentEvent(event string) {
	a.streamMu.Lock()
	defer a.streamMu.Unlock()
	for events := range a.streams {
		select {
		case events <- event:
		default:
		}
	}
}

func (a *App) startAgentQueueRun(ctx context.Context, filters storyFilters, baseURL string) error {
	queued, err := a.listStories(ctx, filters)
	if err != nil {
		return err
	}
	if len(queued) == 0 {
		return badRequest("there are no queued stories to run")
	}
	projects, err := a.listProjects(ctx)
	if err != nil {
		return err
	}
	panel, err := a.agentPanelData(ctx, queued, projects, filters.ProjectID, filters.EpicID)
	if err != nil {
		return err
	}
	if len(panel.MissingPathProjects) > 0 {
		return badRequest("add a project path before running queued stories")
	}
	if _, err := resolveCodexBinary(); err != nil {
		return err
	}
	if _, err := resolveGrokBinary(); err != nil {
		return err
	}
	if _, err := resolveGhBinary(); err != nil {
		return err
	}
	queueRunID, err := a.createQueueRun(ctx, filters, len(queued))
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(context.Background())

	a.agentMu.Lock()
	if a.agentStatus.Running {
		a.agentMu.Unlock()
		cancel()
		return badRequest("agent queue is already running")
	}
	a.agentStatus = AgentStatus{
		Running:    true,
		QueueRunID: queueRunID,
		Message:    "Starting queued run",
		StartedAt:  time.Now().UTC(),
		Total:      len(queued),
	}
	a.agentCancel = cancel
	a.agentMu.Unlock()
	a.publishAgentEvent("activity")
	a.publishAgentEvent("board")

	runFilters := filters
	go a.runAgentQueue(runCtx, queueRunID, runFilters, baseURL, len(queued))
	return nil
}

func (a *App) runAgentQueue(ctx context.Context, queueRunID int64, filters storyFilters, baseURL string, total int) {
	previousSummary := ""
	completed := 0
	for {
		if err := ctx.Err(); err != nil {
			a.finishAgentRun(ctx, queueRunID, "stopped", "Queue run stopped", completed, err)
			return
		}
		stories, err := a.listStories(ctx, filters)
		if err != nil {
			a.finishAgentRun(context.Background(), queueRunID, "failed", "Queue run failed", completed, err)
			return
		}
		if len(stories) == 0 {
			a.finishAgentRun(context.Background(), queueRunID, "completed", fmt.Sprintf("Queue run complete: %d/%d stories", completed, total), completed, nil)
			return
		}
		story := stories[0]
		project, err := a.getProject(ctx, story.ProjectID)
		if err != nil {
			a.finishAgentRun(context.Background(), queueRunID, "failed", "Queue run failed", completed, err)
			return
		}
		if strings.TrimSpace(project.WorkingDirectory) == "" {
			a.finishAgentRun(context.Background(), queueRunID, "failed", "Queue run paused", completed, fmt.Errorf("%s needs a project path", project.Name))
			return
		}
		a.updateAgentProgress(story.ID, fmt.Sprintf("Running %s", story.ID), completed, total)
		_ = a.updateQueueRun(context.Background(), queueRunID, "running", fmt.Sprintf("Running %s", story.ID), completed, nil)
		if err := a.changeStoryStatus(ctx, story.ID, StatusInProgress, false, "Started by agent runner"); err != nil {
			a.finishAgentRun(context.Background(), queueRunID, "failed", "Queue run failed", completed, err)
			return
		}
		a.publishAgentEvent("board")

		pc := pipelineContext{
			QueueRunID: queueRunID,
			BaseURL:    baseURL,
			Project:    project,
			Story:      story,
		}
		finalMessage, err := a.runStoryPipeline(ctx, pc, previousSummary)
		if err != nil {
			_ = a.addEvent(ctx, story.ID, "agent_failed", "Story pipeline failed: "+err.Error())
			status := "failed"
			message := "Queue run failed"
			if errors.Is(err, context.Canceled) {
				status = "stopped"
				message = "Queue run stopped"
			}
			a.finishAgentRun(context.Background(), queueRunID, status, message, completed, fmt.Errorf("%s failed: %w", story.ID, err))
			return
		}
		if err := a.changeStoryStatus(context.Background(), story.ID, StatusDone, false, "Marked done after PR merged"); err != nil {
			_ = a.addEvent(ctx, story.ID, "agent_needs_review", "PR merged, but the app could not mark the story done")
			a.finishAgentRun(context.Background(), queueRunID, "needs_review", "Queue run needs review", completed, fmt.Errorf("%s was not marked done: %w", story.ID, err))
			return
		}
		a.publishAgentEvent("board")
		completed++
		previousSummary = summarizeFinalMessage(story, finalMessage)
		_ = a.addEvent(ctx, story.ID, "agent_completed", "Story pipeline completed and PR merged")
		a.updateAgentProgress("", fmt.Sprintf("Completed %s", story.ID), completed, total)
		_ = a.updateQueueRun(context.Background(), queueRunID, "running", fmt.Sprintf("Completed %s", story.ID), completed, nil)
		a.publishAgentEvent("board")
	}
}

func (a *App) runCodexForStoryWithKind(ctx context.Context, queueRunID int64, baseURL string, project Project, story Story, prompt, runKind, branch string, prNumber int, prURL string) (string, error) {
	runDir := filepath.Join(os.TempDir(), "thetaskmanager", "runs")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		return "", err
	}
	finalPath := filepath.Join(runDir, story.ID+"-"+runKind+"-final.md")
	codexBin, err := resolveCodexBinary()
	if err != nil {
		return "", err
	}
	agentRunID, err := a.createAgentRun(context.Background(), queueRunID, project, story, prompt, runKind, branch, prNumber, prURL)
	if err != nil {
		return "", err
	}
	output := &agentRunOutput{
		flush: func(stdout, stderr string) {
			_ = a.updateAgentStoryRunOutput(context.Background(), agentRunID, stdout, stderr)
		},
	}
	args := []string{
		"exec",
		"--cd", project.WorkingDirectory,
		"--sandbox", "workspace-write",
		"-c", `approval_policy="never"`,
		"--json",
		"--output-last-message", finalPath,
	}
	if !isGitWorkTree(project.WorkingDirectory) {
		args = append(args, "--skip-git-repo-check")
	}
	args = append(args, prompt)
	cmd := osexec.CommandContext(ctx, codexBin, args...)
	cmd.Stdout = output.stdoutWriter()
	cmd.Stderr = output.stderrWriter()
	cmd.Env = append(os.Environ(),
		"TASKMANAGER_BASE_URL="+baseURL,
		"TASKMANAGER_STORY_ID="+story.ID,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Start()
	if err == nil {
		done := make(chan error, 1)
		go func() {
			done <- cmd.Wait()
		}()
		select {
		case err = <-done:
		case <-ctx.Done():
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			err = <-done
		}
	}
	output.flushNow()
	stdoutText, stderrText := output.snapshot()
	if err != nil {
		detail := stderrText
		if detail == "" {
			detail = stdoutText
		}
		status := "failed"
		if errors.Is(ctx.Err(), context.Canceled) {
			status = "stopped"
			err = context.Canceled
		}
		_ = a.finishAgentStoryRun(context.Background(), agentRunID, status, stdoutText, stderrText, "", err)
		if detail != "" {
			return "", fmt.Errorf("%w: %s", err, truncate(detail, 800))
		}
		return "", err
	}
	final, err := os.ReadFile(finalPath)
	if err != nil {
		finalMessage := stdoutText
		_ = a.finishAgentStoryRun(context.Background(), agentRunID, "completed", stdoutText, stderrText, finalMessage, nil)
		return finalMessage, nil
	}
	finalMessage := strings.TrimSpace(string(final))
	_ = a.finishAgentStoryRun(context.Background(), agentRunID, "completed", stdoutText, stderrText, finalMessage, nil)
	return finalMessage, nil
}

func (a *App) updateAgentProgress(storyID, message string, completed, total int) {
	a.agentMu.Lock()
	defer a.agentMu.Unlock()
	a.agentStatus.Running = true
	a.agentStatus.CurrentStoryID = storyID
	a.agentStatus.Message = message
	a.agentStatus.LastError = ""
	a.agentStatus.Completed = completed
	a.agentStatus.Total = total
	a.publishAgentEvent("activity")
}

func (a *App) finishAgentRun(ctx context.Context, queueRunID int64, status, message string, completed int, err error) {
	_ = a.updateQueueRun(ctx, queueRunID, status, message, completed, err)
	a.agentMu.Lock()
	a.agentStatus.Running = false
	a.agentStatus.QueueRunID = queueRunID
	a.agentStatus.CurrentStoryID = ""
	a.agentStatus.Message = message
	a.agentStatus.FinishedAt = time.Now().UTC()
	if err != nil && status != "stopped" {
		a.agentStatus.LastError = truncate(err.Error(), 180)
	} else {
		a.agentStatus.LastError = ""
	}
	a.agentCancel = nil
	a.agentMu.Unlock()
	a.publishAgentEvent("activity")
	a.publishAgentEvent("board")
}

func (a *App) stopAgentQueue() error {
	a.agentMu.Lock()
	cancel := a.agentCancel
	running := a.agentStatus.Running
	if running {
		a.agentStatus.Message = "Stopping queued run"
	}
	a.agentMu.Unlock()
	a.publishAgentEvent("activity")
	if !running || cancel == nil {
		return badRequest("agent queue is not running")
	}
	cancel()
	return nil
}

func (a *App) queueBacklogStories(ctx context.Context, storyIDs []string, filters storyFilters) error {
	var stories []Story
	var err error
	if len(storyIDs) > 0 {
		for _, id := range storyIDs {
			story, err := a.getStory(ctx, id)
			if err != nil {
				return err
			}
			stories = append(stories, story)
		}
	} else {
		filters.Status = StatusBacklog
		filters.ShowClosed = true
		stories, err = a.listStories(ctx, filters)
		if err != nil {
			return err
		}
	}
	if len(stories) == 0 {
		return nil
	}
	for _, story := range stories {
		if story.Status != StatusBacklog {
			return badRequest("only backlog stories can be queued in bulk")
		}
	}
	for _, story := range stories {
		if err := a.changeStoryStatus(ctx, story.ID, StatusQueued, false, "Queued from board UI"); err != nil {
			return err
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

func (a *App) boardData(ctx context.Context, projectID, epicID string, showClosed bool, storyID string) (BoardData, error) {
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
	columns := []string{StatusQueued, StatusBacklog, StatusInProgress, StatusDone}
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
	data := BoardData{
		Projects: projects, Epics: epics, StoriesByCol: byCol,
		SelectedProject: projectID, SelectedEpic: epicID,
		ShowClosed: showClosed, StatusColumns: columns,
	}
	data.Agent, err = a.agentPanelData(ctx, byCol[StatusQueued], projects, projectID, epicID)
	if err != nil {
		return BoardData{}, err
	}
	dashboard, err := a.dashboardData(ctx, projects, projectID, epicID)
	if err != nil {
		return BoardData{}, err
	}
	data.Dashboard = dashboard
	if storyID != "" {
		story, err := a.getStory(ctx, storyID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return data, nil
			}
			return BoardData{}, err
		}
		events, err := a.listEvents(ctx, story.ID)
		if err != nil {
			return BoardData{}, err
		}
		data.HasDetailStory = true
		data.Detail = StoryPanelData{Story: story, Events: events}
	}
	return data, nil
}

func (a *App) dashboardData(ctx context.Context, projects []Project, projectID, epicID string) (DashboardData, error) {
	if projectID != "" && epicID != "" {
		project, err := a.getProject(ctx, projectID)
		if err != nil {
			return DashboardData{}, err
		}
		epic, err := a.getEpic(ctx, epicID)
		if err != nil {
			return DashboardData{}, err
		}
		stories, err := a.listStories(ctx, storyFilters{ProjectID: projectID, EpicID: epicID, ShowClosed: true})
		if err != nil {
			return DashboardData{}, err
		}
		data := DashboardData{
			Scope:   "epic",
			Project: project,
			Epic:    epic,
			Counts:  countStatuses(stories),
		}
		if err := a.addHeatmap(ctx, &data, projectID, epicID); err != nil {
			return DashboardData{}, err
		}
		return data, nil
	}
	if projectID != "" {
		project, err := a.getProject(ctx, projectID)
		if err != nil {
			return DashboardData{}, err
		}
		projectEpics, err := a.listEpics(ctx, projectID)
		if err != nil {
			return DashboardData{}, err
		}
		stories, err := a.listStories(ctx, storyFilters{ProjectID: projectID, ShowClosed: true})
		if err != nil {
			return DashboardData{}, err
		}
		data := DashboardData{
			Scope:     "project",
			Project:   project,
			EpicCount: len(projectEpics),
			Counts:    countStatuses(stories),
		}
		if err := a.addHeatmap(ctx, &data, projectID, ""); err != nil {
			return DashboardData{}, err
		}
		return data, nil
	}

	allEpics, err := a.listEpics(ctx, "")
	if err != nil {
		return DashboardData{}, err
	}
	data := DashboardData{
		Scope:        "all",
		ProjectCount: len(projects),
		EpicCount:    len(allEpics),
	}
	epicsByProject := make(map[string]int)
	for _, epic := range allEpics {
		epicsByProject[epic.ProjectID]++
	}
	for _, project := range projects {
		stories, err := a.listStories(ctx, storyFilters{ProjectID: project.ID, ShowClosed: true})
		if err != nil {
			return DashboardData{}, err
		}
		counts := countStatuses(stories)
		data.Counts.Backlog += counts.Backlog
		data.Counts.Queued += counts.Queued
		data.Counts.InProgress += counts.InProgress
		data.Counts.Done += counts.Done
		data.Counts.Closed += counts.Closed
		data.Counts.Total += counts.Total
		data.Projects = append(data.Projects, ProjectDashboard{
			Project:   project,
			EpicCount: epicsByProject[project.ID],
			Counts:    counts,
		})
	}
	if err := a.addHeatmap(ctx, &data, "", ""); err != nil {
		return DashboardData{}, err
	}
	return data, nil
}

func (a *App) addHeatmap(ctx context.Context, data *DashboardData, projectID, epicID string) error {
	heatmap, total, err := a.completionHeatmap(ctx, projectID, epicID)
	if err != nil {
		return err
	}
	data.Heatmap = heatmap
	data.HeatmapTotal = total
	return nil
}

func (a *App) completionHeatmap(ctx context.Context, projectID, epicID string) ([]HeatmapDay, int, error) {
	const heatmapDays = 62
	today := dateOnly(time.Now().UTC())
	start := today.AddDate(0, 0, -(heatmapDays - 1))
	end := today.AddDate(0, 0, 1)

	counts, err := a.completionCountsByDay(ctx, projectID, epicID, start, end)
	if err != nil {
		return nil, 0, err
	}

	total := 0
	maxCount := 0
	days := make([]HeatmapDay, 0, int(end.Sub(start).Hours()/24))
	for day := start; day.Before(end); day = day.AddDate(0, 0, 1) {
		key := day.Format("2006-01-02")
		count := counts[key]
		if count > maxCount {
			maxCount = count
		}
		total += count
		days = append(days, HeatmapDay{Date: key, Count: count})
	}
	for i := range days {
		days[i].Level = heatmapLevel(days[i].Count, maxCount)
	}
	return days, total, nil
}

func (a *App) completionCountsByDay(ctx context.Context, projectID, epicID string, start, end time.Time) (map[string]int, error) {
	eventWhere := []string{`e.type = ?`, `e.message LIKE ?`, `e.created_at >= ?`, `e.created_at < ?`}
	eventArgs := []any{"status_changed", "% -> done", formatTime(start), formatTime(end)}
	storyWhere := []string{`s.status = ?`, `s.created_at >= ?`, `s.created_at < ?`, `NOT EXISTS (
		SELECT 1 FROM story_events done_events
		WHERE done_events.story_id = s.id
			AND done_events.type = ?
			AND done_events.message LIKE ?
	)`}
	storyArgs := []any{StatusDone, formatTime(start), formatTime(end), "status_changed", "% -> done"}
	if projectID != "" {
		eventWhere = append(eventWhere, `s.project_id = ?`)
		eventArgs = append(eventArgs, projectID)
		storyWhere = append(storyWhere, `s.project_id = ?`)
		storyArgs = append(storyArgs, projectID)
	}
	if epicID != "" {
		eventWhere = append(eventWhere, `s.epic_id = ?`)
		eventArgs = append(eventArgs, epicID)
		storyWhere = append(storyWhere, `s.epic_id = ?`)
		storyArgs = append(storyArgs, epicID)
	}

	query := fmt.Sprintf(`SELECT day, COUNT(*) FROM (
		SELECT substr(e.created_at, 1, 10) AS day
		FROM story_events e
		JOIN stories s ON s.id = e.story_id
		WHERE %s
		UNION ALL
		SELECT substr(s.created_at, 1, 10) AS day
		FROM stories s
		WHERE %s
	) completions
	GROUP BY day`, strings.Join(eventWhere, " AND "), strings.Join(storyWhere, " AND "))
	args := append(eventArgs, storyArgs...)

	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[string]int)
	for rows.Next() {
		var day string
		var count int
		if err := rows.Scan(&day, &count); err != nil {
			return nil, err
		}
		counts[day] = count
	}
	return counts, rows.Err()
}

func heatmapLevel(count, maxCount int) int {
	switch {
	case count == 0:
		return 0
	case maxCount <= 1:
		return 4
	case count*4 >= maxCount*3:
		return 4
	case count*2 >= maxCount:
		return 3
	case count*4 >= maxCount:
		return 2
	default:
		return 1
	}
}

func dateOnly(t time.Time) time.Time {
	year, month, day := t.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

func countStatuses(stories []Story) StatusCounts {
	var counts StatusCounts
	for _, story := range stories {
		counts.Total++
		switch story.Status {
		case StatusBacklog:
			counts.Backlog++
		case StatusQueued:
			counts.Queued++
		case StatusInProgress:
			counts.InProgress++
		case StatusDone:
			counts.Done++
		case StatusClosed:
			counts.Closed++
		}
	}
	return counts
}

func (a *App) listProjects(ctx context.Context) ([]Project, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT id, name, prefix, working_directory, next_story_number, created_at, updated_at FROM projects ORDER BY name`)
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
	row := a.db.QueryRowContext(ctx, `SELECT id, name, prefix, working_directory, next_story_number, created_at, updated_at FROM projects WHERE id = ?`, id)
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
	if err := row.Scan(&p.ID, &p.Name, &p.Prefix, &p.WorkingDirectory, &p.NextStoryNumber, &created, &updated); err != nil {
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

func scanQueueRun(row rowScanner) (QueueRunSummary, error) {
	var r QueueRunSummary
	var started string
	var finished sql.NullString
	if err := row.Scan(&r.ID, &r.Status, &r.ProjectID, &r.EpicID, &r.Total, &r.Completed, &r.Message, &r.Error, &started, &finished); err != nil {
		return QueueRunSummary{}, err
	}
	r.StartedAt = parseTime(started)
	if finished.Valid {
		t := parseTime(finished.String)
		r.FinishedAt = &t
	}
	return r, nil
}

func scanAgentRun(row rowScanner) (AgentRunSummary, error) {
	var r AgentRunSummary
	var title, finished sql.NullString
	var started string
	if err := row.Scan(&r.ID, &r.QueueRunID, &r.StoryID, &title, &r.RunKind, &r.Status, &r.WorkingDirectory, &r.Branch, &r.PRNumber, &r.PRURL, &r.Stdout, &r.Stderr, &r.FinalMessage, &r.ExitError, &started, &finished); err != nil {
		return AgentRunSummary{}, err
	}
	if title.Valid {
		r.StoryTitle = title.String
	}
	r.StartedAt = parseTime(started)
	if finished.Valid {
		t := parseTime(finished.String)
		r.FinishedAt = &t
	}
	r.LogItems = agentLogItems(r)
	return r, nil
}

func runKindTitle(kind string) string {
	switch kind {
	case RunKindGrokReview:
		return "Grok review"
	case RunKindCodexFix:
		return "Codex fix"
	default:
		return "Codex implement"
	}
}

func runKindSource(kind string) string {
	if kind == RunKindGrokReview {
		return "grok"
	}
	return "codex"
}

func agentLogItems(run AgentRunSummary) []AgentLogItem {
	if run.RunKind == RunKindGrokReview {
		return grokLogItems(run)
	}
	type codexItem struct {
		Type             string `json:"type"`
		Text             string `json:"text"`
		Command          string `json:"command"`
		AggregatedOutput string `json:"aggregated_output"`
		Status           string `json:"status"`
		ExitCode         *int   `json:"exit_code"`
	}
	type codexEvent struct {
		Type     string    `json:"type"`
		ThreadID string    `json:"thread_id"`
		Item     codexItem `json:"item"`
	}

	items := []AgentLogItem{}
	add := func(kind, text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		items = append(items, AgentLogItem{Kind: kind, Text: truncate(text, 700)})
	}

	for _, line := range strings.Split(run.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var event codexEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		switch event.Type {
		case "thread.started":
			add("meta", "Codex thread started: "+event.ThreadID)
		case "turn.started":
			add("meta", "Codex turn started")
		case "turn.completed":
			add("success", "Codex turn completed")
		case "item.started":
			if event.Item.Type == "command_execution" {
				add("command", "Running command: "+event.Item.Command)
			}
		case "item.completed":
			switch event.Item.Type {
			case "agent_message":
				add("message", event.Item.Text)
			case "command_execution":
				status := "Command finished"
				kind := "success"
				if event.Item.ExitCode != nil {
					status = fmt.Sprintf("Command exited %d", *event.Item.ExitCode)
					if *event.Item.ExitCode != 0 {
						kind = "error"
					}
				}
				detail := status + ": " + event.Item.Command
				if strings.TrimSpace(event.Item.AggregatedOutput) != "" {
					detail += "\n" + strings.TrimSpace(event.Item.AggregatedOutput)
				}
				add(kind, detail)
			}
		}
	}
	if strings.TrimSpace(run.ExitError) != "" {
		add("error", run.ExitError)
	}
	if strings.TrimSpace(run.Stderr) != "" {
		add("error", run.Stderr)
	}
	if strings.TrimSpace(run.FinalMessage) != "" {
		add("summary", run.FinalMessage)
	}
	if len(items) > 24 {
		items = items[len(items)-24:]
	}
	return items
}

func grokLogItems(run AgentRunSummary) []AgentLogItem {
	items := []AgentLogItem{}
	add := func(kind, text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		items = append(items, AgentLogItem{Kind: kind, Text: truncate(text, 700)})
	}
	if text, err := extractGrokJSONText(run.Stdout); err == nil && strings.TrimSpace(text) != "" {
		if review, err := parseGrokReview(text); err == nil {
			if review.Approved {
				add("success", "Grok approved: "+review.Summary)
			} else {
				add("message", "Grok requested changes: "+review.Summary)
			}
			for _, comment := range review.Comments {
				body := strings.TrimSpace(comment.Body)
				if body == "" {
					continue
				}
				if strings.TrimSpace(comment.Path) != "" {
					add("message", fmt.Sprintf("%s — %s", comment.Path, body))
				} else {
					add("message", body)
				}
			}
		} else {
			add("summary", text)
		}
	}
	if strings.TrimSpace(run.ExitError) != "" {
		add("error", run.ExitError)
	}
	if strings.TrimSpace(run.FinalMessage) != "" {
		add("summary", run.FinalMessage)
	}
	if len(items) == 0 && strings.TrimSpace(run.Stdout) != "" {
		add("summary", run.Stdout)
	}
	return items
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
	case StatusQueued:
		return "Queued"
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

func normalizeWorkingDirectory(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	path, err := expandUserPath(path)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", badRequest("working directory does not exist")
	}
	if !info.IsDir() {
		return "", badRequest("working directory must be a directory")
	}
	return abs, nil
}

func expandUserPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if strings.HasPrefix(path, "~/") || path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	}
	return path, nil
}

func resolveCodexBinary() (string, error) {
	candidates := []string{}
	if configured := strings.TrimSpace(os.Getenv("TASKMANAGER_CODEX_BIN")); configured != "" {
		candidates = append(candidates, configured)
	}
	candidates = append(candidates,
		"codex",
		"/opt/homebrew/bin/codex",
		"/usr/local/bin/codex",
		"/Applications/Codex.app/Contents/Resources/codex",
	)
	for _, candidate := range candidates {
		if strings.ContainsRune(candidate, filepath.Separator) {
			expanded, err := expandUserPath(candidate)
			if err != nil {
				return "", err
			}
			info, err := os.Stat(expanded)
			if err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
				return expanded, nil
			}
			continue
		}
		if path, err := osexec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", badRequest("Codex CLI was not found. Set TASKMANAGER_CODEX_BIN to the codex executable path, for example /Applications/Codex.app/Contents/Resources/codex or /opt/homebrew/bin/codex.")
}

func isGitWorkTree(dir string) bool {
	cmd := osexec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return false
	}
	return strings.TrimSpace(out.String()) == "true"
}

func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "localhost:8080"
	}
	return scheme + "://" + host
}

func summarizeFinalMessage(story Story, finalMessage string) string {
	finalMessage = strings.TrimSpace(finalMessage)
	if finalMessage == "" {
		finalMessage = "Story pipeline completed and PR merged."
	}
	return fmt.Sprintf("%s - %s\n%s", story.ID, story.Title, truncate(finalMessage, 1200))
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
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
