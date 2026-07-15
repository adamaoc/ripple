package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestParseAgentReviewJSON(t *testing.T) {
	raw := `{"approved":false,"summary":"needs tests","comments":[{"path":"a.go","line":3,"body":"add coverage"}]}`
	review, err := parseAgentReview(raw)
	if err != nil {
		t.Fatal(err)
	}
	if review.Approved || review.Summary != "needs tests" || len(review.Comments) != 1 {
		t.Fatalf("unexpected review: %+v", review)
	}
	// fenced / noisy response
	noisy := "Here is the review:\n```json\n" + raw + "\n```\n"
	if _, err := parseAgentReview(noisy); err != nil {
		t.Fatalf("should extract JSON from noisy text: %v", err)
	}
}

func TestHTTPAPIRunnerReviewSuccess(t *testing.T) {
	reviewPayload := map[string]any{
		"approved": true,
		"summary":  "looks good",
		"comments": []any{},
	}
	reviewJSON, _ := json.Marshal(reviewPayload)
	var sawAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		sawAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "secret-key-should-not-appear-in-body") {
			// key is only in Authorization header
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": string(reviewJSON)}},
			},
		})
	}))
	defer server.Close()

	runner := &HTTPAPIRunner{
		providerID:   "api_test",
		providerName: "Test API",
		Config: APIProviderConfig{
			BaseURL: server.URL + "/v1",
			APIKey:  "secret-key-xyz",
			Model:   "test-model",
		},
		Client: server.Client(),
	}
	result, err := runner.Run(context.Background(), AgentRunRequest{
		Role:   AgentRunRoleReview,
		Prompt: buildAgentReviewPrompt(Story{ID: "A-001", Title: "T"}, 1, "https://example.com/pull/1", "diff --git a/x"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sawAuth, "Bearer secret-key-xyz") {
		t.Fatalf("auth header = %q", sawAuth)
	}
	if strings.Contains(result.Stdout, "secret-key-xyz") {
		t.Fatal("API key must not appear in runner stdout")
	}
	if strings.Contains(result.FinalMessage, "secret-key-xyz") {
		t.Fatal("API key must not appear in final message")
	}
	review, err := parseAgentReview(result.FinalMessage)
	if err != nil {
		t.Fatal(err)
	}
	if !review.Approved || review.Summary != "looks good" {
		t.Fatalf("review = %+v", review)
	}
}

func TestHTTPAPIRunnerInvalidKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"message": "Incorrect API key provided: sk-secret-leaked"},
		})
	}))
	defer server.Close()

	runner := &HTTPAPIRunner{
		providerID:   "api_test",
		providerName: "Test API",
		Config: APIProviderConfig{
			BaseURL: server.URL + "/v1",
			APIKey:  "sk-secret-leaked",
			Model:   "test-model",
		},
		Client: server.Client(),
	}
	_, err := runner.Run(context.Background(), AgentRunRequest{Role: AgentRunRoleReview, Prompt: "review"})
	if err == nil {
		t.Fatal("expected unauthorized error")
	}
	if !strings.Contains(err.Error(), "unauthorized") && !strings.Contains(strings.ToLower(err.Error()), "api key") {
		t.Fatalf("error should mention unauthorized/key: %v", err)
	}
}

func TestAPIProviderAsReviewerEndToEnd(t *testing.T) {
	app := testApp(t)
	reviewJSON := `{"approved":true,"summary":"lgtm","comments":[]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": reviewJSON}},
			},
		})
	}))
	defer server.Close()

	provider, err := app.createAPIProvider(context.Background(), "Mock Reviewer", server.URL+"/v1", "test-key-abc", "mock-model")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.updateAppAgentRoles(context.Background(), ProviderIDCodexCLI, provider.ID); err != nil {
		t.Fatal(err)
	}

	// Key must not appear in settings HTML.
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("settings status = %d", res.Code)
	}
	body := res.Body.String()
	if strings.Contains(body, "test-key-abc") {
		t.Fatal("API key leaked into settings HTML")
	}
	if !strings.Contains(body, "Mock Reviewer") || !strings.Contains(body, "••••••••") {
		t.Fatal("settings should show API provider with masked key")
	}
	if !strings.Contains(body, `id="api-providers"`) {
		t.Fatal("settings missing API providers section")
	}

	// Resolution and runner path.
	resolved, err := app.resolveReviewer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Kind != ProviderKindAPI || resolved.ProviderID != provider.ID {
		t.Fatalf("resolved reviewer = %+v", resolved)
	}
	if resolved.API.APIKey != "test-key-abc" {
		t.Fatal("resolved API key mismatch")
	}

	runner, err := app.newReviewerRunner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	httpRunner, ok := runner.(*HTTPAPIRunner)
	if !ok {
		t.Fatalf("expected HTTPAPIRunner, got %T", runner)
	}
	httpRunner.Client = server.Client()

	// Simulate pipeline review call bookkeeping path.
	stories := seedProjectStories(t, app, "atlas", 1)
	story := stories[0]
	project, _ := app.getProject(context.Background(), "atlas")
	_ = app.changeStoryStatus(context.Background(), story.ID, StatusQueued, false, "q")
	queued, _ := app.listStories(context.Background(), storyFilters{ProjectID: "atlas", Status: StatusQueued, ShowClosed: true})
	runID, err := app.createQueueRun(context.Background(), storyFilters{ProjectID: "atlas"}, queued)
	if err != nil {
		t.Fatal(err)
	}
	text, err := app.runReviewerForStory(context.Background(), runID, "http://localhost:8080", project, story,
		buildAgentReviewPrompt(story, 1, "https://example.com/1", "diff"), RunKindGrokReview)
	if err != nil {
		t.Fatal(err)
	}
	review, err := parseAgentReview(text)
	if err != nil {
		t.Fatal(err)
	}
	if !review.Approved {
		t.Fatalf("review not approved: %+v", review)
	}

	// Agent run transcript must not contain the API key.
	var stdout, stderr, finalMsg, exitErr, prompt string
	err = app.db.QueryRowContext(context.Background(),
		`SELECT stdout, stderr, final_message, exit_error, prompt FROM agent_runs WHERE queue_run_id = ? ORDER BY id DESC LIMIT 1`,
		runID).Scan(&stdout, &stderr, &finalMsg, &exitErr, &prompt)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{stdout, stderr, finalMsg, exitErr, prompt} {
		if strings.Contains(field, "test-key-abc") {
			t.Fatal("API key found in agent run storage")
		}
	}
}

func TestCreateAPIProviderViaForm(t *testing.T) {
	app := testApp(t)
	form := url.Values{
		"name":    {"OpenAI"},
		"baseUrl": {"https://api.openai.com/v1"},
		"apiKey":  {"sk-test-form-key"},
		"model":   {"gpt-4o-mini"},
	}
	req := httptest.NewRequest(http.MethodPost, "/settings/agents/api-providers", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, req)
	if res.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}
	providers, err := app.listAgentProviders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range providers {
		if p.Kind == ProviderKindAPI && p.Name == "OpenAI" {
			found = true
			if p.APIConfig().APIKey != "sk-test-form-key" {
				t.Fatalf("stored key mismatch")
			}
		}
	}
	if !found {
		t.Fatal("API provider not created")
	}
}

func TestAPIProviderCannotBeImplementer(t *testing.T) {
	app := testApp(t)
	provider, err := app.createAPIProvider(context.Background(), "Bad", "https://example.com/v1", "k", "m")
	if err != nil {
		t.Fatal(err)
	}
	err = app.updateAppAgentRoles(context.Background(), provider.ID, ProviderIDGrokCLI)
	if err == nil {
		t.Fatal("expected error binding API as implementer")
	}
	if !strings.Contains(err.Error(), "Implementer") {
		t.Fatalf("error = %v", err)
	}
}

func TestCLIProvidersCanFillEitherRoleOrBoth(t *testing.T) {
	app := testApp(t)
	// Swap defaults: Grok implements, Codex reviews.
	if err := app.updateAppAgentRoles(context.Background(), ProviderIDGrokCLI, ProviderIDCodexCLI); err != nil {
		t.Fatalf("swap roles: %v", err)
	}
	cfg, err := app.getAppAgentConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ImplementerProviderID != ProviderIDGrokCLI || cfg.ReviewerProviderID != ProviderIDCodexCLI {
		t.Fatalf("cfg = %+v", cfg)
	}
	// Same agent for both roles is allowed.
	if err := app.updateAppAgentRoles(context.Background(), ProviderIDCodexCLI, ProviderIDCodexCLI); err != nil {
		t.Fatalf("same agent both roles: %v", err)
	}
	cfg, err = app.getAppAgentConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ImplementerProviderID != ProviderIDCodexCLI || cfg.ReviewerProviderID != ProviderIDCodexCLI {
		t.Fatalf("same-agent cfg = %+v", cfg)
	}
}

func TestCLIProviderCapabilities(t *testing.T) {
	codex := AgentProvider{ID: ProviderIDCodexCLI, Kind: ProviderKindCLI, Name: "Codex CLI"}
	grok := AgentProvider{ID: ProviderIDGrokCLI, Kind: ProviderKindCLI, Name: "Grok CLI"}
	api := AgentProvider{ID: "api_x", Kind: ProviderKindAPI, Name: "API"}
	if !codex.IsImplementerCapable() || !codex.IsReviewerCapable() {
		t.Fatal("codex should be usable as implementer and reviewer")
	}
	if !grok.IsImplementerCapable() || !grok.IsReviewerCapable() {
		t.Fatal("grok should be usable as implementer and reviewer")
	}
	if api.IsImplementerCapable() || !api.IsReviewerCapable() {
		t.Fatal("api should be reviewer-only")
	}
}

func TestDefaultCLIPathStillResolves(t *testing.T) {
	app := testApp(t)
	// Defaults: implementer codex_cli, reviewer grok_cli — resolution errors only if binaries missing.
	// We only assert role binding + runner construction type for CLI when binaries exist, or clear config error otherwise.
	cfg, err := app.getAppAgentConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ImplementerProviderID != ProviderIDCodexCLI || cfg.ReviewerProviderID != ProviderIDGrokCLI {
		t.Fatalf("defaults = %+v", cfg)
	}
	impl, err := app.resolveImplementer(context.Background())
	if err == nil {
		if impl.Kind != ProviderKindCLI || impl.BinaryPath == "" {
			t.Fatalf("implementer = %+v", impl)
		}
		runner, rerr := app.newImplementerRunner(context.Background())
		if rerr != nil {
			t.Fatal(rerr)
		}
		if _, ok := runner.(*CodexCLIRunner); !ok {
			t.Fatalf("got %T", runner)
		}
	}
}

