package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func testApp(t *testing.T) *App {
	t.Helper()
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "-")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app, err := NewApp(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return app
}

func seedProjectStories(t *testing.T, app *App, projectID string, count int) []Story {
	t.Helper()
	if _, err := app.createProject(context.Background(), projectID, strings.ToUpper(projectID), strings.ToUpper(projectID[:1]), "/tmp"); err != nil {
		t.Fatal(err)
	}
	stories := make([]Story, 0, count)
	for i := 1; i <= count; i++ {
		story, err := app.createStory(context.Background(), createStoryRequest{ProjectID: projectID, Title: fmt.Sprintf("Story %d", i), Description: "Test story description"})
		if err != nil {
			t.Fatal(err)
		}
		stories = append(stories, story)
	}
	return stories
}

func TestThreePageRoutes(t *testing.T) {
	app := testApp(t)
	seedProjectStories(t, app, "atlas", 2)
	tests := []struct{ path, marker string }{
		{"/", "Dashboard"},
		{"/about", "About Ripple"},
		{"/settings", "Application preferences"},
		{"/projects/atlas/backlog", "Project backlog"},
		{"/projects/atlas/run?new=1", "Run workspace"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			res := httptest.NewRecorder()
			app.routes().ServeHTTP(res, req)
			if res.Code != http.StatusOK {
				t.Fatalf("status = %d; body = %s", res.Code, res.Body.String())
			}
			if !strings.Contains(res.Body.String(), tc.marker) {
				t.Fatalf("response missing %q", tc.marker)
			}
			if !strings.Contains(res.Body.String(), "app-rail") || !strings.Contains(res.Body.String(), "data-tooltip=\"Dashboard\"") {
				t.Fatalf("response missing icon navigation rail")
			}
		})
	}
}

func TestSettingsOwnsThemeControlsAndUtilityNavigation(t *testing.T) {
	app := testApp(t)
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("settings status = %d", res.Code)
	}
	body := res.Body.String()
	for _, marker := range []string{`data-theme-choice="light"`, `data-theme-choice="dark"`, `data-theme-choice="system"`, `href="/about"`, `href="/api/docs"`} {
		if !strings.Contains(body, marker) {
			t.Fatalf("settings page missing %q", marker)
		}
	}
	if strings.Contains(body, "data-theme-toggle") {
		t.Fatalf("theme toggle should no longer live in the navigation rail")
	}
}

func TestRippleBranding(t *testing.T) {
	app := testApp(t)
	for _, tc := range []struct {
		path   string
		marker string
	}{
		{"/", `aria-label="Ripple"`},
		{"/about", "About Ripple"},
		{"/api", `"name":"Ripple API"`},
		{"/api/docs", "# Ripple Bot API"},
		{"/api/openapi.yaml", "title: Ripple API"},
	} {
		res := httptest.NewRecorder()
		app.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), tc.marker) {
			t.Fatalf("%s missing Ripple brand marker %q: %d %s", tc.path, tc.marker, res.Code, res.Body.String())
		}
		if (tc.path == "/" || tc.path == "/about") && !strings.Contains(res.Body.String(), `/static/Ripple.png`) {
			t.Fatalf("%s missing Ripple image branding", tc.path)
		}
	}

	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/static/Ripple.png", nil))
	if res.Code != http.StatusOK || res.Body.Len() == 0 || !strings.HasPrefix(res.Header().Get("Content-Type"), "image/png") {
		t.Fatalf("Ripple image response = %d, type %q, bytes %d", res.Code, res.Header().Get("Content-Type"), res.Body.Len())
	}
}

func TestAboutPageExplainsAgentSetupAndDiscovery(t *testing.T) {
	app := testApp(t)
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/about", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("about status = %d", res.Code)
	}
	body := res.Body.String()
	for _, marker := range []string{"Codex CLI", "Grok CLI", "GitHub CLI", "GET /api", "/api/openapi.yaml", "RIPPLE_CODEX_BIN", "https://github.com/adamaoc", "data-agent-prompt"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("about page missing %q", marker)
		}
	}
}

func TestQueueOrderingAndProjectIsolation(t *testing.T) {
	app := testApp(t)
	atlas := seedProjectStories(t, app, "atlas", 3)
	orbit := seedProjectStories(t, app, "orbit", 1)
	for _, story := range append(atlas, orbit...) {
		if err := app.changeStoryStatus(context.Background(), story.ID, StatusQueued, false, "test"); err != nil {
			t.Fatal(err)
		}
	}
	queued, err := app.listStories(context.Background(), storyFilters{ProjectID: "atlas", Status: StatusQueued, ShowClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{queued[0].ID, queued[1].ID, queued[2].ID}; strings.Join(got, ",") != strings.Join([]string{atlas[0].ID, atlas[1].ID, atlas[2].ID}, ",") {
		t.Fatalf("initial order = %v", got)
	}

	form := url.Values{"storyId": {atlas[2].ID}, "direction": {"up"}, "redirect": {"/projects/atlas/backlog"}}
	req := httptest.NewRequest(http.MethodPost, "/projects/atlas/queue/reorder", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, req)
	if res.Code != http.StatusSeeOther {
		t.Fatalf("reorder status = %d; body = %s", res.Code, res.Body.String())
	}
	queued, err = app.listStories(context.Background(), storyFilters{ProjectID: "atlas", Status: StatusQueued, ShowClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	if queued[1].ID != atlas[2].ID || queued[2].ID != atlas[1].ID {
		t.Fatalf("reordered queue = %s, %s, %s", queued[0].ID, queued[1].ID, queued[2].ID)
	}
	orbitQueued, err := app.listStories(context.Background(), storyFilters{ProjectID: "orbit", Status: StatusQueued, ShowClosed: true})
	if err != nil || len(orbitQueued) != 1 || orbitQueued[0].ID != orbit[0].ID {
		t.Fatalf("other project queue changed: %#v, %v", orbitQueued, err)
	}
}

func TestQueueRunSnapshotDoesNotAcceptLaterStories(t *testing.T) {
	app := testApp(t)
	stories := seedProjectStories(t, app, "atlas", 3)
	for _, story := range stories[:2] {
		if err := app.changeStoryStatus(context.Background(), story.ID, StatusQueued, false, "test"); err != nil {
			t.Fatal(err)
		}
	}
	queued, err := app.listStories(context.Background(), storyFilters{ProjectID: "atlas", Status: StatusQueued, ShowClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	runID, err := app.createQueueRun(context.Background(), storyFilters{ProjectID: "atlas", Status: StatusQueued, ShowClosed: true}, queued)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.changeStoryStatus(context.Background(), stories[2].ID, StatusQueued, false, "queued later"); err != nil {
		t.Fatal(err)
	}
	items, err := app.listQueueRunItems(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Story.ID != stories[0].ID || items[1].Story.ID != stories[1].ID {
		t.Fatalf("snapshot items = %#v", items)
	}
}

func TestQueueRunItemTerminalStatuses(t *testing.T) {
	app := testApp(t)
	stories := seedProjectStories(t, app, "atlas", 3)
	for _, story := range stories {
		if err := app.changeStoryStatus(context.Background(), story.ID, StatusQueued, false, "test"); err != nil {
			t.Fatal(err)
		}
	}
	queued, _ := app.listStories(context.Background(), storyFilters{ProjectID: "atlas", Status: StatusQueued, ShowClosed: true})
	runID, err := app.createQueueRun(context.Background(), storyFilters{ProjectID: "atlas"}, queued)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.updateQueueRunItemStatus(context.Background(), runID, stories[0].ID, "completed"); err != nil {
		t.Fatal(err)
	}
	if err := app.updateQueueRunItemStatus(context.Background(), runID, stories[1].ID, "stopped"); err != nil {
		t.Fatal(err)
	}
	app.skipPendingQueueRunItems(context.Background(), runID)
	items, err := app.listQueueRunItems(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{items[0].Status, items[1].Status, items[2].Status}
	if strings.Join(got, ",") != "completed,stopped,skipped" {
		t.Fatalf("statuses = %v", got)
	}
}

func TestRunHistoryAndUnknownProject(t *testing.T) {
	app := testApp(t)
	stories := seedProjectStories(t, app, "atlas", 1)
	if err := app.changeStoryStatus(context.Background(), stories[0].ID, StatusQueued, false, "test"); err != nil {
		t.Fatal(err)
	}
	queued, _ := app.listStories(context.Background(), storyFilters{ProjectID: "atlas", Status: StatusQueued, ShowClosed: true})
	runID, err := app.createQueueRun(context.Background(), storyFilters{ProjectID: "atlas"}, queued)
	if err != nil {
		t.Fatal(err)
	}

	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/projects/atlas/runs/%d", runID), nil))
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), fmt.Sprintf("Run #%d", runID)) {
		t.Fatalf("history response = %d %s", res.Code, res.Body.String())
	}
	res = httptest.NewRecorder()
	app.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/projects/missing/backlog", nil))
	if res.Code != http.StatusNotFound {
		t.Fatalf("unknown project status = %d", res.Code)
	}
}

func TestRunTranscriptIsChronologicalAndKeepsRawOutput(t *testing.T) {
	app := testApp(t)
	stories := seedProjectStories(t, app, "atlas", 2)
	for _, story := range stories {
		if err := app.changeStoryStatus(context.Background(), story.ID, StatusQueued, false, "test"); err != nil {
			t.Fatal(err)
		}
	}
	queued, _ := app.listStories(context.Background(), storyFilters{ProjectID: "atlas", Status: StatusQueued, ShowClosed: true})
	runID, err := app.createQueueRun(context.Background(), storyFilters{ProjectID: "atlas"}, queued)
	if err != nil {
		t.Fatal(err)
	}
	project, _ := app.getProject(context.Background(), "atlas")
	firstID, err := app.createAgentRun(context.Background(), runID, project, stories[0], "prompt", RunKindCodexImplement, "branch-one", 42, "https://example.com/pull/42")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.finishAgentStoryRun(context.Background(), firstID, "completed", "{\"type\":\"turn.started\"}", "", "First summary", nil); err != nil {
		t.Fatal(err)
	}
	secondID, err := app.createAgentRun(context.Background(), runID, project, stories[1], "prompt", RunKindCodexImplement, "branch-two", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.finishAgentStoryRun(context.Background(), secondID, "completed", "{\"type\":\"turn.completed\"}", "", "Second summary", nil); err != nil {
		t.Fatal(err)
	}
	if err := app.upsertStoryPipeline(context.Background(), StoryPipeline{
		QueueRunID: runID, StoryID: stories[1].ID, Phase: PipelinePhaseCompleted,
		Branch: "branch-two", PRNumber: 43, PRURL: "https://example.com/pull/43",
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.updateQueueRun(context.Background(), runID, "completed", "Queue run complete", 2, nil); err != nil {
		t.Fatal(err)
	}

	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/projects/atlas/runs/%d", runID), nil))
	body := res.Body.String()
	firstAt, secondAt := strings.Index(body, stories[0].ID), strings.Index(body, stories[1].ID)
	if firstAt < 0 || secondAt < 0 || firstAt >= secondAt {
		t.Fatalf("transcript order is not chronological")
	}
	if !strings.Contains(body, "Raw output") || !strings.Contains(body, "First summary") {
		t.Fatalf("transcript omitted raw output or final summary")
	}
	if !strings.Contains(body, "Work finished successfully") || !strings.Contains(body, "PR #42") || !strings.Contains(body, "https://example.com/pull/42") || !strings.Contains(body, "PR #43") || !strings.Contains(body, "https://example.com/pull/43") {
		t.Fatalf("run completion summary omitted outcome or pull request")
	}
	if !strings.Contains(body, `data-collapse-all`) || !strings.Contains(body, `data-run-section="agent-`) || !strings.Contains(body, `data-run-section="completion"`) {
		t.Fatalf("run transcript omitted collapsible section controls")
	}
}

func TestAgentLogItemsTreatRecoveredDiagnosticsAsWarnings(t *testing.T) {
	stdout := strings.Join([]string{
		`{"type":"item.completed","item":{"type":"command_execution","command":"missing-probe","exit_code":1,"aggregated_output":"not found"}}`,
		`{"type":"turn.completed"}`,
	}, "\n")
	run := AgentRunSummary{
		Status: "completed",
		Stdout: stdout,
		Stderr: "Reading additional input from stdin...\ninternal tool diagnostic",
	}
	items := agentLogItems(run)
	var warnings, errorsFound int
	for _, item := range items {
		if item.Kind == "warning" {
			warnings++
		}
		if item.Kind == "error" {
			errorsFound++
		}
		if strings.Contains(item.Text, "Reading additional input") {
			t.Fatalf("stdin status line should be filtered: %#v", items)
		}
	}
	if warnings != 2 || errorsFound != 0 {
		t.Fatalf("warnings = %d, errors = %d; items = %#v", warnings, errorsFound, items)
	}

	run.Status = "failed"
	items = agentLogItems(run)
	errorsFound = 0
	for _, item := range items {
		if item.Kind == "error" {
			errorsFound++
		}
	}
	if errorsFound != 2 {
		t.Fatalf("failed run should retain error severity: %#v", items)
	}
}

func TestBacklogFiltersAndStoryPanel(t *testing.T) {
	app := testApp(t)
	stories := seedProjectStories(t, app, "atlas", 2)
	if err := app.changeStoryStatus(context.Background(), stories[1].ID, StatusDone, false, "test"); err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/projects/atlas/backlog?status=done", nil))
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), stories[1].Title) || strings.Contains(res.Body.String(), stories[0].Title) {
		t.Fatalf("done filter response was incorrect")
	}
	if !strings.Contains(res.Body.String(), `aria-label="Filter by epic"`) || !strings.Contains(res.Body.String(), `onchange="this.form.submit()"`) || !strings.Contains(res.Body.String(), `>Epics</option>`) {
		t.Fatalf("epic filter should be unlabeled and submit immediately")
	}
	if !strings.Contains(res.Body.String(), "Choose folder") || !strings.Contains(res.Body.String(), `id="folder-picker"`) || !strings.Contains(res.Body.String(), "project-settings") {
		t.Fatalf("project settings omitted the folder picker or settings control")
	}
	res = httptest.NewRecorder()
	app.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/stories/"+stories[0].ID+"/panel", nil))
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "Save description") || !strings.Contains(res.Body.String(), "Close") {
		t.Fatalf("story panel response was incorrect")
	}
}

func TestFolderPickerCanSelectProjectWorkingDirectory(t *testing.T) {
	app := testApp(t)
	seedProjectStories(t, app, "atlas", 1)
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/folder-picker?projectId=atlas&path=/tmp", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("folder picker status = %d", res.Code)
	}
	body := res.Body.String()
	for _, marker := range []string{"Choose folder", "Use this folder", `name="workingDirectory" value="/tmp"`, `name="redirect" value="/projects/atlas/backlog"`} {
		if !strings.Contains(body, marker) {
			t.Fatalf("folder picker missing %q", marker)
		}
	}
}
