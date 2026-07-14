package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentSettingsDefaultSeed(t *testing.T) {
	app := testApp(t)
	providers, err := app.listAgentProviders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) < 2 {
		t.Fatalf("expected seeded providers, got %d", len(providers))
	}
	ids := map[string]bool{}
	for _, p := range providers {
		ids[p.ID] = true
		if p.Kind != ProviderKindCLI {
			t.Fatalf("provider %s kind = %q", p.ID, p.Kind)
		}
	}
	if !ids[ProviderIDCodexCLI] || !ids[ProviderIDGrokCLI] {
		t.Fatalf("missing default providers: %#v", ids)
	}
	cfg, err := app.getAppAgentConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ImplementerProviderID != ProviderIDCodexCLI {
		t.Fatalf("implementer = %q, want codex_cli", cfg.ImplementerProviderID)
	}
	if cfg.ReviewerProviderID != ProviderIDGrokCLI {
		t.Fatalf("reviewer = %q, want grok_cli", cfg.ReviewerProviderID)
	}
}

func TestAgentSettingsUpdateChangesResolution(t *testing.T) {
	app := testApp(t)
	dir := t.TempDir()
	codexBin := filepath.Join(dir, "fake-codex")
	grokBin := filepath.Join(dir, "fake-grok")
	writeExecutable(t, codexBin, "#!/bin/sh\necho codex-test 1.0\n")
	writeExecutable(t, grokBin, "#!/bin/sh\necho grok-test 1.0\n")

	// Clear env overrides for this test.
	t.Setenv("RIPPLE_CODEX_BIN", "")
	t.Setenv("TASKMANAGER_CODEX_BIN", "")
	t.Setenv("RIPPLE_GROK_BIN", "")
	t.Setenv("TASKMANAGER_GROK_BIN", "")

	if err := app.saveAgentSettingsFromForm(context.Background(), ProviderIDCodexCLI, ProviderIDGrokCLI, codexBin, grokBin); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	impl, err := app.resolveImplementer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if impl.BinaryPath != codexBin {
		t.Fatalf("implementer path = %q, want %q", impl.BinaryPath, codexBin)
	}
	rev, err := app.resolveReviewer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rev.BinaryPath != grokBin {
		t.Fatalf("reviewer path = %q, want %q", rev.BinaryPath, grokBin)
	}

	// Round-trip via HTTP POST.
	form := url.Values{
		"implementerProviderId": {ProviderIDCodexCLI},
		"reviewerProviderId":    {ProviderIDGrokCLI},
		"codexBinaryPath":       {codexBin},
		"grokBinaryPath":        {grokBin},
	}
	req := httptest.NewRequest(http.MethodPost, "/settings/agents", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, req)
	if res.Code != http.StatusSeeOther {
		t.Fatalf("status = %d; body = %s", res.Code, res.Body.String())
	}
	if loc := res.Header().Get("Location"); !strings.Contains(loc, "flash=saved") {
		t.Fatalf("redirect location = %q", loc)
	}

	getRes := httptest.NewRecorder()
	app.routes().ServeHTTP(getRes, httptest.NewRequest(http.MethodGet, "/settings?flash=saved", nil))
	if getRes.Code != http.StatusOK {
		t.Fatalf("get settings status = %d", getRes.Code)
	}
	body := getRes.Body.String()
	if !strings.Contains(body, "settings-flash") || !strings.Contains(body, "Saved") {
		t.Fatal("expected saved flash on settings page")
	}
	if !strings.Contains(body, codexBin) {
		t.Fatal("settings page should show saved codex path")
	}
}

func TestAgentBinaryEnvOverrideWinsOverSettings(t *testing.T) {
	app := testApp(t)
	dir := t.TempDir()
	settingsCodex := filepath.Join(dir, "settings-codex")
	envCodex := filepath.Join(dir, "env-codex")
	settingsGrok := filepath.Join(dir, "settings-grok")
	envGrok := filepath.Join(dir, "env-grok")
	writeExecutable(t, settingsCodex, "#!/bin/sh\necho settings-codex\n")
	writeExecutable(t, envCodex, "#!/bin/sh\necho env-codex\n")
	writeExecutable(t, settingsGrok, "#!/bin/sh\necho settings-grok\n")
	writeExecutable(t, envGrok, "#!/bin/sh\necho env-grok\n")

	if err := app.saveAgentSettingsFromForm(context.Background(), ProviderIDCodexCLI, ProviderIDGrokCLI, settingsCodex, settingsGrok); err != nil {
		t.Fatal(err)
	}

	t.Setenv("RIPPLE_CODEX_BIN", envCodex)
	t.Setenv("RIPPLE_GROK_BIN", envGrok)
	t.Setenv("TASKMANAGER_CODEX_BIN", "")
	t.Setenv("TASKMANAGER_GROK_BIN", "")

	impl, err := app.resolveImplementer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if impl.BinaryPath != envCodex {
		t.Fatalf("implementer path = %q, want env override %q", impl.BinaryPath, envCodex)
	}
	rev, err := app.resolveReviewer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rev.BinaryPath != envGrok {
		t.Fatalf("reviewer path = %q, want env override %q", rev.BinaryPath, envGrok)
	}

	// Direct helper also respects precedence.
	path, err := resolveCodexBinaryWithSettings(settingsCodex)
	if err != nil {
		t.Fatal(err)
	}
	if path != envCodex {
		t.Fatalf("resolveCodexBinaryWithSettings = %q, want %q", path, envCodex)
	}
}

func TestResolveCodexUsesSettingsPathWhenNoEnv(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex-from-settings")
	writeExecutable(t, bin, "#!/bin/sh\necho ok\n")
	t.Setenv("RIPPLE_CODEX_BIN", "")
	t.Setenv("TASKMANAGER_CODEX_BIN", "")

	path, err := resolveCodexBinaryWithSettings(bin)
	if err != nil {
		t.Fatal(err)
	}
	if path != bin {
		t.Fatalf("path = %q, want %q", path, bin)
	}
}

func TestSaveAgentSettingsRejectsMissingBinaryPath(t *testing.T) {
	app := testApp(t)
	t.Setenv("RIPPLE_CODEX_BIN", "")
	t.Setenv("TASKMANAGER_CODEX_BIN", "")
	err := app.saveAgentSettingsFromForm(context.Background(), ProviderIDCodexCLI, ProviderIDGrokCLI, "/no/such/codex-binary", "")
	if err == nil {
		t.Fatal("expected error for missing codex path")
	}
	if !strings.Contains(err.Error(), "Codex path") {
		t.Fatalf("error = %v", err)
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}
