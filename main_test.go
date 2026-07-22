package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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
	if _, err := app.createProject(context.Background(), projectID, strings.ToUpper(projectID), strings.ToUpper(projectID[:1]), "/tmp", ""); err != nil {
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
	for _, marker := range []string{
		`data-theme-choice="light"`,
		`data-theme-choice="dark"`,
		`data-theme-choice="system"`,
		`href="/about"`,
		`href="/api/docs"`,
		`id="agents"`,
		`href="#agents"`,
		`id="api-providers"`,
		"Implementer",
		"Reviewer",
		"Codex binary path",
		"Grok binary path",
		"Tool status",
		"API providers",
		`action="/settings/agents"`,
		`action="/settings/agents/api-providers"`,
		`value="codex_cli"`,
		`value="grok_cli"`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("settings page missing %q", marker)
		}
	}
	// Both CLI providers must appear in both role dropdowns (not filtered to one each).
	if c := strings.Count(body, `value="codex_cli"`); c < 2 {
		t.Fatalf("codex_cli options = %d, want at least 2 (implementer + reviewer)", c)
	}
	if c := strings.Count(body, `value="grok_cli"`); c < 2 {
		t.Fatalf("grok_cli options = %d, want at least 2 (implementer + reviewer)", c)
	}
	if strings.Contains(body, "data-theme-toggle") {
		t.Fatalf("theme toggle should no longer live in the navigation rail")
	}
	if strings.Contains(body, "More settings will be added") || strings.Contains(body, "Coming later") {
		t.Fatalf("settings page should no longer show the placeholder future-settings block")
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
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "Save description") || !strings.Contains(res.Body.String(), "Close Story") {
		t.Fatalf("story panel response was incorrect")
	}
	for _, marker := range []string{`name="status"`, `this.form.elements.redirect.value=location.pathname+location.search`, `data-close-story-popup`, `name="closeComment"`, `required`, "Confirm close", "Cancel"} {
		if !strings.Contains(res.Body.String(), marker) {
			t.Fatalf("story panel missing %q", marker)
		}
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

func TestSupervisedPipelinePausesForHuman(t *testing.T) {
	app := testApp(t)
	project, err := app.createProject(context.Background(), "atlas", "Atlas", "A", "/tmp", AutonomySupervised)
	if err != nil {
		t.Fatal(err)
	}
	story, err := app.createStory(context.Background(), createStoryRequest{
		ProjectID: project.ID, Title: "Supervised story", Description: "desc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.changeStoryStatus(context.Background(), story.ID, StatusQueued, false, "queue"); err != nil {
		t.Fatal(err)
	}
	queued, err := app.listStories(context.Background(), storyFilters{ProjectID: project.ID, Status: StatusQueued, ShowClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	runID, err := app.createQueueRun(context.Background(), storyFilters{ProjectID: project.ID}, queued)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.changeStoryStatus(context.Background(), story.ID, StatusInProgress, false, "running"); err != nil {
		t.Fatal(err)
	}

	pc := pipelineContext{
		QueueRunID: runID,
		Project:    project,
		Story:      story,
		PRNumber:   42,
		PRURL:      "https://github.com/acme/atlas/pull/42",
	}
	pipeline := StoryPipeline{
		QueueRunID: runID,
		StoryID:    story.ID,
		Branch:     "ripple/A-001-supervised-story",
		PRNumber:   42,
		PRURL:      "https://github.com/acme/atlas/pull/42",
		ReviewJSON: `{"approved":false,"summary":"needs work","comments":[]}`,
	}
	result, err := app.pausePipelineForHuman(context.Background(), pc, pipeline, "implemented")
	if err != nil {
		t.Fatal(err)
	}
	if !result.AwaitingHuman || result.FinalMessage != "implemented" {
		t.Fatalf("result = %#v", result)
	}

	stored, err := app.getStoryPipeline(context.Background(), runID, story.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Phase != PipelinePhaseAwaitingHuman {
		t.Fatalf("pipeline phase = %q, want %q", stored.Phase, PipelinePhaseAwaitingHuman)
	}
	if stored.PRNumber != 42 || stored.PRURL == "" {
		t.Fatalf("pipeline PR missing: %#v", stored)
	}
	if stored.Phase == PipelinePhaseMerge || stored.Phase == PipelinePhaseCompleted {
		t.Fatalf("supervised pause must not complete merge path")
	}

	loaded, err := app.getStory(context.Background(), story.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != StatusInReview {
		t.Fatalf("story status = %q, want %q", loaded.Status, StatusInReview)
	}

	events, err := app.listEvents(context.Background(), story.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Type == "awaiting_human_review" && strings.Contains(ev.Message, "pull/42") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("missing awaiting_human_review event: %#v", events)
	}

	// Queue item outcome: awaiting_human, not completed/merged.
	completed, _, err := app.applyStoryPipelineOutcome(context.Background(), runID, story, result, 0, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if completed != 1 {
		t.Fatalf("completed count = %d", completed)
	}
	items, err := app.listQueueRunItems(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Status != QueueItemAwaitingHuman {
		t.Fatalf("queue items = %#v", items)
	}
	// Story must remain in_review (not flipped to done).
	loaded, err = app.getStory(context.Background(), story.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != StatusInReview {
		t.Fatalf("after outcome story status = %q, want %q", loaded.Status, StatusInReview)
	}
}

func TestAutonomousPipelineOutcomeMarksDone(t *testing.T) {
	app := testApp(t)
	stories := seedProjectStories(t, app, "atlas", 1)
	story := stories[0]
	if err := app.changeStoryStatus(context.Background(), story.ID, StatusQueued, false, "queue"); err != nil {
		t.Fatal(err)
	}
	queued, _ := app.listStories(context.Background(), storyFilters{ProjectID: "atlas", Status: StatusQueued, ShowClosed: true})
	runID, err := app.createQueueRun(context.Background(), storyFilters{ProjectID: "atlas"}, queued)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.changeStoryStatus(context.Background(), story.ID, StatusInProgress, false, "running"); err != nil {
		t.Fatal(err)
	}

	result := pipelineResult{FinalMessage: "merged ok", AwaitingHuman: false}
	completed, _, err := app.applyStoryPipelineOutcome(context.Background(), runID, story, result, 0, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if completed != 1 {
		t.Fatalf("completed = %d", completed)
	}
	loaded, err := app.getStory(context.Background(), story.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != StatusDone {
		t.Fatalf("autonomous outcome status = %q, want done", loaded.Status)
	}
	items, err := app.listQueueRunItems(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Status != QueueItemCompleted {
		t.Fatalf("queue item status = %q, want completed", items[0].Status)
	}
}

func TestQueueContinuesAfterSupervisedPause(t *testing.T) {
	app := testApp(t)
	stories := seedProjectStories(t, app, "atlas", 2)
	for _, story := range stories {
		if err := app.changeStoryStatus(context.Background(), story.ID, StatusQueued, false, "queue"); err != nil {
			t.Fatal(err)
		}
	}
	queued, _ := app.listStories(context.Background(), storyFilters{ProjectID: "atlas", Status: StatusQueued, ShowClosed: true})
	runID, err := app.createQueueRun(context.Background(), storyFilters{ProjectID: "atlas"}, queued)
	if err != nil {
		t.Fatal(err)
	}

	// First story pauses for human; second completes autonomously.
	if err := app.changeStoryStatus(context.Background(), stories[0].ID, StatusInProgress, false, "run"); err != nil {
		t.Fatal(err)
	}
	project, err := app.getProject(context.Background(), "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.updateProjectAutonomyMode(context.Background(), "atlas", AutonomySupervised); err != nil {
		t.Fatal(err)
	}
	project, err = app.getProject(context.Background(), "atlas")
	if err != nil {
		t.Fatal(err)
	}
	result1, err := app.pausePipelineForHuman(context.Background(), pipelineContext{
		QueueRunID: runID, Project: project, Story: stories[0],
	}, StoryPipeline{
		QueueRunID: runID, StoryID: stories[0].ID, PRNumber: 1, PRURL: "https://example.com/pull/1",
	}, "first")
	if err != nil {
		t.Fatal(err)
	}
	completed, prev, err := app.applyStoryPipelineOutcome(context.Background(), runID, stories[0], result1, 0, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if completed != 1 {
		t.Fatalf("after first story completed count = %d", completed)
	}

	if err := app.changeStoryStatus(context.Background(), stories[1].ID, StatusInProgress, false, "run"); err != nil {
		t.Fatal(err)
	}
	result2 := pipelineResult{FinalMessage: "second merged", AwaitingHuman: false}
	completed, _, err = app.applyStoryPipelineOutcome(context.Background(), runID, stories[1], result2, completed, 2, prev)
	if err != nil {
		t.Fatal(err)
	}
	if completed != 2 {
		t.Fatalf("after second story completed count = %d", completed)
	}

	items, err := app.listQueueRunItems(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items len = %d", len(items))
	}
	// Order matches queue positions (story order from seed).
	byID := map[string]string{}
	for _, item := range items {
		byID[item.Story.ID] = item.Status
	}
	if byID[stories[0].ID] != QueueItemAwaitingHuman {
		t.Fatalf("first item status = %q", byID[stories[0].ID])
	}
	if byID[stories[1].ID] != QueueItemCompleted {
		t.Fatalf("second item status = %q", byID[stories[1].ID])
	}

	s0, _ := app.getStory(context.Background(), stories[0].ID)
	s1, _ := app.getStory(context.Background(), stories[1].ID)
	if s0.Status != StatusInReview {
		t.Fatalf("first story status = %q", s0.Status)
	}
	if s1.Status != StatusDone {
		t.Fatalf("second story status = %q", s1.Status)
	}
}

func TestAddressFeedbackWrongStatusRejected(t *testing.T) {
	app := testApp(t)
	stories := seedProjectStories(t, app, "atlas", 1)
	form := url.Values{"redirect": {"/projects/atlas/backlog"}}
	req := httptest.NewRequest(http.MethodPost, "/stories/"+stories[0].ID+"/address-feedback", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, req)
	if res.Code != http.StatusSeeOther {
		t.Fatalf("status = %d; body = %s", res.Code, res.Body.String())
	}
	loc := res.Header().Get("Location")
	decoded, _ := url.QueryUnescape(loc)
	if !strings.Contains(loc, "/projects/atlas/backlog") || !strings.Contains(decoded, "in review") {
		t.Fatalf("expected redirect with in-review error, got %q", loc)
	}
}

func TestResolveConflictsWrongStatusRejected(t *testing.T) {
	app := testApp(t)
	stories := seedProjectStories(t, app, "atlas", 1)
	form := url.Values{"redirect": {"/projects/atlas/backlog"}}
	req := httptest.NewRequest(http.MethodPost, "/stories/"+stories[0].ID+"/resolve-conflicts", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, req)
	if res.Code != http.StatusSeeOther {
		t.Fatalf("status = %d; body = %s", res.Code, res.Body.String())
	}
	decoded, _ := url.QueryUnescape(res.Header().Get("Location"))
	if !strings.Contains(decoded, "in review") {
		t.Fatalf("expected in-review error, got %q", decoded)
	}
}

func TestAddressFeedbackHappyPathReturnsToInReview(t *testing.T) {
	app := testApp(t)
	project, err := app.createProject(context.Background(), "atlas", "Atlas", "A", "/tmp", AutonomySupervised)
	if err != nil {
		t.Fatal(err)
	}
	story, err := app.createStory(context.Background(), createStoryRequest{
		ProjectID: project.ID, Title: "Feedback story", Description: "desc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.changeStoryStatus(context.Background(), story.ID, StatusQueued, false, "queue"); err != nil {
		t.Fatal(err)
	}
	queued, _ := app.listStories(context.Background(), storyFilters{ProjectID: project.ID, Status: StatusQueued, ShowClosed: true})
	runID, err := app.createQueueRun(context.Background(), storyFilters{ProjectID: project.ID}, queued)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.changeStoryStatus(context.Background(), story.ID, StatusInProgress, false, "run"); err != nil {
		t.Fatal(err)
	}
	pipeline := StoryPipeline{
		QueueRunID: runID,
		StoryID:    story.ID,
		Phase:      PipelinePhaseAwaitingHuman,
		Branch:     "ripple/A-001-feedback-story",
		PRNumber:   9,
		PRURL:      "https://github.com/acme/atlas/pull/9",
		ReviewJSON: `{"approved":false,"summary":"needs work","comments":[{"path":"a.go","line":1,"body":"fix me"}]}`,
	}
	if err := app.upsertStoryPipeline(context.Background(), pipeline); err != nil {
		t.Fatal(err)
	}
	if err := app.changeStoryStatus(context.Background(), story.ID, StatusInReview, false, "awaiting"); err != nil {
		t.Fatal(err)
	}

	feedback := PRFeedback{
		Items: []PRFeedbackItem{
			{Kind: "issue_comment", Author: "human", Body: "Please rename the helper"},
		},
		AgentReviewJSON: pipeline.ReviewJSON,
	}
	// Simulate agent activity: record a fix run, then complete the supervised loop (no quality gate).
	agentRunID, err := app.createAgentRun(context.Background(), runID, project, story, "prompt", RunKindCodexAddressFeedback, pipeline.Branch, pipeline.PRNumber, pipeline.PRURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.finishAgentStoryRun(context.Background(), agentRunID, "completed", "", "", "renamed helper", nil); err != nil {
		t.Fatal(err)
	}
	if err := app.changeStoryStatus(context.Background(), story.ID, StatusInProgress, false, "addressing"); err != nil {
		t.Fatal(err)
	}
	if err := app.completeAddressFeedback(context.Background(), story, pipeline, feedback, true, "renamed helper"); err != nil {
		t.Fatal(err)
	}

	loaded, err := app.getStory(context.Background(), story.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != StatusInReview {
		t.Fatalf("status = %q, want in_review", loaded.Status)
	}
	stored, err := app.getLatestStoryPipeline(context.Background(), story.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Phase != PipelinePhaseAwaitingHuman {
		t.Fatalf("phase = %q, want awaiting_human", stored.Phase)
	}
	events, err := app.listEvents(context.Background(), story.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundAddressed := false
	for _, ev := range events {
		if ev.Type == eventFeedbackAddressed && strings.Contains(ev.Message, feedbackFingerprintPrefix) {
			foundAddressed = true
			break
		}
	}
	if !foundAddressed {
		t.Fatalf("missing feedback_addressed event: %#v", events)
	}
	// Second pass with same feedback must not start (no new comments).
	if err := evaluateAddressFeedback(feedback, events); err == nil {
		t.Fatal("expected no-new-comments rejection on second identical feedback")
	}
	// Agent run of address-feedback kind is recorded.
	runs, err := app.listAgentStoryRuns(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	foundKind := false
	for _, r := range runs {
		if r.RunKind == RunKindCodexAddressFeedback {
			foundKind = true
			break
		}
	}
	if !foundKind {
		t.Fatalf("address-feedback run not recorded: %#v", runs)
	}
}

func TestAddressFeedbackNoChangesEvent(t *testing.T) {
	app := testApp(t)
	stories := seedProjectStories(t, app, "atlas", 1)
	story := stories[0]
	if err := app.changeStoryStatus(context.Background(), story.ID, StatusQueued, false, "queue"); err != nil {
		t.Fatal(err)
	}
	queued, _ := app.listStories(context.Background(), storyFilters{ProjectID: "atlas", Status: StatusQueued, ShowClosed: true})
	runID, _ := app.createQueueRun(context.Background(), storyFilters{ProjectID: "atlas"}, queued)
	pipeline := StoryPipeline{QueueRunID: runID, StoryID: story.ID, Phase: PipelinePhaseAwaitingHuman, Branch: "b", PRNumber: 1, PRURL: "https://example.com/pull/1"}
	_ = app.upsertStoryPipeline(context.Background(), pipeline)
	_ = app.changeStoryStatus(context.Background(), story.ID, StatusInReview, false, "await")
	feedback := PRFeedback{Items: []PRFeedbackItem{{Kind: "review", Author: "x", Body: "nits"}}}
	if err := app.completeAddressFeedback(context.Background(), story, pipeline, feedback, false, "nothing to change"); err != nil {
		t.Fatal(err)
	}
	events, _ := app.listEvents(context.Background(), story.ID)
	found := false
	for _, ev := range events {
		if ev.Type == eventFeedbackNoChanges {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected feedback_no_changes event: %#v", events)
	}
	loaded, _ := app.getStory(context.Background(), story.ID)
	if loaded.Status != StatusInReview {
		t.Fatalf("status = %q", loaded.Status)
	}
}

func TestStoryPanelShowsAddressFeedbackForInReview(t *testing.T) {
	app := testApp(t)
	stories := seedProjectStories(t, app, "atlas", 1)
	story := stories[0]
	if err := app.changeStoryStatus(context.Background(), story.ID, StatusQueued, false, "queue"); err != nil {
		t.Fatal(err)
	}
	queued, _ := app.listStories(context.Background(), storyFilters{ProjectID: "atlas", Status: StatusQueued, ShowClosed: true})
	runID, _ := app.createQueueRun(context.Background(), storyFilters{ProjectID: "atlas"}, queued)
	_ = app.upsertStoryPipeline(context.Background(), StoryPipeline{
		QueueRunID: runID, StoryID: story.ID, Phase: PipelinePhaseAwaitingHuman,
		Branch: "ripple/branch", PRNumber: 3, PRURL: "https://github.com/acme/atlas/pull/3",
	})
	_ = app.changeStoryStatus(context.Background(), story.ID, StatusInReview, false, "await")

	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/stories/"+story.ID+"/panel", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d", res.Code)
	}
	body := res.Body.String()
	for _, marker := range []string{
		"Act on review comments",
		`action="/stories/` + story.ID + `/address-feedback"`,
		`action="/stories/` + story.ID + `/merge"`,
		`action="/stories/` + story.ID + `/resolve-conflicts"`,
		"Merge pull request",
		"Fix merge conflicts",
		"I already merged on GitHub",
		"https://github.com/acme/atlas/pull/3",
		"Open pull request",
		"action-menu",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("story panel missing %q", marker)
		}
	}
}

func TestHumanMergeWrongStatusRejected(t *testing.T) {
	app := testApp(t)
	stories := seedProjectStories(t, app, "atlas", 1)
	form := url.Values{"redirect": {"/projects/atlas/backlog"}}
	req := httptest.NewRequest(http.MethodPost, "/stories/"+stories[0].ID+"/merge", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, req)
	if res.Code != http.StatusSeeOther {
		t.Fatalf("status = %d; body = %s", res.Code, res.Body.String())
	}
	loc := res.Header().Get("Location")
	decoded, _ := url.QueryUnescape(loc)
	if !strings.Contains(loc, "/projects/atlas/backlog") || !strings.Contains(decoded, "in review") {
		t.Fatalf("expected redirect with in-review error, got %q", loc)
	}
}

func TestHumanMergeSuccessMarksDone(t *testing.T) {
	app := testApp(t)
	stories := seedProjectStories(t, app, "atlas", 1)
	story := stories[0]
	if err := app.changeStoryStatus(context.Background(), story.ID, StatusQueued, false, "queue"); err != nil {
		t.Fatal(err)
	}
	queued, _ := app.listStories(context.Background(), storyFilters{ProjectID: "atlas", Status: StatusQueued, ShowClosed: true})
	runID, err := app.createQueueRun(context.Background(), storyFilters{ProjectID: "atlas"}, queued)
	if err != nil {
		t.Fatal(err)
	}
	pipeline := StoryPipeline{
		QueueRunID:    runID,
		StoryID:       story.ID,
		Phase:         PipelinePhaseAwaitingHuman,
		DefaultBranch: "main",
		PRNumber:      11,
		PRURL:         "https://github.com/acme/atlas/pull/11",
	}
	if err := app.upsertStoryPipeline(context.Background(), pipeline); err != nil {
		t.Fatal(err)
	}
	if err := app.changeStoryStatus(context.Background(), story.ID, StatusInReview, false, "await"); err != nil {
		t.Fatal(err)
	}
	if err := app.updateQueueRunItemStatus(context.Background(), runID, story.ID, QueueItemAwaitingHuman); err != nil {
		t.Fatal(err)
	}

	origGate, origMerge, origConflicts := humanMergeQualityGate, humanMergePR, checkPRMergeConflicts
	origDelay, origRetry := afterMergeConflictProbeDelay, mergeConflictRetryDelay
	t.Cleanup(func() {
		humanMergeQualityGate = origGate
		humanMergePR = origMerge
		checkPRMergeConflicts = origConflicts
		afterMergeConflictProbeDelay = origDelay
		mergeConflictRetryDelay = origRetry
	})
	afterMergeConflictProbeDelay = 0
	mergeConflictRetryDelay = 0
	humanMergeQualityGate = func(ctx context.Context, dir string) error { return nil }
	humanMergePR = func(ctx context.Context, ghBin, dir string, prNumber int, deleteRemoteBranch bool) error {
		if prNumber != 11 {
			t.Fatalf("prNumber = %d", prNumber)
		}
		return nil
	}
	checkPRMergeConflicts = func(ctx context.Context, ghBin, dir string, prNumber int) (bool, string, error) {
		return false, "", nil
	}

	project, err := app.getProject(context.Background(), "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.executeHumanMerge(context.Background(), story, project, pipeline); err != nil {
		t.Fatal(err)
	}

	loaded, err := app.getStory(context.Background(), story.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != StatusDone {
		t.Fatalf("status = %q, want done", loaded.Status)
	}
	stored, err := app.getLatestStoryPipeline(context.Background(), story.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Phase != PipelinePhaseCompleted {
		t.Fatalf("phase = %q, want completed", stored.Phase)
	}
	events, err := app.listEvents(context.Background(), story.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Type == eventMergedByHuman && strings.Contains(ev.Message, "PR #11") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("missing merged_by_human event: %#v", events)
	}
	items, err := app.listQueueRunItems(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Status != QueueItemCompleted {
		t.Fatalf("queue item status = %q, want completed", items[0].Status)
	}
}

func TestRefreshAwaitingPRConflictsMarksSibling(t *testing.T) {
	app := testApp(t)
	stories := seedProjectStories(t, app, "atlas", 2)
	first, second := stories[0], stories[1]
	for _, story := range stories {
		if err := app.changeStoryStatus(context.Background(), story.ID, StatusQueued, false, "queue"); err != nil {
			t.Fatal(err)
		}
	}
	queued, _ := app.listStories(context.Background(), storyFilters{ProjectID: "atlas", Status: StatusQueued, ShowClosed: true})
	runID, err := app.createQueueRun(context.Background(), storyFilters{ProjectID: "atlas"}, queued)
	if err != nil {
		t.Fatal(err)
	}
	for i, story := range []Story{first, second} {
		_ = app.upsertStoryPipeline(context.Background(), StoryPipeline{
			QueueRunID: runID, StoryID: story.ID, Phase: PipelinePhaseAwaitingHuman,
			Branch: fmt.Sprintf("b-%d", i+1), DefaultBranch: "main",
			PRNumber: 20 + i, PRURL: fmt.Sprintf("https://example.com/pull/%d", 20+i),
		})
		_ = app.changeStoryStatus(context.Background(), story.ID, StatusInReview, false, "await")
		_ = app.updateQueueRunItemStatus(context.Background(), runID, story.ID, QueueItemAwaitingHuman)
	}

	origConflicts := checkPRMergeConflicts
	origDelay, origRetry := afterMergeConflictProbeDelay, mergeConflictRetryDelay
	t.Cleanup(func() {
		checkPRMergeConflicts = origConflicts
		afterMergeConflictProbeDelay = origDelay
		mergeConflictRetryDelay = origRetry
	})
	afterMergeConflictProbeDelay = 0
	mergeConflictRetryDelay = 0
	checkPRMergeConflicts = func(ctx context.Context, ghBin, dir string, prNumber int) (bool, string, error) {
		if prNumber == 21 {
			return true, "Pull request has merge conflicts with the base branch", nil
		}
		return false, "", nil
	}

	// Completing the first story should probe siblings and flag conflicts on the second.
	if err := app.finalizeMergedStory(context.Background(), first, StoryPipeline{
		QueueRunID: runID, StoryID: first.ID, Phase: PipelinePhaseMerge, PRNumber: 20, PRURL: "https://example.com/pull/20",
	}, eventMergedByHuman, "Merged by human", "merged"); err != nil {
		t.Fatal(err)
	}

	secondPipe, err := app.getLatestStoryPipeline(context.Background(), second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !secondPipe.MergeConflict {
		t.Fatal("expected second story pipeline to be marked merge_conflict")
	}

	_, _, _, err = app.prepareHumanMerge(context.Background(), second.ID)
	if err == nil || !strings.Contains(err.Error(), "merge conflicts") {
		t.Fatalf("prepareHumanMerge should block conflicted PR, got %v", err)
	}

	summary := buildRunCompletionSummary(QueueRunSummary{ID: runID}, []AgentRunSummary{
		{StoryID: first.ID, StoryTitle: first.Title, PRNumber: 20, PRURL: "https://example.com/pull/20"},
		{StoryID: second.ID, StoryTitle: second.Title, PRNumber: 21, PRURL: "https://example.com/pull/21", Branch: "b-2"},
	}, []QueueRunItem{
		{Story: first, Status: QueueItemCompleted},
		{Story: second, Status: QueueItemAwaitingHuman},
	})
	app.enrichAwaitingPRConflicts(context.Background(), runID, &summary)
	if len(summary.AwaitingHumanPRs) != 1 || !summary.AwaitingHumanPRs[0].HasMergeConflict {
		t.Fatalf("AwaitingHumanPRs = %#v", summary.AwaitingHumanPRs)
	}

	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/projects/atlas/runs/%d", runID), nil))
	body := res.Body.String()
	if !strings.Contains(body, "Merge conflicts with the base branch") {
		t.Fatal("run page should warn about merge conflicts")
	}
	if !strings.Contains(body, `title="Resolve merge conflicts first"`) && !strings.Contains(body, "disabled") {
		t.Fatal("merge button should be disabled for conflicted PR")
	}
}

func TestLooksLikeMergeConflictError(t *testing.T) {
	if !looksLikeMergeConflictError("merge PR failed: GraphQL: Pull Request has merge conflicts (mergePullRequest)") {
		t.Fatal("expected conflict detection")
	}
	if looksLikeMergeConflictError("permission denied") {
		t.Fatal("should not treat unrelated errors as conflicts")
	}
}

func TestHumanMergeQualityGateFailureStaysInReview(t *testing.T) {
	app := testApp(t)
	stories := seedProjectStories(t, app, "atlas", 1)
	story := stories[0]
	if err := app.changeStoryStatus(context.Background(), story.ID, StatusQueued, false, "queue"); err != nil {
		t.Fatal(err)
	}
	queued, _ := app.listStories(context.Background(), storyFilters{ProjectID: "atlas", Status: StatusQueued, ShowClosed: true})
	runID, _ := app.createQueueRun(context.Background(), storyFilters{ProjectID: "atlas"}, queued)
	pipeline := StoryPipeline{
		QueueRunID: runID, StoryID: story.ID, Phase: PipelinePhaseAwaitingHuman,
		DefaultBranch: "main", PRNumber: 5, PRURL: "https://example.com/pull/5",
	}
	_ = app.upsertStoryPipeline(context.Background(), pipeline)
	_ = app.changeStoryStatus(context.Background(), story.ID, StatusInReview, false, "await")

	origGate, origMerge := humanMergeQualityGate, humanMergePR
	t.Cleanup(func() {
		humanMergeQualityGate = origGate
		humanMergePR = origMerge
	})
	humanMergeQualityGate = func(ctx context.Context, dir string) error {
		return errors.New("go test failed: assertion error")
	}
	merged := false
	humanMergePR = func(ctx context.Context, ghBin, dir string, prNumber int, deleteRemoteBranch bool) error {
		merged = true
		return nil
	}

	project, _ := app.getProject(context.Background(), "atlas")
	err := app.executeHumanMerge(context.Background(), story, project, pipeline)
	if err == nil {
		t.Fatal("expected quality gate error")
	}
	if !strings.Contains(err.Error(), "Quality gate failed") {
		t.Fatalf("error = %v", err)
	}
	if merged {
		t.Fatal("merge must not run after quality gate failure")
	}
	loaded, _ := app.getStory(context.Background(), story.ID)
	if loaded.Status != StatusInReview {
		t.Fatalf("status = %q, want in_review", loaded.Status)
	}
	stored, _ := app.getLatestStoryPipeline(context.Background(), story.ID)
	if stored.Phase != PipelinePhaseAwaitingHuman {
		t.Fatalf("phase = %q, want awaiting_human", stored.Phase)
	}
	if !strings.Contains(stored.Error, "go test failed") {
		t.Fatalf("pipeline error = %q", stored.Error)
	}
	events, _ := app.listEvents(context.Background(), story.ID)
	found := false
	for _, ev := range events {
		if ev.Type == eventQualityGateFailed {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("missing quality_gate_failed event: %#v", events)
	}
}

func TestCompleteHumanMergeOnly(t *testing.T) {
	app := testApp(t)
	stories := seedProjectStories(t, app, "atlas", 1)
	story := stories[0]
	_ = app.changeStoryStatus(context.Background(), story.ID, StatusQueued, false, "queue")
	queued, _ := app.listStories(context.Background(), storyFilters{ProjectID: "atlas", Status: StatusQueued, ShowClosed: true})
	runID, err := app.createQueueRun(context.Background(), storyFilters{ProjectID: "atlas"}, queued)
	if err != nil {
		t.Fatal(err)
	}
	_ = app.changeStoryStatus(context.Background(), story.ID, StatusInReview, false, "await")
	pipeline := StoryPipeline{QueueRunID: runID, StoryID: story.ID, Phase: PipelinePhaseMerge, PRNumber: 2, PRURL: "https://example.com/pull/2"}
	if err := app.completeHumanMerge(context.Background(), story, pipeline); err != nil {
		t.Fatal(err)
	}
	loaded, _ := app.getStory(context.Background(), story.ID)
	if loaded.Status != StatusDone {
		t.Fatalf("status = %q", loaded.Status)
	}
}

func TestBotAPICannotSetInReview(t *testing.T) {
	app := testApp(t)
	stories := seedProjectStories(t, app, "atlas", 1)
	body := `{"status":"in_review"}`
	req := httptest.NewRequest(http.MethodPatch, "/api/stories/"+stories[0].ID+"/status", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; body = %s", res.Code, res.Body.String())
	}
	loaded, err := app.getStory(context.Background(), stories[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status == StatusInReview {
		t.Fatalf("bot must not set in_review")
	}
}

func TestBoardAndBacklogShowInReview(t *testing.T) {
	app := testApp(t)
	stories := seedProjectStories(t, app, "atlas", 1)
	if err := app.changeStoryStatus(context.Background(), stories[0].ID, StatusInReview, false, "test"); err != nil {
		t.Fatal(err)
	}

	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/board?projectId=atlas", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("board status = %d", res.Code)
	}
	body := res.Body.String()
	for _, marker := range []string{"In Review", `column-in_review`, stories[0].Title, `value="in_review"`} {
		if !strings.Contains(body, marker) {
			t.Fatalf("board missing %q", marker)
		}
	}

	res = httptest.NewRecorder()
	app.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/projects/atlas/backlog?status=in_review", nil))
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), stories[0].Title) {
		t.Fatalf("backlog in_review filter failed: %d %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "In review") {
		t.Fatalf("backlog missing In review tab")
	}
}

func TestNormalizeAutonomyMode(t *testing.T) {
	cases := map[string]string{
		"":             AutonomyAutonomous,
		"  ":           AutonomyAutonomous,
		"autonomous":   AutonomyAutonomous,
		"Autonomous":   AutonomyAutonomous,
		"supervised":   AutonomySupervised,
		"SUPERVISED":   AutonomySupervised,
		" supervised ": AutonomySupervised,
		"manual":       AutonomyAutonomous,
		"invalid":      AutonomyAutonomous,
	}
	for input, want := range cases {
		if got := normalizeAutonomyMode(input); got != want {
			t.Fatalf("normalizeAutonomyMode(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestProjectAutonomyModeRoundTrip(t *testing.T) {
	app := testApp(t)
	project, err := app.createProject(context.Background(), "atlas", "Atlas", "A", "/tmp", AutonomySupervised)
	if err != nil {
		t.Fatal(err)
	}
	if project.AutonomyMode != AutonomySupervised {
		t.Fatalf("createProject autonomyMode = %q, want %q", project.AutonomyMode, AutonomySupervised)
	}
	loaded, err := app.getProject(context.Background(), "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AutonomyMode != AutonomySupervised {
		t.Fatalf("getProject autonomyMode = %q, want %q", loaded.AutonomyMode, AutonomySupervised)
	}
	if err := app.updateProjectAutonomyMode(context.Background(), "atlas", AutonomyAutonomous); err != nil {
		t.Fatal(err)
	}
	loaded, err = app.getProject(context.Background(), "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AutonomyMode != AutonomyAutonomous {
		t.Fatalf("after update autonomyMode = %q, want %q", loaded.AutonomyMode, AutonomyAutonomous)
	}
	if err := app.updateProjectAutonomyMode(context.Background(), "atlas", "not-a-mode"); err != nil {
		t.Fatal(err)
	}
	loaded, err = app.getProject(context.Background(), "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AutonomyMode != AutonomyAutonomous {
		t.Fatalf("invalid value should normalize to autonomous, got %q", loaded.AutonomyMode)
	}
}

func TestProjectAutonomyModeDefaultsAutonomous(t *testing.T) {
	app := testApp(t)
	project, err := app.createProject(context.Background(), "atlas", "Atlas", "A", "/tmp", "")
	if err != nil {
		t.Fatal(err)
	}
	if project.AutonomyMode != AutonomyAutonomous {
		t.Fatalf("default autonomyMode = %q, want %q", project.AutonomyMode, AutonomyAutonomous)
	}
	// Existing rows with empty/invalid stored values normalize on read.
	if _, err := app.db.ExecContext(context.Background(), `UPDATE projects SET autonomy_mode = '' WHERE id = ?`, "atlas"); err != nil {
		t.Fatal(err)
	}
	loaded, err := app.getProject(context.Background(), "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AutonomyMode != AutonomyAutonomous {
		t.Fatalf("empty DB value should normalize to autonomous, got %q", loaded.AutonomyMode)
	}
}

func TestProjectSettingsFormPostsAutonomyMode(t *testing.T) {
	app := testApp(t)
	seedProjectStories(t, app, "atlas", 1)

	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/projects/atlas/backlog", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("backlog status = %d", res.Code)
	}
	body := res.Body.String()
	for _, marker := range []string{
		`action="/projects/atlas/settings"`,
		`name="autonomyMode"`,
		`value="autonomous"`,
		`value="supervised"`,
		"Save settings",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("backlog project settings missing %q", marker)
		}
	}

	form := url.Values{
		"workingDirectory": {"/tmp"},
		"autonomyMode":     {AutonomySupervised},
		"redirect":         {"/projects/atlas/backlog"},
	}
	req := httptest.NewRequest(http.MethodPost, "/projects/atlas/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res = httptest.NewRecorder()
	app.routes().ServeHTTP(res, req)
	if res.Code != http.StatusSeeOther && res.Code != http.StatusOK {
		t.Fatalf("settings post status = %d; body = %s", res.Code, res.Body.String())
	}
	project, err := app.getProject(context.Background(), "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if project.AutonomyMode != AutonomySupervised {
		t.Fatalf("after form post autonomyMode = %q, want %q", project.AutonomyMode, AutonomySupervised)
	}

	// API list/create include autonomyMode.
	res = httptest.NewRecorder()
	app.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/projects", nil))
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"autonomyMode":"supervised"`) {
		t.Fatalf("API list projects missing autonomyMode; body = %s", res.Body.String())
	}

	createBody := `{"id":"nova","name":"Nova","prefix":"N","workingDirectory":"/tmp","autonomyMode":"supervised"}`
	req = httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader(createBody))
	req.Header.Set("Content-Type", "application/json")
	res = httptest.NewRecorder()
	app.routes().ServeHTTP(res, req)
	if res.Code != http.StatusCreated {
		t.Fatalf("API create status = %d; body = %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), `"autonomyMode":"supervised"`) {
		t.Fatalf("API create response missing autonomyMode; body = %s", res.Body.String())
	}
}

func TestBuildRunCompletionSummaryDistinguishesAwaitingHuman(t *testing.T) {
	finished := time.Now().UTC()
	run := QueueRunSummary{ID: 1, Status: "completed", Total: 2, Completed: 2, StartedAt: finished.Add(-2 * time.Minute), FinishedAt: &finished}
	storyRuns := []AgentRunSummary{
		{StoryID: "A-001", StoryTitle: "One", PRNumber: 10, PRURL: "https://example.com/pull/10", Branch: "b1"},
		{StoryID: "A-002", StoryTitle: "Two", PRNumber: 11, PRURL: "https://example.com/pull/11", Branch: "b2"},
		// Second agent pass on same PR should not duplicate.
		{StoryID: "A-001", StoryTitle: "One", PRNumber: 10, PRURL: "https://example.com/pull/10", Branch: "b1", RunKind: RunKindGrokReview},
	}
	items := []QueueRunItem{
		{Story: Story{ID: "A-001", Title: "One"}, Status: QueueItemAwaitingHuman},
		{Story: Story{ID: "A-002", Title: "Two"}, Status: QueueItemCompleted},
	}
	summary := buildRunCompletionSummary(run, storyRuns, items)
	if summary.MergedCount != 1 {
		t.Fatalf("MergedCount = %d, want 1", summary.MergedCount)
	}
	if summary.AwaitingHumanCount != 1 {
		t.Fatalf("AwaitingHumanCount = %d, want 1", summary.AwaitingHumanCount)
	}
	if len(summary.MergedPRs) != 1 || summary.MergedPRs[0].Number != 11 {
		t.Fatalf("MergedPRs = %#v", summary.MergedPRs)
	}
	if len(summary.AwaitingHumanPRs) != 1 || summary.AwaitingHumanPRs[0].Number != 10 {
		t.Fatalf("AwaitingHumanPRs = %#v", summary.AwaitingHumanPRs)
	}
	if summary.Elapsed == "" {
		t.Fatal("expected elapsed time")
	}
}

func TestEventAndQueueItemTitles(t *testing.T) {
	if got := eventTitle(eventAwaitingHumanReview); got != "Awaiting your review" {
		t.Fatalf("eventTitle awaiting = %q", got)
	}
	if got := eventTitle(eventAddressingFeedback); got != "Addressing feedback" {
		t.Fatalf("eventTitle addressing = %q", got)
	}
	if got := eventTitle(eventResolvingConflicts); got != "Resolving conflicts" {
		t.Fatalf("eventTitle resolving = %q", got)
	}
	if got := eventTitle(eventConflictsResolved); got != "Conflicts resolved" {
		t.Fatalf("eventTitle resolved = %q", got)
	}
	if got := eventTitle(eventMergeConflictDetected); got != "Merge conflicts detected" {
		t.Fatalf("eventTitle merge conflict = %q", got)
	}
	if got := eventTitle(eventMergedByHuman); got != "Merged by you" {
		t.Fatalf("eventTitle merged by human = %q", got)
	}
	if got := eventTitle(eventMergedExternally); got != "Synced external merge" {
		t.Fatalf("eventTitle external = %q", got)
	}
	if got := runKindTitle(RunKindCodexResolveConflicts); got != "Resolve merge conflicts" {
		t.Fatalf("runKindTitle resolve = %q", got)
	}
	if got := queueItemStatusTitle(QueueItemAwaitingHuman); got != "waiting on you" {
		t.Fatalf("queue item title = %q", got)
	}
	if got := queueItemStatusTitle(QueueItemCompleted); got != "merged" {
		t.Fatalf("queue item completed title = %q", got)
	}
}

func TestStoryPanelShowsSyncPRForInReview(t *testing.T) {
	app := testApp(t)
	stories := seedProjectStories(t, app, "atlas", 1)
	story := stories[0]
	_ = app.changeStoryStatus(context.Background(), story.ID, StatusQueued, false, "queue")
	queued, _ := app.listStories(context.Background(), storyFilters{ProjectID: "atlas", Status: StatusQueued, ShowClosed: true})
	runID, _ := app.createQueueRun(context.Background(), storyFilters{ProjectID: "atlas"}, queued)
	_ = app.upsertStoryPipeline(context.Background(), StoryPipeline{
		QueueRunID: runID, StoryID: story.ID, Phase: PipelinePhaseAwaitingHuman,
		Branch: "ripple/branch", PRNumber: 3, PRURL: "https://github.com/acme/atlas/pull/3",
	})
	_ = app.changeStoryStatus(context.Background(), story.ID, StatusInReview, false, "await")

	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/stories/"+story.ID+"/panel", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d", res.Code)
	}
	body := res.Body.String()
	for _, marker := range []string{
		"I already merged on GitHub",
		`action="/stories/` + story.ID + `/sync-pr"`,
		"already merged on GitHub",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("story panel missing %q", marker)
		}
	}
}

func TestStoryPanelHistoryUsesEventTitles(t *testing.T) {
	app := testApp(t)
	stories := seedProjectStories(t, app, "atlas", 1)
	story := stories[0]
	if err := app.addEvent(context.Background(), story.ID, eventAwaitingHumanReview, "waiting for you"); err != nil {
		t.Fatal(err)
	}
	if err := app.addEvent(context.Background(), story.ID, eventMergedByHuman, "merged"); err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/stories/"+story.ID+"/panel", nil))
	body := res.Body.String()
	if !strings.Contains(body, "Awaiting your review") {
		t.Fatalf("panel missing human-readable awaiting event: %s", body)
	}
	if !strings.Contains(body, "Merged by you") {
		t.Fatalf("panel missing human-readable merge event: %s", body)
	}
	if strings.Contains(body, "<strong>awaiting_human_review</strong>") {
		t.Fatal("panel still shows raw event type awaiting_human_review")
	}
}

func TestAboutPageMentionsSupervisedFlow(t *testing.T) {
	app := testApp(t)
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/about", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d", res.Code)
	}
	body := res.Body.String()
	for _, marker := range []string{"In review", "Supervised", "Autonomous", "Done always means merged"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("about page missing %q", marker)
		}
	}
}

func TestSyncExternalPRMergeSuccess(t *testing.T) {
	app := testApp(t)
	stories := seedProjectStories(t, app, "atlas", 1)
	story := stories[0]
	_ = app.changeStoryStatus(context.Background(), story.ID, StatusQueued, false, "queue")
	queued, _ := app.listStories(context.Background(), storyFilters{ProjectID: "atlas", Status: StatusQueued, ShowClosed: true})
	runID, err := app.createQueueRun(context.Background(), storyFilters{ProjectID: "atlas"}, queued)
	if err != nil {
		t.Fatal(err)
	}
	pipeline := StoryPipeline{
		QueueRunID:    runID,
		StoryID:       story.ID,
		Phase:         PipelinePhaseAwaitingHuman,
		DefaultBranch: "main",
		Branch:        "ripple/feature",
		PRNumber:      22,
		PRURL:         "https://github.com/acme/atlas/pull/22",
	}
	if err := app.upsertStoryPipeline(context.Background(), pipeline); err != nil {
		t.Fatal(err)
	}
	if err := app.changeStoryStatus(context.Background(), story.ID, StatusInReview, false, "await"); err != nil {
		t.Fatal(err)
	}
	if err := app.updateQueueRunItemStatus(context.Background(), runID, story.ID, QueueItemAwaitingHuman); err != nil {
		t.Fatal(err)
	}

	orig := checkPRMerged
	t.Cleanup(func() { checkPRMerged = orig })
	checkPRMerged = func(ctx context.Context, ghBin, dir string, prNumber int) (bool, error) {
		if prNumber != 22 {
			t.Fatalf("prNumber = %d", prNumber)
		}
		return true, nil
	}

	if err := app.syncExternalPRMerge(context.Background(), story.ID); err != nil {
		t.Fatal(err)
	}
	loaded, err := app.getStory(context.Background(), story.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != StatusDone {
		t.Fatalf("status = %q, want done", loaded.Status)
	}
	stored, err := app.getLatestStoryPipeline(context.Background(), story.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Phase != PipelinePhaseCompleted {
		t.Fatalf("phase = %q, want completed", stored.Phase)
	}
	events, err := app.listEvents(context.Background(), story.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Type == eventMergedExternally && strings.Contains(ev.Message, "PR #22") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("missing merged_externally event: %#v", events)
	}
	items, err := app.listQueueRunItems(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Status != QueueItemCompleted {
		t.Fatalf("queue item status = %q, want completed", items[0].Status)
	}
}

func TestSyncExternalPRNotMergedRejected(t *testing.T) {
	app := testApp(t)
	stories := seedProjectStories(t, app, "atlas", 1)
	story := stories[0]
	_ = app.changeStoryStatus(context.Background(), story.ID, StatusQueued, false, "queue")
	queued, _ := app.listStories(context.Background(), storyFilters{ProjectID: "atlas", Status: StatusQueued, ShowClosed: true})
	runID, _ := app.createQueueRun(context.Background(), storyFilters{ProjectID: "atlas"}, queued)
	_ = app.upsertStoryPipeline(context.Background(), StoryPipeline{
		QueueRunID: runID, StoryID: story.ID, Phase: PipelinePhaseAwaitingHuman, PRNumber: 9, PRURL: "https://example.com/pull/9",
	})
	_ = app.changeStoryStatus(context.Background(), story.ID, StatusInReview, false, "await")

	orig := checkPRMerged
	t.Cleanup(func() { checkPRMerged = orig })
	checkPRMerged = func(ctx context.Context, ghBin, dir string, prNumber int) (bool, error) {
		return false, nil
	}

	err := app.syncExternalPRMerge(context.Background(), story.ID)
	if err == nil {
		t.Fatal("expected error when PR not merged")
	}
	if !strings.Contains(err.Error(), "not merged") {
		t.Fatalf("error = %v", err)
	}
	loaded, _ := app.getStory(context.Background(), story.ID)
	if loaded.Status != StatusInReview {
		t.Fatalf("status = %q, want in_review", loaded.Status)
	}
}

func TestSyncExternalPRWrongStatusRejected(t *testing.T) {
	app := testApp(t)
	stories := seedProjectStories(t, app, "atlas", 1)
	form := url.Values{"redirect": {"/projects/atlas/backlog"}}
	req := httptest.NewRequest(http.MethodPost, "/stories/"+stories[0].ID+"/sync-pr", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, req)
	if res.Code != http.StatusSeeOther {
		t.Fatalf("status = %d; body = %s", res.Code, res.Body.String())
	}
	loc := res.Header().Get("Location")
	if !strings.Contains(loc, "/projects/atlas/backlog") || !strings.Contains(loc, "error=") || !strings.Contains(loc, "review") {
		t.Fatalf("expected redirect with error about in review, got %q", loc)
	}
}

func TestMergeWhileAgentRunningRedirectsWithError(t *testing.T) {
	app := testApp(t)
	stories := seedProjectStories(t, app, "atlas", 1)
	story := stories[0]
	_ = app.changeStoryStatus(context.Background(), story.ID, StatusQueued, false, "queue")
	queued, _ := app.listStories(context.Background(), storyFilters{ProjectID: "atlas", Status: StatusQueued, ShowClosed: true})
	runID, err := app.createQueueRun(context.Background(), storyFilters{ProjectID: "atlas"}, queued)
	if err != nil {
		t.Fatal(err)
	}
	_ = app.upsertStoryPipeline(context.Background(), StoryPipeline{
		QueueRunID: runID, StoryID: story.ID, Phase: PipelinePhaseAwaitingHuman,
		Branch: "b", PRNumber: 1, PRURL: "https://example.com/pull/1",
	})
	_ = app.changeStoryStatus(context.Background(), story.ID, StatusInReview, false, "await")

	// Simulate another agent action holding the global lock.
	app.agentMu.Lock()
	app.agentStatus = AgentStatus{Running: true, QueueRunID: runID, CurrentStoryID: story.ID, Message: "busy"}
	app.agentMu.Unlock()

	form := url.Values{"redirect": {fmt.Sprintf("/projects/atlas/runs/%d", runID)}}
	req := httptest.NewRequest(http.MethodPost, "/stories/"+story.ID+"/merge", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, req)
	if res.Code != http.StatusSeeOther {
		t.Fatalf("status = %d; body = %s", res.Code, res.Body.String())
	}
	loc := res.Header().Get("Location")
	if !strings.Contains(loc, fmt.Sprintf("/projects/atlas/runs/%d", runID)) {
		t.Fatalf("location = %q", loc)
	}
	decoded, _ := url.QueryUnescape(loc)
	if !strings.Contains(decoded, "already running") {
		t.Fatalf("expected already-running error in redirect, got %q", loc)
	}

	// Run page surfaces the error banner.
	getRes := httptest.NewRecorder()
	app.routes().ServeHTTP(getRes, httptest.NewRequest(http.MethodGet, loc, nil))
	if getRes.Code != http.StatusOK {
		t.Fatalf("run page status = %d", getRes.Code)
	}
	if !strings.Contains(getRes.Body.String(), "already running") {
		t.Fatal("run page should show action error banner")
	}
}

func TestRunPageShowsAwaitingHumanSummary(t *testing.T) {
	app := testApp(t)
	stories := seedProjectStories(t, app, "atlas", 1)
	story := stories[0]
	_ = app.changeStoryStatus(context.Background(), story.ID, StatusQueued, false, "queue")
	queued, _ := app.listStories(context.Background(), storyFilters{ProjectID: "atlas", Status: StatusQueued, ShowClosed: true})
	runID, err := app.createQueueRun(context.Background(), storyFilters{ProjectID: "atlas"}, queued)
	if err != nil {
		t.Fatal(err)
	}
	project, err := app.getProject(context.Background(), "atlas")
	if err != nil {
		t.Fatal(err)
	}
	agentRunID, err := app.createAgentRun(context.Background(), runID, project, story, "prompt", RunKindCodexImplement, "ripple/branch", 7, "https://example.com/pull/7")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.finishAgentStoryRun(context.Background(), agentRunID, "completed", "", "", "implemented", nil); err != nil {
		t.Fatal(err)
	}
	if err := app.updateQueueRunItemStatus(context.Background(), runID, story.ID, QueueItemAwaitingHuman); err != nil {
		t.Fatal(err)
	}
	if err := app.updateQueueRun(context.Background(), runID, "completed", "Queue run complete", 1, nil); err != nil {
		t.Fatal(err)
	}

	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/projects/atlas/runs/%d", runID), nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", res.Code, res.Body.String())
	}
	body := res.Body.String()
	for _, marker := range []string{
		"waiting on you",
		"Awaiting you",
		"Waiting on you",
		"https://example.com/pull/7",
		"Queue finished",
		`action="/stories/` + story.ID + `/address-feedback"`,
		`action="/stories/` + story.ID + `/merge"`,
		`action="/stories/` + story.ID + `/sync-pr"`,
		"Act on review comments",
		"Merge pull request",
		"I already merged on GitHub",
		"Fix merge conflicts",
		`action="/stories/` + story.ID + `/resolve-conflicts"`,
		"Waiting on you",
		"action-menu",
		fmt.Sprintf(`value="/projects/atlas/runs/%d"`, runID),
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("run page missing %q in body", marker)
		}
	}
	if strings.Contains(body, "Pull requests created and merged") {
		t.Fatal("run page must not claim all PRs were merged for supervised pause")
	}
	if strings.Contains(body, "Work finished successfully") {
		t.Fatal("run page must not say work finished successfully when stories await human")
	}
}

func TestDefaultRippleDBPathPrefersPopulatedLegacyDB(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	// Empty/new ripple.db shell.
	ripple, err := sql.Open("sqlite", "ripple.db")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ripple.Exec(`CREATE TABLE projects (id TEXT PRIMARY KEY, name TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	_ = ripple.Close()

	// Legacy DB with real data.
	legacy, err := sql.Open("sqlite", "taskmanager.db")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`CREATE TABLE projects (id TEXT PRIMARY KEY, name TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO projects (id, name) VALUES ('atlas', 'Atlas')`); err != nil {
		t.Fatal(err)
	}
	_ = legacy.Close()

	t.Setenv("RIPPLE_DB", "")
	t.Setenv("TASKMANAGER_DB", "")
	if got := defaultRippleDBPath(); got != "taskmanager.db" {
		t.Fatalf("defaultRippleDBPath() = %q, want taskmanager.db", got)
	}
}

func TestActionErrorNeedsAgentsSettings(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"Reviewer not available: Codex CLI was not found. Set a path in Settings → Agents.", true},
		{"Implementer not available: Grok CLI was not found.", true},
		{"Reviewer is not configured. Open Settings → Agents and choose a provider.", true},
		{"agent queue is already running", false},
		{"there are no queued stories to run", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := actionErrorNeedsAgentsSettings(tc.msg); got != tc.want {
			t.Fatalf("actionErrorNeedsAgentsSettings(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

func TestStartRunFailureRedirectsWithErrorInsteadOfJSON(t *testing.T) {
	app := testApp(t)
	seedProjectStories(t, app, "atlas", 1) // backlog only — nothing queued

	form := url.Values{
		"projectId": {"atlas"},
		"redirect":  {"/projects/atlas/run?new=1"},
	}
	req := httptest.NewRequest(http.MethodPost, "/agent/run-queue", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, req)

	if res.Code != http.StatusSeeOther {
		t.Fatalf("status = %d; body = %s", res.Code, res.Body.String())
	}
	if ct := res.Header().Get("Content-Type"); strings.Contains(ct, "json") {
		t.Fatalf("should not return JSON, content-type = %q body = %s", ct, res.Body.String())
	}
	loc := res.Header().Get("Location")
	decoded, _ := url.QueryUnescape(loc)
	if !strings.Contains(loc, "/projects/atlas/run") || !strings.Contains(decoded, "no queued stories") {
		t.Fatalf("location = %q", loc)
	}
}

func TestRunPageShowsAgentsSettingsLinkForToolingError(t *testing.T) {
	app := testApp(t)
	seedProjectStories(t, app, "atlas", 1)

	msg := "Reviewer not available: Codex CLI was not found. Set a path in Settings → Agents, or set RIPPLE_CODEX_BIN."
	path := "/projects/atlas/run?new=1&error=" + url.QueryEscape(msg)
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d", res.Code)
	}
	body := res.Body.String()
	if !strings.Contains(body, "Codex CLI was not found") {
		t.Fatal("run page should show the tooling error")
	}
	if !strings.Contains(body, `href="/settings#agents"`) {
		t.Fatal("run page should link to Settings → Agents")
	}
}

func TestStartRunHTMXFailureRendersAgentControlsWithSettingsLink(t *testing.T) {
	app := testApp(t)
	stories := seedProjectStories(t, app, "atlas", 1)
	if err := app.changeStoryStatus(context.Background(), stories[0].ID, StatusQueued, false, "queue"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.ExecContext(context.Background(), `UPDATE app_config SET reviewer_provider_id = 'missing_provider' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}

	form := url.Values{"projectId": {"atlas"}}
	req := httptest.NewRequest(http.MethodPost, "/agent/run-queue", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", res.Code, res.Body.String())
	}
	if res.Header().Get("HX-Retarget") != "#agent-run-controls" {
		t.Fatalf("HX-Retarget = %q", res.Header().Get("HX-Retarget"))
	}
	body := res.Body.String()
	if !strings.Contains(body, "id=\"agent-run-controls\"") {
		t.Fatal("expected agent controls HTML")
	}
	if !strings.Contains(body, "not available") && !strings.Contains(body, "not configured") && !strings.Contains(body, "CLI was not found") {
		t.Fatalf("expected agent tooling error, got %s", body)
	}
	if !strings.Contains(body, `href="/settings#agents"`) {
		t.Fatal("HTMX error controls should link to Settings → Agents")
	}
	if strings.Contains(body, `"error"`) && strings.HasPrefix(strings.TrimSpace(body), "{") {
		t.Fatal("should not return raw JSON error")
	}
}
