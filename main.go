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
	StatusInReview   = "in_review"
	StatusDone       = "done"
	StatusClosed     = "closed"

	AutonomyAutonomous = "autonomous"
	AutonomySupervised = "supervised"

	QueueItemAwaitingHuman = "awaiting_human"
	QueueItemCompleted     = "completed"
	QueueItemFailed        = "failed"
	QueueItemRunning       = "running"
	QueueItemStopped       = "stopped"
	QueueItemSkipped       = "skipped"
)

var validStatuses = map[string]bool{
	StatusBacklog:    true,
	StatusQueued:     true,
	StatusInProgress: true,
	StatusInReview:   true,
	StatusDone:       true,
	StatusClosed:     true,
}

var uiWritableStatuses = map[string]bool{
	StatusBacklog:    true,
	StatusQueued:     true,
	StatusInProgress: true,
	StatusInReview:   true,
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
	AutonomyMode     string    `json:"autonomyMode"`
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
	QueuePosition *int       `json:"-"`
}

type QueueRunItem struct {
	ID         int64
	QueueRunID int64
	Story      Story
	Position   int
	Status     string
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
	Story    Story
	Events   []StoryEvent
	Pipeline *StoryPipeline
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
	Project          Project
	Path             string
	Parent           string
	Home             string
	Directories      []FolderEntry
	Error            string
	SuggestedGitRoot string
	GitRootDiffers   bool
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

type PageData struct {
	Page           string
	Projects       []Project
	Project        Project
	Dashboard      DashboardData
	Backlog        BacklogPageData
	Run            RunPageData
	HasDetailStory bool
	Detail         StoryPanelData
	CurrentAgent   AgentStatus
	ActiveRun      QueueRunSummary
	SettingsAgents SettingsAgentsData
}

type BacklogPageData struct {
	Project        Project
	Epics          []Epic
	Stories        []Story
	Queued         []Story
	SelectedEpic   string
	SelectedStatus string
	Counts         StatusCounts
}

type RunPageData struct {
	Project     Project
	Run         QueueRunSummary
	Runs        []QueueRunSummary
	Items       []QueueRunItem
	LiveQueue   []Story
	Activity    AgentActivityData
	Agent       AgentStatus
	MissingPath bool
	Legacy      bool
	Summary     RunCompletionSummary
}

type RunCompletionSummary struct {
	Elapsed            string
	AgentSteps         int
	PullRequests       []RunPullRequest
	MergedPRs          []RunPullRequest
	AwaitingHumanPRs   []RunPullRequest
	OpenPRs            []RunPullRequest
	MergedCount        int
	AwaitingHumanCount int
}

type RunPullRequest struct {
	StoryID    string
	StoryTitle string
	Number     int
	URL        string
	Branch     string
	// Outcome is "merged", "awaiting_human", or "open" for run completion UI.
	Outcome string
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
	InReview   int
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
		addr   = flag.String("addr", defaultEnv("RIPPLE_ADDR", defaultEnv("TASKMANAGER_ADDR", ":8080")), "HTTP listen address")
		dbPath = flag.String("db", defaultRippleDBPath(), "SQLite database path")
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

	log.Printf("Ripple listening on http://localhost%s", strings.TrimPrefix(*addr, "0.0.0.0"))
	log.Fatal(http.ListenAndServe(*addr, app.routes()))
}

func NewApp(db *sql.DB) (*App, error) {
	db.SetMaxOpenConns(1)
	funcs := template.FuncMap{
		"statusTitle":          statusTitle,
		"runKindTitle":         runKindTitle,
		"runKindSource":        runKindSource,
		"eventTitle":           eventTitle,
		"queueItemStatusTitle": queueItemStatusTitle,
		"truncateText":         truncate,
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

	mux.HandleFunc("GET /", a.handleDashboard)
	mux.HandleFunc("GET /about", a.handleAbout)
	mux.HandleFunc("GET /settings", a.handleSettings)
	mux.HandleFunc("POST /settings/agents", a.handleUIAgentSettings)
	mux.HandleFunc("POST /settings/agents/api-providers", a.handleUICreateAPIProvider)
	mux.HandleFunc("POST /settings/agents/api-providers/{id}", a.handleUIUpdateAPIProvider)
	mux.HandleFunc("POST /settings/agents/api-providers/{id}/delete", a.handleUIDeleteAPIProvider)
	mux.HandleFunc("POST /settings/agents/api-providers/{id}/test", a.handleUITestAPIProvider)
	mux.HandleFunc("GET /board", a.handleBoardPartial)
	mux.HandleFunc("GET /projects/{id}/backlog", a.handleProjectBacklog)
	mux.HandleFunc("GET /projects/{id}/run", a.handleProjectRun)
	mux.HandleFunc("GET /projects/{id}/run/content", a.handleProjectRunContent)
	mux.HandleFunc("GET /projects/{id}/runs/{runID}", a.handleProjectRun)
	mux.HandleFunc("POST /projects/{id}/queue/reorder", a.handleUIQueueReorder)
	mux.HandleFunc("GET /stories/{id}/panel", a.handleStoryPanel)
	mux.HandleFunc("POST /stories/{id}/status", a.handleUIStatus)
	mux.HandleFunc("POST /stories/{id}/description", a.handleUIDescription)
	mux.HandleFunc("POST /stories/{id}/address-feedback", a.handleUIAddressFeedback)
	mux.HandleFunc("POST /stories/{id}/merge", a.handleUIMergeStory)
	mux.HandleFunc("POST /stories/{id}/sync-pr", a.handleUISyncPR)
	mux.HandleFunc("POST /stories/{id}/close", a.handleUIClose)
	mux.HandleFunc("POST /stories/close-done", a.handleUICloseDone)
	mux.HandleFunc("POST /stories/queue-backlog", a.handleUIQueueBacklog)
	mux.HandleFunc("POST /projects/{id}/working-directory", a.handleUIProjectWorkingDirectory)
	mux.HandleFunc("POST /projects/{id}/settings", a.handleUIProjectSettings)
	mux.HandleFunc("GET /projects/{id}/setup-status", a.handleProjectSetupStatus)
	mux.HandleFunc("POST /projects/{id}/use-git-root", a.handleUIUseGitRoot)
	mux.HandleFunc("POST /projects/{id}/clone", a.handleUICloneRepo)
	mux.HandleFunc("POST /projects", a.handleUICreateProject)
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

func (a *App) handleAbout(w http.ResponseWriter, r *http.Request) {
	projects, err := a.listProjects(r.Context())
	if err != nil {
		httpError(w, err)
		return
	}
	a.render(w, "layout.html", PageData{Page: "about", Projects: projects, CurrentAgent: a.currentAgentStatus()})
}

func (a *App) handleSettings(w http.ResponseWriter, r *http.Request) {
	projects, err := a.listProjects(r.Context())
	if err != nil {
		httpError(w, err)
		return
	}
	flash := strings.TrimSpace(r.URL.Query().Get("flash"))
	agents, err := a.settingsAgentsData(r.Context(), flash)
	if err != nil {
		httpError(w, err)
		return
	}
	a.render(w, "layout.html", PageData{
		Page:           "settings",
		Projects:       projects,
		CurrentAgent:   a.currentAgentStatus(),
		SettingsAgents: agents,
	})
}

func (a *App) handleUIAgentSettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpError(w, err)
		return
	}
	err := a.saveAgentSettingsFromForm(r.Context(),
		r.FormValue("implementerProviderId"),
		r.FormValue("reviewerProviderId"),
		r.FormValue("codexBinaryPath"),
		r.FormValue("grokBinaryPath"),
	)
	if err != nil {
		httpError(w, err)
		return
	}
	http.Redirect(w, r, "/settings?flash=saved#agents", http.StatusSeeOther)
}

func (a *App) handleUICreateAPIProvider(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpError(w, err)
		return
	}
	_, err := a.createAPIProvider(r.Context(),
		r.FormValue("name"),
		r.FormValue("baseUrl"),
		r.FormValue("apiKey"),
		r.FormValue("model"),
	)
	if err != nil {
		httpError(w, err)
		return
	}
	http.Redirect(w, r, "/settings?flash=api_saved#api-providers", http.StatusSeeOther)
}

func (a *App) handleUIUpdateAPIProvider(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpError(w, err)
		return
	}
	err := a.updateAPIProvider(r.Context(), r.PathValue("id"),
		r.FormValue("name"),
		r.FormValue("baseUrl"),
		r.FormValue("apiKey"),
		r.FormValue("model"),
	)
	if err != nil {
		httpError(w, err)
		return
	}
	http.Redirect(w, r, "/settings?flash=api_saved#api-providers", http.StatusSeeOther)
}

func (a *App) handleUIDeleteAPIProvider(w http.ResponseWriter, r *http.Request) {
	if err := a.deleteAPIProvider(r.Context(), r.PathValue("id")); err != nil {
		httpError(w, err)
		return
	}
	http.Redirect(w, r, "/settings?flash=api_deleted#api-providers", http.StatusSeeOther)
}

func (a *App) handleUITestAPIProvider(w http.ResponseWriter, r *http.Request) {
	if err := a.testAPIProviderConnection(r.Context(), r.PathValue("id")); err != nil {
		// Surface a soft redirect with failure flash rather than raw JSON for form posts.
		http.Redirect(w, r, "/settings?flash=api_test_failed#api-providers", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings?flash=api_tested#api-providers", http.StatusSeeOther)
}

func (a *App) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	projects, err := a.listProjects(r.Context())
	if err != nil {
		httpError(w, err)
		return
	}
	dashboard, err := a.dashboardData(r.Context(), projects, "", "")
	if err != nil {
		httpError(w, err)
		return
	}
	current := a.currentAgentStatus()
	var activeRun QueueRunSummary
	if current.Running && current.QueueRunID != 0 {
		activeRun, _ = a.getQueueRun(r.Context(), current.QueueRunID)
	}
	a.render(w, "layout.html", PageData{Page: "dashboard", Projects: projects, Dashboard: dashboard, CurrentAgent: current, ActiveRun: activeRun})
}

func (a *App) handleProjectBacklog(w http.ResponseWriter, r *http.Request) {
	data, err := a.projectBacklogPageData(r)
	if err != nil {
		httpError(w, err)
		return
	}
	a.render(w, "layout.html", data)
}

func (a *App) projectBacklogPageData(r *http.Request) (PageData, error) {
	project, err := a.getProject(r.Context(), r.PathValue("id"))
	if err != nil {
		return PageData{}, err
	}
	projects, err := a.listProjects(r.Context())
	if err != nil {
		return PageData{}, err
	}
	epics, err := a.listEpics(r.Context(), project.ID)
	if err != nil {
		return PageData{}, err
	}
	epicID := strings.TrimSpace(r.URL.Query().Get("epicId"))
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status == "" {
		status = StatusBacklog
	}
	if !validStatuses[status] {
		return PageData{}, badRequest("invalid status filter")
	}
	projectStories, err := a.listStories(r.Context(), storyFilters{ProjectID: project.ID, ShowClosed: true})
	if err != nil {
		return PageData{}, err
	}
	all := projectStories
	if epicID != "" {
		all = []Story{}
		for _, story := range projectStories {
			if story.EpicID != nil && *story.EpicID == epicID {
				all = append(all, story)
			}
		}
	}
	stories := []Story{}
	queued := []Story{}
	for _, story := range projectStories {
		if story.Status == StatusQueued {
			queued = append(queued, story)
		}
	}
	for _, story := range all {
		if story.Status == status {
			stories = append(stories, story)
		}
	}
	data := PageData{Page: "backlog", Projects: projects, Project: project, Backlog: BacklogPageData{
		Project: project, Epics: epics, Stories: stories, Queued: queued, SelectedEpic: epicID, SelectedStatus: status, Counts: countStatuses(projectStories),
	}, CurrentAgent: a.currentAgentStatus()}
	if storyID := strings.TrimSpace(r.URL.Query().Get("storyId")); storyID != "" {
		story, err := a.getStory(r.Context(), storyID)
		if err == nil && story.ProjectID == project.ID {
			panel, panelErr := a.storyPanelData(r.Context(), story.ID)
			if panelErr != nil {
				return PageData{}, panelErr
			}
			data.HasDetailStory = true
			data.Detail = panel
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return PageData{}, err
		}
	}
	return data, nil
}

func (a *App) handleProjectRun(w http.ResponseWriter, r *http.Request) {
	data, err := a.projectRunPageData(r)
	if err != nil {
		httpError(w, err)
		return
	}
	a.render(w, "layout.html", data)
}

func (a *App) handleProjectRunContent(w http.ResponseWriter, r *http.Request) {
	data, err := a.projectRunPageData(r)
	if err != nil {
		httpError(w, err)
		return
	}
	a.render(w, "run_content.html", data)
}

func (a *App) projectRunPageData(r *http.Request) (PageData, error) {
	project, err := a.getProject(r.Context(), r.PathValue("id"))
	if err != nil {
		return PageData{}, err
	}
	projects, err := a.listProjects(r.Context())
	if err != nil {
		return PageData{}, err
	}
	runs, err := a.listQueueRuns(r.Context(), project.ID)
	if err != nil {
		return PageData{}, err
	}
	var selected QueueRunSummary
	rawRunID := strings.TrimSpace(r.PathValue("runID"))
	if rawRunID == "" {
		rawRunID = strings.TrimSpace(r.URL.Query().Get("runId"))
	}
	if raw := rawRunID; raw != "" {
		var runID int64
		if _, err := fmt.Sscanf(raw, "%d", &runID); err != nil {
			return PageData{}, badRequest("invalid run id")
		}
		selected, err = a.getQueueRun(r.Context(), runID)
		if err != nil {
			return PageData{}, err
		}
		if selected.ProjectID != project.ID {
			return PageData{}, sql.ErrNoRows
		}
	} else if r.URL.Query().Get("new") != "1" && len(runs) > 0 {
		selected = runs[0]
	}
	queued, err := a.listStories(r.Context(), storyFilters{ProjectID: project.ID, Status: StatusQueued, ShowClosed: true})
	if err != nil {
		return PageData{}, err
	}
	runData := RunPageData{Project: project, Run: selected, Runs: runs, LiveQueue: queued, Agent: a.currentAgentStatus(), MissingPath: strings.TrimSpace(project.WorkingDirectory) == ""}
	if selected.ID != 0 {
		runData.Items, err = a.listQueueRunItems(r.Context(), selected.ID)
		if err != nil {
			return PageData{}, err
		}
		runData.Activity = AgentActivityData{LatestRun: selected}
		runData.Activity.StoryRuns, err = a.listAgentStoryRuns(r.Context(), selected.ID)
		if err != nil {
			return PageData{}, err
		}
		if len(runData.Items) == 0 && len(runData.Activity.StoryRuns) > 0 {
			runData.Legacy = true
			seen := map[string]bool{}
			for _, storyRun := range runData.Activity.StoryRuns {
				if seen[storyRun.StoryID] {
					continue
				}
				seen[storyRun.StoryID] = true
				story, getErr := a.getStory(r.Context(), storyRun.StoryID)
				if getErr == nil {
					runData.Items = append(runData.Items, QueueRunItem{QueueRunID: selected.ID, Story: story, Position: len(runData.Items) + 1, Status: storyRun.Status})
				}
			}
		}
		runData.Summary = buildRunCompletionSummary(selected, runData.Activity.StoryRuns, runData.Items)
	}
	return PageData{Page: "run", Projects: projects, Project: project, Run: runData, CurrentAgent: a.currentAgentStatus()}, nil
}

func buildRunCompletionSummary(run QueueRunSummary, storyRuns []AgentRunSummary, items []QueueRunItem) RunCompletionSummary {
	summary := RunCompletionSummary{AgentSteps: len(storyRuns)}
	if run.FinishedAt != nil {
		summary.Elapsed = formatElapsed(run.FinishedAt.Sub(run.StartedAt))
	}
	itemStatus := map[string]string{}
	for _, item := range items {
		itemStatus[item.Story.ID] = item.Status
	}
	seen := map[string]bool{}
	for _, storyRun := range storyRuns {
		if storyRun.PRNumber == 0 || strings.TrimSpace(storyRun.PRURL) == "" {
			continue
		}
		key := storyRun.PRURL
		if seen[key] {
			continue
		}
		seen[key] = true
		outcome := prOutcomeForRunItem(itemStatus[storyRun.StoryID])
		pr := RunPullRequest{
			StoryID: storyRun.StoryID, StoryTitle: storyRun.StoryTitle, Number: storyRun.PRNumber, URL: storyRun.PRURL, Branch: storyRun.Branch, Outcome: outcome,
		}
		summary.PullRequests = append(summary.PullRequests, pr)
		switch outcome {
		case "merged":
			summary.MergedPRs = append(summary.MergedPRs, pr)
			summary.MergedCount++
		case "awaiting_human":
			summary.AwaitingHumanPRs = append(summary.AwaitingHumanPRs, pr)
		default:
			summary.OpenPRs = append(summary.OpenPRs, pr)
		}
	}
	// Prefer queue-item outcomes for "waiting on you" so the count matches the sidebar.
	for _, item := range items {
		if item.Status == QueueItemAwaitingHuman {
			summary.AwaitingHumanCount++
		}
	}
	return summary
}

func prOutcomeForRunItem(itemStatus string) string {
	switch itemStatus {
	case QueueItemAwaitingHuman:
		return "awaiting_human"
	case QueueItemCompleted:
		return "merged"
	default:
		return "open"
	}
}

func formatElapsed(duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	seconds := int(duration.Round(time.Second).Seconds())
	hours, seconds := seconds/3600, seconds%3600
	minutes, seconds := seconds/60, seconds%60
	if hours > 0 {
		return fmt.Sprintf("%dh %dm %ds", hours, minutes, seconds)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
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
			autonomy_mode TEXT NOT NULL DEFAULT 'autonomous',
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
		`CREATE TABLE IF NOT EXISTS queue_run_items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			queue_run_id INTEGER NOT NULL REFERENCES queue_runs(id),
			story_id TEXT NOT NULL REFERENCES stories(id),
			position INTEGER NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			UNIQUE(queue_run_id, story_id),
			UNIQUE(queue_run_id, position)
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
	if err := a.ensureColumn(ctx, "projects", "autonomy_mode", "TEXT NOT NULL DEFAULT 'autonomous'"); err != nil {
		return err
	}
	if err := a.ensureColumn(ctx, "stories", "queue_position", "INTEGER"); err != nil {
		return err
	}
	if _, err := a.db.ExecContext(ctx, `WITH ranked AS (
		SELECT id, ROW_NUMBER() OVER (PARTITION BY project_id ORDER BY created_at, id) AS position
		FROM stories WHERE status = ?
	) UPDATE stories SET queue_position = (SELECT position FROM ranked WHERE ranked.id = stories.id)
	WHERE status = ? AND queue_position IS NULL`, StatusQueued, StatusQueued); err != nil {
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
	if err := a.ensureAgentSettings(ctx); err != nil {
		return err
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

func (a *App) finishUIAction(w http.ResponseWriter, r *http.Request) {
	redirect := strings.TrimSpace(r.FormValue("redirect"))
	if strings.HasPrefix(redirect, "/") && !strings.HasPrefix(redirect, "//") {
		http.Redirect(w, r, redirect, http.StatusSeeOther)
		return
	}
	a.renderBoardPartial(w, r)
}

func (a *App) handleStoryPanel(w http.ResponseWriter, r *http.Request) {
	data, err := a.storyPanelData(r.Context(), r.PathValue("id"))
	if err != nil {
		httpError(w, err)
		return
	}
	a.render(w, "story_panel.html", data)
}

func (a *App) storyPanelData(ctx context.Context, storyID string) (StoryPanelData, error) {
	story, err := a.getStory(ctx, storyID)
	if err != nil {
		return StoryPanelData{}, err
	}
	events, err := a.listEvents(ctx, story.ID)
	if err != nil {
		return StoryPanelData{}, err
	}
	data := StoryPanelData{Story: story, Events: events}
	if pipeline, err := a.getLatestStoryPipeline(ctx, story.ID); err == nil {
		p := pipeline
		data.Pipeline = &p
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return StoryPanelData{}, err
	}
	return data, nil
}

func (a *App) handleUIAddressFeedback(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpError(w, err)
		return
	}
	if err := a.startAddressFeedback(r.PathValue("id"), requestBaseURL(r)); err != nil {
		httpError(w, err)
		return
	}
	a.finishUIAction(w, r)
}

func (a *App) handleUIMergeStory(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpError(w, err)
		return
	}
	if err := a.startHumanMerge(r.PathValue("id")); err != nil {
		httpError(w, err)
		return
	}
	a.finishUIAction(w, r)
}

func (a *App) handleUISyncPR(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpError(w, err)
		return
	}
	if err := a.syncExternalPRMerge(r.Context(), r.PathValue("id")); err != nil {
		httpError(w, err)
		return
	}
	a.finishUIAction(w, r)
}

func (a *App) handleUIStatus(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpError(w, err)
		return
	}
	status := r.FormValue("status")
	if !uiWritableStatuses[status] {
		httpError(w, badRequest("UI status changes may use backlog, queued, in_progress, in_review, or done; use close for closed"))
		return
	}
	if err := a.changeStoryStatus(r.Context(), r.PathValue("id"), status, false, "Updated from board UI"); err != nil {
		httpError(w, err)
		return
	}
	a.finishUIAction(w, r)
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
	a.finishUIAction(w, r)
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
	a.finishUIAction(w, r)
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
		a.finishUIAction(w, r)
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
	a.finishUIAction(w, r)
}

func (a *App) handleUIQueueReorder(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpError(w, err)
		return
	}
	projectID := r.PathValue("id")
	story, err := a.getStory(r.Context(), r.FormValue("storyId"))
	if err != nil {
		httpError(w, err)
		return
	}
	if story.ProjectID != projectID || story.Status != StatusQueued || story.QueuePosition == nil {
		httpError(w, badRequest("story is not in this project's queue"))
		return
	}
	direction := r.FormValue("direction")
	comparison, ordering := "<", "DESC"
	if direction == "down" {
		comparison, ordering = ">", "ASC"
	} else if direction != "up" {
		httpError(w, badRequest("direction must be up or down"))
		return
	}
	var otherID string
	var otherPosition int
	err = a.db.QueryRowContext(r.Context(), `SELECT id, queue_position FROM stories WHERE project_id = ? AND status = ? AND queue_position `+comparison+` ? ORDER BY queue_position `+ordering+` LIMIT 1`,
		projectID, StatusQueued, *story.QueuePosition).Scan(&otherID, &otherPosition)
	if errors.Is(err, sql.ErrNoRows) {
		a.finishUIAction(w, r)
		return
	}
	if err != nil {
		httpError(w, err)
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		httpError(w, err)
		return
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(r.Context(), `UPDATE stories SET queue_position = ? WHERE id = ?`, otherPosition, story.ID); err == nil {
		_, err = tx.ExecContext(r.Context(), `UPDATE stories SET queue_position = ? WHERE id = ?`, *story.QueuePosition, otherID)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		httpError(w, err)
		return
	}
	a.publishAgentEvent("board")
	a.finishUIAction(w, r)
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
	a.finishUIAction(w, r)
}

func (a *App) handleUIProjectSettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpError(w, err)
		return
	}
	id := r.PathValue("id")
	if _, ok := r.Form["workingDirectory"]; ok {
		if err := a.updateProjectWorkingDirectory(r.Context(), id, r.FormValue("workingDirectory")); err != nil {
			httpError(w, err)
			return
		}
	}
	if _, ok := r.Form["autonomyMode"]; ok {
		if err := a.updateProjectAutonomyMode(r.Context(), id, r.FormValue("autonomyMode")); err != nil {
			httpError(w, err)
			return
		}
	}
	a.finishUIAction(w, r)
}

func (a *App) handleProjectSetupStatus(w http.ResponseWriter, r *http.Request) {
	status, err := a.projectSetupStatus(r.Context(), r.PathValue("id"))
	if err != nil {
		httpError(w, err)
		return
	}
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "application/json") && !strings.Contains(accept, "text/html") {
		writeJSON(w, http.StatusOK, status)
		return
	}
	a.render(w, "setup_status.html", status)
}

func (a *App) handleUIUseGitRoot(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpError(w, err)
		return
	}
	if _, err := a.useGitRootForProject(r.Context(), r.PathValue("id")); err != nil {
		httpError(w, err)
		return
	}
	a.finishUIAction(w, r)
}

func (a *App) handleUICloneRepo(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpError(w, err)
		return
	}
	if _, err := a.cloneGitHubRepo(r.Context(), r.PathValue("id"), r.FormValue("repoUrl"), r.FormValue("parentDirectory")); err != nil {
		httpError(w, err)
		return
	}
	a.finishUIAction(w, r)
}

func (a *App) handleUICreateProject(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpError(w, err)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	prefix := strings.TrimSpace(r.FormValue("prefix"))
	workingDirectory := strings.TrimSpace(r.FormValue("workingDirectory"))
	autonomyMode := r.FormValue("autonomyMode")
	project, err := a.createProject(r.Context(), "", name, prefix, workingDirectory, autonomyMode)
	if err != nil {
		httpError(w, err)
		return
	}
	http.Redirect(w, r, "/projects/"+project.ID+"/backlog", http.StatusSeeOther)
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
	a.finishUIAction(w, r)
}

func (a *App) handleUIStopAgent(w http.ResponseWriter, r *http.Request) {
	if err := a.stopAgentQueue(); err != nil {
		httpError(w, err)
		return
	}
	a.finishUIAction(w, r)
}

func (a *App) handleAPIRoot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    "Ripple API",
		"docs":    "/api/docs",
		"openapi": "/api/openapi.yaml",
		"rules": map[string]any{
			"intendedStatusFlow":  []string{StatusBacklog, StatusQueued, StatusInProgress, StatusInReview, StatusDone},
			"botWritableStatuses": []string{StatusBacklog, StatusInProgress, StatusDone},
			"closed":              "Closed is manual-only. Bots should move finished work to done.",
			"in_review":           "in_review is orchestrator/human-only (supervised delivery). Bots cannot set it.",
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
			AutonomyMode     string `json:"autonomyMode"`
		}
		if err := decodeJSON(r, &req); err != nil {
			httpError(w, err)
			return
		}
		project, err := a.createProject(r.Context(), req.ID, req.Name, req.Prefix, req.WorkingDirectory, req.AutonomyMode)
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
		httpError(w, badRequest("bots may only set status to backlog, in_progress, or done; queued, in_review, and closed are not bot-writable"))
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

func (a *App) createProject(ctx context.Context, id, name, prefix, workingDirectory, autonomyMode string) (Project, error) {
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
	autonomyMode = normalizeAutonomyMode(autonomyMode)
	now := time.Now().UTC()
	_, err = a.db.ExecContext(ctx, `INSERT INTO projects (id, name, prefix, working_directory, autonomy_mode, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, name, prefix, workingDirectory, autonomyMode, formatTime(now), formatTime(now))
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
		return a.createProject(ctx, id, name, prefix, workingDirectory, "")
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
	return a.createProject(ctx, generatedID, name, prefix, workingDirectory, "")
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

func (a *App) updateProjectAutonomyMode(ctx context.Context, id, autonomyMode string) error {
	if _, err := a.getProject(ctx, id); err != nil {
		return err
	}
	autonomyMode = normalizeAutonomyMode(autonomyMode)
	_, err := a.db.ExecContext(ctx, `UPDATE projects SET autonomy_mode = ?, updated_at = ? WHERE id = ?`,
		autonomyMode, formatTime(time.Now().UTC()), id)
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
	} else if isGitWorkTree(abs) {
		if root, rootErr := detectGitRoot(context.Background(), abs); rootErr == nil {
			rootAbs, _ := filepath.Abs(root)
			if rootAbs != "" && rootAbs != abs {
				data.SuggestedGitRoot = rootAbs
				data.GitRootDiffers = true
			}
		}
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
	if status == StatusQueued {
		_, err = a.db.ExecContext(ctx, `UPDATE stories SET status = ?, updated_at = ?, closed_at = ?,
			queue_position = COALESCE(queue_position, (SELECT COALESCE(MAX(queue_position), 0) + 1 FROM stories WHERE project_id = ? AND status = ?))
			WHERE id = ?`, status, formatTime(now), closedAt, current.ProjectID, StatusQueued, id)
	} else {
		_, err = a.db.ExecContext(ctx, `UPDATE stories SET status = ?, updated_at = ?, closed_at = ?, queue_position = NULL WHERE id = ?`,
			status, formatTime(now), closedAt, id)
	}
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

func (a *App) createQueueRun(ctx context.Context, filters storyFilters, stories []Story) (int64, error) {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO queue_runs (status, project_id, epic_id, total, completed, message, started_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"running", filters.ProjectID, filters.EpicID, len(stories), 0, "Starting queued run", formatTime(time.Now().UTC()))
	if err != nil {
		return 0, err
	}
	runID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for i, story := range stories {
		if _, err := tx.ExecContext(ctx, `INSERT INTO queue_run_items (queue_run_id, story_id, position, status) VALUES (?, ?, ?, 'pending')`, runID, story.ID, i+1); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return runID, nil
}

func (a *App) listQueueRunItems(ctx context.Context, queueRunID int64) ([]QueueRunItem, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT qi.id, qi.queue_run_id, qi.position, qi.status,
		s.id, s.project_id, p.name, p.prefix, s.epic_id, e.name, s.title, s.description, s.status, s.close_comment, s.created_at, s.updated_at, s.closed_at, s.queue_position
		FROM queue_run_items qi
		JOIN stories s ON s.id = qi.story_id
		JOIN projects p ON p.id = s.project_id
		LEFT JOIN epics e ON e.id = s.epic_id
		WHERE qi.queue_run_id = ? ORDER BY qi.position`, queueRunID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []QueueRunItem{}
	for rows.Next() {
		var item QueueRunItem
		var epicID, epicName, closedAt sql.NullString
		var queuePosition sql.NullInt64
		var created, updated string
		if err := rows.Scan(&item.ID, &item.QueueRunID, &item.Position, &item.Status,
			&item.Story.ID, &item.Story.ProjectID, &item.Story.ProjectName, &item.Story.ProjectPrefix,
			&epicID, &epicName, &item.Story.Title, &item.Story.Description, &item.Story.Status,
			&item.Story.CloseComment, &created, &updated, &closedAt, &queuePosition); err != nil {
			return nil, err
		}
		if epicID.Valid {
			item.Story.EpicID = &epicID.String
		}
		if epicName.Valid {
			item.Story.EpicName = &epicName.String
		}
		item.Story.CreatedAt = parseTime(created)
		item.Story.UpdatedAt = parseTime(updated)
		if closedAt.Valid {
			t := parseTime(closedAt.String)
			item.Story.ClosedAt = &t
		}
		if queuePosition.Valid {
			p := int(queuePosition.Int64)
			item.Story.QueuePosition = &p
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (a *App) updateQueueRunItemStatus(ctx context.Context, queueRunID int64, storyID, status string) error {
	_, err := a.db.ExecContext(ctx, `UPDATE queue_run_items SET status = ? WHERE queue_run_id = ? AND story_id = ?`, status, queueRunID, storyID)
	a.publishAgentEvent("activity")
	return err
}

func (a *App) skipPendingQueueRunItems(ctx context.Context, queueRunID int64) {
	_, _ = a.db.ExecContext(ctx, `UPDATE queue_run_items SET status = 'skipped' WHERE queue_run_id = ? AND status = 'pending'`, queueRunID)
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

func (a *App) listQueueRuns(ctx context.Context, projectID string) ([]QueueRunSummary, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT id, status, project_id, epic_id, total, completed, message, error, started_at, finished_at
		FROM queue_runs WHERE project_id = ? ORDER BY id DESC LIMIT 50`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []QueueRunSummary{}
	for rows.Next() {
		run, err := scanQueueRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (a *App) getQueueRun(ctx context.Context, id int64) (QueueRunSummary, error) {
	return scanQueueRun(a.db.QueryRowContext(ctx, `SELECT id, status, project_id, epic_id, total, completed, message, error, started_at, finished_at FROM queue_runs WHERE id = ?`, id))
}

func (a *App) listAgentStoryRuns(ctx context.Context, queueRunID int64) ([]AgentRunSummary, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT ar.id, ar.queue_run_id, ar.story_id, s.title, ar.run_kind, ar.status, ar.working_directory,
		COALESCE(NULLIF(ar.branch, ''), sp.branch, ''),
		CASE WHEN ar.pr_number > 0 THEN ar.pr_number ELSE COALESCE(sp.pr_number, 0) END,
		COALESCE(NULLIF(ar.pr_url, ''), sp.pr_url, ''),
		ar.stdout, ar.stderr, ar.final_message, ar.exit_error, ar.started_at, ar.finished_at
		FROM agent_runs ar
		LEFT JOIN stories s ON s.id = ar.story_id
		LEFT JOIN story_pipelines sp ON sp.queue_run_id = ar.queue_run_id AND sp.story_id = ar.story_id
		WHERE ar.queue_run_id = ?
		ORDER BY ar.id ASC`, queueRunID)
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
	if strings.TrimSpace(filters.ProjectID) == "" {
		return badRequest("project-scoped runs require a project")
	}
	a.agentMu.Lock()
	if a.agentStatus.Running {
		a.agentMu.Unlock()
		return badRequest("agent queue is already running")
	}
	a.agentMu.Unlock()
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
	if _, err := a.resolveImplementer(ctx); err != nil {
		return err
	}
	if _, err := a.resolveReviewer(ctx); err != nil {
		return err
	}
	if _, err := resolveGhBinary(); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	a.agentMu.Lock()
	if a.agentStatus.Running {
		a.agentMu.Unlock()
		cancel()
		return badRequest("agent queue is already running")
	}
	queueRunID, err := a.createQueueRun(ctx, filters, queued)
	if err != nil {
		a.agentMu.Unlock()
		cancel()
		return err
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

	go a.runAgentQueue(runCtx, queueRunID, baseURL, len(queued))
	return nil
}

func (a *App) runAgentQueue(ctx context.Context, queueRunID int64, baseURL string, total int) {
	previousSummary := ""
	completed := 0
	items, err := a.listQueueRunItems(context.Background(), queueRunID)
	if err != nil {
		a.finishAgentRun(context.Background(), queueRunID, "failed", "Queue run failed", completed, err)
		return
	}
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			a.skipPendingQueueRunItems(context.Background(), queueRunID)
			a.finishAgentRun(context.Background(), queueRunID, "stopped", "Queue run stopped", completed, err)
			return
		}
		story := item.Story
		_ = a.updateQueueRunItemStatus(context.Background(), queueRunID, story.ID, "running")
		project, err := a.getProject(ctx, story.ProjectID)
		if err != nil {
			_ = a.updateQueueRunItemStatus(context.Background(), queueRunID, story.ID, "failed")
			a.skipPendingQueueRunItems(context.Background(), queueRunID)
			a.finishAgentRun(context.Background(), queueRunID, "failed", "Queue run failed", completed, err)
			return
		}
		if strings.TrimSpace(project.WorkingDirectory) == "" {
			_ = a.updateQueueRunItemStatus(context.Background(), queueRunID, story.ID, "failed")
			a.skipPendingQueueRunItems(context.Background(), queueRunID)
			a.finishAgentRun(context.Background(), queueRunID, "failed", "Queue run paused", completed, fmt.Errorf("%s needs a project path", project.Name))
			return
		}
		a.updateAgentProgress(story.ID, fmt.Sprintf("Running %s", story.ID), completed, total)
		_ = a.updateQueueRun(context.Background(), queueRunID, "running", fmt.Sprintf("Running %s", story.ID), completed, nil)
		if err := a.changeStoryStatus(ctx, story.ID, StatusInProgress, false, "Started by agent runner"); err != nil {
			_ = a.updateQueueRunItemStatus(context.Background(), queueRunID, story.ID, "failed")
			a.skipPendingQueueRunItems(context.Background(), queueRunID)
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
		result, err := a.runStoryPipeline(ctx, pc, previousSummary)
		if err != nil {
			_ = a.addEvent(ctx, story.ID, "agent_failed", "Story pipeline failed: "+err.Error())
			status := QueueItemFailed
			message := "Queue run failed"
			if errors.Is(err, context.Canceled) {
				status = QueueItemStopped
				message = "Queue run stopped"
			}
			_ = a.updateQueueRunItemStatus(context.Background(), queueRunID, story.ID, status)
			a.skipPendingQueueRunItems(context.Background(), queueRunID)
			a.finishAgentRun(context.Background(), queueRunID, status, message, completed, fmt.Errorf("%s failed: %w", story.ID, err))
			return
		}
		completed, previousSummary, err = a.applyStoryPipelineOutcome(context.Background(), queueRunID, story, result, completed, total, previousSummary)
		if err != nil {
			_ = a.updateQueueRunItemStatus(context.Background(), queueRunID, story.ID, QueueItemFailed)
			a.skipPendingQueueRunItems(context.Background(), queueRunID)
			_ = a.addEvent(ctx, story.ID, "agent_needs_review", "Pipeline finished, but the app could not finalize the story status")
			a.finishAgentRun(context.Background(), queueRunID, "needs_review", "Queue run needs review", completed, fmt.Errorf("%s was not finalized: %w", story.ID, err))
			return
		}
		a.publishAgentEvent("board")
	}
	a.finishAgentRun(context.Background(), queueRunID, "completed", fmt.Sprintf("Queue run complete: %d/%d stories", completed, total), completed, nil)
}

// applyStoryPipelineOutcome records the post-pipeline story outcome and advances the queue item.
// Supervised pauses leave the story in_review and mark the queue item awaiting_human so later stories can run.
func (a *App) applyStoryPipelineOutcome(ctx context.Context, queueRunID int64, story Story, result pipelineResult, completed, total int, previousSummary string) (int, string, error) {
	if result.AwaitingHuman {
		completed++
		if err := a.updateQueueRunItemStatus(ctx, queueRunID, story.ID, QueueItemAwaitingHuman); err != nil {
			return completed, previousSummary, err
		}
		previousSummary = summarizeFinalMessage(story, result.FinalMessage)
		a.updateAgentProgress("", fmt.Sprintf("Waiting on human for %s", story.ID), completed, total)
		_ = a.updateQueueRun(ctx, queueRunID, "running", fmt.Sprintf("Waiting on human for %s", story.ID), completed, nil)
		return completed, previousSummary, nil
	}

	if err := a.changeStoryStatus(ctx, story.ID, StatusDone, false, "Marked done after PR merged"); err != nil {
		return completed, previousSummary, err
	}
	completed++
	if err := a.updateQueueRunItemStatus(ctx, queueRunID, story.ID, QueueItemCompleted); err != nil {
		return completed, previousSummary, err
	}
	previousSummary = summarizeFinalMessage(story, result.FinalMessage)
	_ = a.addEvent(ctx, story.ID, "agent_completed", "Story pipeline completed and PR merged")
	a.updateAgentProgress("", fmt.Sprintf("Completed %s", story.ID), completed, total)
	_ = a.updateQueueRun(ctx, queueRunID, "running", fmt.Sprintf("Completed %s", story.ID), completed, nil)
	return completed, previousSummary, nil
}

func (a *App) runCodexForStoryWithKind(ctx context.Context, queueRunID int64, baseURL string, project Project, story Story, prompt, runKind, branch string, prNumber int, prURL string) (string, error) {
	runDir := filepath.Join(os.TempDir(), "ripple", "runs")
	if err := os.MkdirAll(runDir, 0755); err != nil {
		return "", err
	}
	finalPath := filepath.Join(runDir, story.ID+"-"+runKind+"-final.md")
	runner, err := a.newImplementerRunner(context.Background())
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
	role := AgentRunRoleImplement
	if runKind == RunKindCodexFix || runKind == RunKindCodexAddressFeedback {
		role = AgentRunRoleFix
	}
	result, err := runner.Run(ctx, AgentRunRequest{
		Role:             role,
		Prompt:           prompt,
		WorkingDir:       project.WorkingDirectory,
		BaseURL:          baseURL,
		StoryID:          story.ID,
		FinalMessagePath: finalPath,
		Stdout:           output.stdoutWriter(),
		Stderr:           output.stderrWriter(),
	})
	output.flushNow()
	stdoutText, stderrText := result.Stdout, result.Stderr
	if stdoutText == "" && stderrText == "" {
		stdoutText, stderrText = output.snapshot()
	}
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
	finalMessage := strings.TrimSpace(result.FinalMessage)
	if finalMessage == "" {
		finalMessage = strings.TrimSpace(stdoutText)
	}
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

// finishManualAgentWork clears the global agent slot after a non-queue action (e.g. address-feedback).
func (a *App) finishManualAgentWork(queueRunID int64, message string, err error) {
	a.agentMu.Lock()
	a.agentStatus.Running = false
	a.agentStatus.QueueRunID = queueRunID
	a.agentStatus.CurrentStoryID = ""
	a.agentStatus.Message = message
	a.agentStatus.FinishedAt = time.Now().UTC()
	if err != nil && !errors.Is(err, context.Canceled) {
		a.agentStatus.LastError = truncate(err.Error(), 180)
	} else {
		a.agentStatus.LastError = ""
	}
	a.agentCancel = nil
	a.agentMu.Unlock()
	a.publishAgentEvent("activity")
	a.publishAgentEvent("board")
}

func (a *App) startAddressFeedback(storyID, baseURL string) error {
	story, project, pipeline, feedback, err := a.prepareAddressFeedback(context.Background(), storyID)
	if err != nil {
		return err
	}
	if _, err := a.resolveImplementer(context.Background()); err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(context.Background())
	a.agentMu.Lock()
	if a.agentStatus.Running {
		a.agentMu.Unlock()
		cancel()
		return badRequest("An agent is already running. Wait for it to finish or stop it first.")
	}
	a.agentStatus = AgentStatus{
		Running:        true,
		QueueRunID:     pipeline.QueueRunID,
		CurrentStoryID: story.ID,
		Message:        "Addressing review comments",
		StartedAt:      time.Now().UTC(),
		Total:          1,
	}
	a.agentCancel = cancel
	a.agentMu.Unlock()
	a.publishAgentEvent("activity")
	a.publishAgentEvent("board")

	go func() {
		runErr := a.runAddressFeedback(runCtx, baseURL, story, project, pipeline, feedback)
		message := fmt.Sprintf("Finished addressing feedback for %s", story.ID)
		if runErr != nil {
			if errors.Is(runErr, context.Canceled) {
				message = "Address feedback stopped"
			} else {
				message = "Address feedback failed"
			}
		}
		a.finishManualAgentWork(pipeline.QueueRunID, message, runErr)
	}()
	return nil
}

// startHumanMerge merges a supervised story's PR under the global agent lock (sync so gate failures surface to the UI).
func (a *App) startHumanMerge(storyID string) error {
	story, project, pipeline, err := a.prepareHumanMerge(context.Background(), storyID)
	if err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.agentMu.Lock()
	if a.agentStatus.Running {
		a.agentMu.Unlock()
		return badRequest("An agent is already running. Wait for it to finish or stop it first.")
	}
	a.agentStatus = AgentStatus{
		Running:        true,
		QueueRunID:     pipeline.QueueRunID,
		CurrentStoryID: story.ID,
		Message:        "Merging pull request",
		StartedAt:      time.Now().UTC(),
		Total:          1,
	}
	a.agentCancel = cancel
	a.agentMu.Unlock()
	a.publishAgentEvent("activity")
	a.publishAgentEvent("board")

	runErr := a.executeHumanMerge(runCtx, story, project, pipeline)
	message := fmt.Sprintf("Merged %s", story.ID)
	if runErr != nil {
		if errors.Is(runErr, context.Canceled) {
			message = "Merge stopped"
		} else {
			message = "Merge failed"
		}
	}
	a.finishManualAgentWork(pipeline.QueueRunID, message, runErr)
	return runErr
}

func (a *App) stopAgentQueue() error {
	a.agentMu.Lock()
	cancel := a.agentCancel
	running := a.agentStatus.Running
	if running {
		a.agentStatus.Message = "Stopping agent"
	}
	a.agentMu.Unlock()
	a.publishAgentEvent("activity")
	if !running || cancel == nil {
		return badRequest("agent is not running")
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
	columns := []string{StatusQueued, StatusBacklog, StatusInProgress, StatusInReview, StatusDone}
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
		panel, err := a.storyPanelData(ctx, storyID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return data, nil
			}
			return BoardData{}, err
		}
		data.HasDetailStory = true
		data.Detail = panel
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
		data.Counts.InReview += counts.InReview
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
		case StatusInReview:
			counts.InReview++
		case StatusDone:
			counts.Done++
		case StatusClosed:
			counts.Closed++
		}
	}
	return counts
}

func (a *App) listProjects(ctx context.Context) ([]Project, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT id, name, prefix, working_directory, autonomy_mode, next_story_number, created_at, updated_at FROM projects ORDER BY name`)
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
	row := a.db.QueryRowContext(ctx, `SELECT id, name, prefix, working_directory, autonomy_mode, next_story_number, created_at, updated_at FROM projects WHERE id = ?`, id)
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
	query := `SELECT s.id, s.project_id, p.name, p.prefix, s.epic_id, e.name, s.title, s.description, s.status, s.close_comment, s.created_at, s.updated_at, s.closed_at, s.queue_position
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
	query += ` ORDER BY CASE WHEN s.queue_position IS NULL THEN 1 ELSE 0 END, s.queue_position ASC, s.created_at ASC, s.id ASC`
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
	row := a.db.QueryRowContext(ctx, `SELECT s.id, s.project_id, p.name, p.prefix, s.epic_id, e.name, s.title, s.description, s.status, s.close_comment, s.created_at, s.updated_at, s.closed_at, s.queue_position
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
	if err := row.Scan(&p.ID, &p.Name, &p.Prefix, &p.WorkingDirectory, &p.AutonomyMode, &p.NextStoryNumber, &created, &updated); err != nil {
		return Project{}, err
	}
	p.AutonomyMode = normalizeAutonomyMode(p.AutonomyMode)
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
	var queuePosition sql.NullInt64
	var created, updated string
	if err := row.Scan(&s.ID, &s.ProjectID, &s.ProjectName, &s.ProjectPrefix, &epicID, &epicName, &s.Title, &s.Description, &s.Status, &s.CloseComment, &created, &updated, &closedAt, &queuePosition); err != nil {
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
	if queuePosition.Valid {
		position := int(queuePosition.Int64)
		s.QueuePosition = &position
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
		return "Reviewer"
	case RunKindCodexFix:
		return "Implementer fix"
	case RunKindCodexAddressFeedback:
		return "Address review comments"
	default:
		return "Implementer"
	}
}

func runKindSource(kind string) string {
	if kind == RunKindGrokReview {
		return "reviewer"
	}
	return "implementer"
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
						kind = "warning"
						if run.Status != "completed" {
							kind = "error"
						}
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
	if stderr := displayAgentStderr(run.Stderr); stderr != "" {
		kind := "warning"
		if run.Status != "completed" {
			kind = "error"
		}
		add(kind, stderr)
	}
	if strings.TrimSpace(run.FinalMessage) != "" {
		add("summary", run.FinalMessage)
	}
	if len(items) > 24 {
		items = items[len(items)-24:]
	}
	return items
}

func displayAgentStderr(stderr string) string {
	lines := strings.Split(stderr, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.TrimSpace(line) == "Reading additional input from stdin..." {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
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
	case StatusInReview:
		return "In Review"
	case StatusDone:
		return "Done"
	case StatusClosed:
		return "Closed"
	default:
		return status
	}
}

// eventTitle returns human-readable labels for story history event types.
func eventTitle(eventType string) string {
	switch eventType {
	case eventAwaitingHumanReview:
		return "Awaiting your review"
	case eventAddressingFeedback:
		return "Addressing feedback"
	case eventFeedbackAddressed:
		return "Feedback addressed"
	case eventFeedbackNoChanges:
		return "No code changes from feedback"
	case eventMergedByHuman:
		return "Merged by you"
	case eventMergedExternally:
		return "Synced external merge"
	case eventQualityGateFailed:
		return "Quality gate failed"
	case "merge_failed":
		return "Merge failed"
	case "agent_completed":
		return "Completed"
	case "agent_failed":
		return "Failed"
	case "agent_needs_review":
		return "Needs review"
	case "status_changed":
		return "Status changed"
	case "story_closed":
		return "Closed"
	case "story_created":
		return "Created"
	default:
		return strings.ReplaceAll(eventType, "_", " ")
	}
}

// queueItemStatusTitle is the run-sidebar label for a queue item outcome.
func queueItemStatusTitle(status string) string {
	switch status {
	case QueueItemAwaitingHuman:
		return "waiting on you"
	case QueueItemCompleted:
		return "merged"
	case QueueItemRunning:
		return "running"
	case QueueItemFailed:
		return "failed"
	case QueueItemStopped:
		return "stopped"
	case QueueItemSkipped:
		return "skipped"
	case "pending":
		return "pending"
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

func normalizeAutonomyMode(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case AutonomySupervised:
		return AutonomySupervised
	default:
		return AutonomyAutonomous
	}
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

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func defaultRippleDBPath() string {
	if configured := firstEnv("RIPPLE_DB", "TASKMANAGER_DB"); configured != "" {
		return configured
	}
	if _, err := os.Stat("ripple.db"); err == nil {
		return "ripple.db"
	}
	if _, err := os.Stat("taskmanager.db"); err == nil {
		return "taskmanager.db"
	}
	return "ripple.db"
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
