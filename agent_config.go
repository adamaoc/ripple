package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	ProviderKindCLI = "cli"

	ProviderIDCodexCLI = "codex_cli"
	ProviderIDGrokCLI  = "grok_cli"

	AgentRoleImplementer = "implementer"
	AgentRoleReviewer    = "reviewer"
)

// AgentProvider is a configured agent backend (CLI or API).
type AgentProvider struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	ConfigJSON string    `json:"configJson"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// CLIProviderConfig is stored in agent_providers.config_json for kind=cli.
type CLIProviderConfig struct {
	BinaryPath string `json:"binaryPath"`
}

// AppAgentConfig binds global implementer/reviewer roles to providers.
type AppAgentConfig struct {
	ImplementerProviderID string    `json:"implementerProviderId"`
	ReviewerProviderID    string    `json:"reviewerProviderId"`
	UpdatedAt             time.Time `json:"updatedAt"`
}

// AgentRoleResolved is the resolved provider for a role (CLI path and/or API config).
type AgentRoleResolved struct {
	Role         string
	ProviderID   string
	ProviderName string
	Kind         string
	BinaryPath   string
	API          APIProviderConfig
}

// ToolProbe is a status check shown on Settings → Agents.
type ToolProbe struct {
	Name    string
	Found   bool
	Path    string
	Version string
	Detail  string
}

// APIProviderView is a safe-for-UI view of an API provider (key masked).
type APIProviderView struct {
	ID        string
	Name      string
	BaseURL   string
	Model     string
	HasAPIKey bool
	MaskedKey string
	Selected  bool
}

// SettingsAgentsData powers the Agents section of the settings page.
type SettingsAgentsData struct {
	Providers             []AgentProvider
	ImplementerOptions    []AgentProvider
	ReviewerOptions       []AgentProvider
	APIProviders          []APIProviderView
	ImplementerProviderID string
	ReviewerProviderID    string
	CodexBinaryPath       string
	GrokBinaryPath        string
	Probes                []ToolProbe
	Saved                 bool
	Flash                 string
	PathPrecedence        string
	TestResult            string
	TestOK                bool
}

func (a *App) ensureAgentSettings(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS agent_providers (
			id TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			name TEXT NOT NULL,
			config_json TEXT NOT NULL DEFAULT '{}',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS app_config (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			implementer_provider_id TEXT NOT NULL DEFAULT 'codex_cli',
			reviewer_provider_id TEXT NOT NULL DEFAULT 'grok_cli',
			updated_at TEXT NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := a.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return a.seedDefaultAgentSettings(ctx)
}

func (a *App) seedDefaultAgentSettings(ctx context.Context) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	providers := []struct {
		id, kind, name string
	}{
		{ProviderIDCodexCLI, ProviderKindCLI, "Codex CLI"},
		{ProviderIDGrokCLI, ProviderKindCLI, "Grok CLI"},
	}
	for _, p := range providers {
		_, err := a.db.ExecContext(ctx, `INSERT OR IGNORE INTO agent_providers (id, kind, name, config_json, created_at, updated_at)
			VALUES (?, ?, ?, '{}', ?, ?)`, p.id, p.kind, p.name, now, now)
		if err != nil {
			return err
		}
	}
	_, err := a.db.ExecContext(ctx, `INSERT OR IGNORE INTO app_config (id, implementer_provider_id, reviewer_provider_id, updated_at)
		VALUES (1, ?, ?, ?)`, ProviderIDCodexCLI, ProviderIDGrokCLI, now)
	return err
}

func (a *App) listAgentProviders(ctx context.Context) ([]AgentProvider, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT id, kind, name, config_json, created_at, updated_at
		FROM agent_providers ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentProvider
	for rows.Next() {
		var p AgentProvider
		var created, updated string
		if err := rows.Scan(&p.ID, &p.Kind, &p.Name, &p.ConfigJSON, &created, &updated); err != nil {
			return nil, err
		}
		p.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		p.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (a *App) getAgentProvider(ctx context.Context, id string) (AgentProvider, error) {
	var p AgentProvider
	var created, updated string
	err := a.db.QueryRowContext(ctx, `SELECT id, kind, name, config_json, created_at, updated_at
		FROM agent_providers WHERE id = ?`, id).Scan(&p.ID, &p.Kind, &p.Name, &p.ConfigJSON, &created, &updated)
	if err == sql.ErrNoRows {
		return AgentProvider{}, badRequest("agent provider was not found")
	}
	if err != nil {
		return AgentProvider{}, err
	}
	p.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	p.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return p, nil
}

func (a *App) getAppAgentConfig(ctx context.Context) (AppAgentConfig, error) {
	var c AppAgentConfig
	var updated string
	err := a.db.QueryRowContext(ctx, `SELECT implementer_provider_id, reviewer_provider_id, updated_at
		FROM app_config WHERE id = 1`).Scan(&c.ImplementerProviderID, &c.ReviewerProviderID, &updated)
	if err == sql.ErrNoRows {
		if err := a.seedDefaultAgentSettings(ctx); err != nil {
			return AppAgentConfig{}, err
		}
		return a.getAppAgentConfig(ctx)
	}
	if err != nil {
		return AppAgentConfig{}, err
	}
	c.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	if c.ImplementerProviderID == "" {
		c.ImplementerProviderID = ProviderIDCodexCLI
	}
	if c.ReviewerProviderID == "" {
		c.ReviewerProviderID = ProviderIDGrokCLI
	}
	return c, nil
}

func parseCLIProviderConfig(configJSON string) CLIProviderConfig {
	var cfg CLIProviderConfig
	_ = json.Unmarshal([]byte(strings.TrimSpace(configJSON)), &cfg)
	cfg.BinaryPath = strings.TrimSpace(cfg.BinaryPath)
	return cfg
}

func (p AgentProvider) CLIConfig() CLIProviderConfig {
	return parseCLIProviderConfig(p.ConfigJSON)
}

func (a *App) updateAgentProviderCLIPath(ctx context.Context, id, binaryPath string) error {
	provider, err := a.getAgentProvider(ctx, id)
	if err != nil {
		return err
	}
	if provider.Kind != ProviderKindCLI {
		return badRequest("only CLI providers support binary path overrides")
	}
	cfg := provider.CLIConfig()
	cfg.BinaryPath = strings.TrimSpace(binaryPath)
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = a.db.ExecContext(ctx, `UPDATE agent_providers SET config_json = ?, updated_at = ? WHERE id = ?`,
		string(raw), now, id)
	return err
}

func (a *App) updateAppAgentRoles(ctx context.Context, implementerID, reviewerID string) error {
	implementerID = strings.TrimSpace(implementerID)
	reviewerID = strings.TrimSpace(reviewerID)
	if implementerID == "" {
		implementerID = ProviderIDCodexCLI
	}
	if reviewerID == "" {
		reviewerID = ProviderIDGrokCLI
	}
	impl, err := a.getAgentProvider(ctx, implementerID)
	if err != nil {
		return badRequest("implementer provider was not found")
	}
	rev, err := a.getAgentProvider(ctx, reviewerID)
	if err != nil {
		return badRequest("reviewer provider was not found")
	}
	if !impl.IsImplementerCapable() {
		return badRequest("Implementer must be a CLI provider that can write code (Codex CLI in v1). API providers cannot be implementers yet.")
	}
	if !rev.IsReviewerCapable() {
		return badRequest("Reviewer must be Grok CLI or an OpenAI-compatible API provider")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = a.db.ExecContext(ctx, `UPDATE app_config SET implementer_provider_id = ?, reviewer_provider_id = ?, updated_at = ? WHERE id = 1`,
		implementerID, reviewerID, now)
	return err
}

// resolveImplementer returns the configured implementer (CLI only in v1).
// Precedence for CLI path: env override > settings path > auto-detect.
func (a *App) resolveImplementer(ctx context.Context) (AgentRoleResolved, error) {
	cfg, err := a.getAppAgentConfig(ctx)
	if err != nil {
		return AgentRoleResolved{}, err
	}
	provider, err := a.getAgentProvider(ctx, cfg.ImplementerProviderID)
	if err != nil {
		return AgentRoleResolved{}, badRequest("Implementer is not configured. Open Settings → Agents and choose a provider.")
	}
	if !provider.IsImplementerCapable() {
		return AgentRoleResolved{}, badRequest("Implementer must be Codex CLI in this version")
	}
	path, err := resolveCodexBinaryWithSettings(provider.CLIConfig().BinaryPath)
	if err != nil {
		return AgentRoleResolved{}, badRequest("Implementer not available: " + err.Error())
	}
	return AgentRoleResolved{
		Role:         AgentRoleImplementer,
		ProviderID:   provider.ID,
		ProviderName: provider.Name,
		Kind:         provider.Kind,
		BinaryPath:   path,
	}, nil
}

// resolveReviewer returns the configured reviewer (Grok CLI or HTTP API).
// Precedence for CLI path: env override > settings path > auto-detect.
func (a *App) resolveReviewer(ctx context.Context) (AgentRoleResolved, error) {
	cfg, err := a.getAppAgentConfig(ctx)
	if err != nil {
		return AgentRoleResolved{}, err
	}
	provider, err := a.getAgentProvider(ctx, cfg.ReviewerProviderID)
	if err != nil {
		return AgentRoleResolved{}, badRequest("Reviewer is not configured. Open Settings → Agents and choose a provider.")
	}
	if !provider.IsReviewerCapable() {
		return AgentRoleResolved{}, badRequest("Reviewer must be Grok CLI or an OpenAI-compatible API provider")
	}
	resolved := AgentRoleResolved{
		Role:         AgentRoleReviewer,
		ProviderID:   provider.ID,
		ProviderName: provider.Name,
		Kind:         provider.Kind,
	}
	if provider.Kind == ProviderKindAPI {
		api := provider.APIConfig()
		if api.BaseURL == "" || api.Model == "" {
			return AgentRoleResolved{}, badRequest("Reviewer API provider is missing base URL or model. Open Settings → Agents.")
		}
		if api.APIKey == "" {
			return AgentRoleResolved{}, badRequest("Reviewer API provider has no API key. Open Settings → Agents.")
		}
		resolved.API = api
		return resolved, nil
	}
	path, err := resolveGrokBinaryWithSettings(provider.CLIConfig().BinaryPath)
	if err != nil {
		return AgentRoleResolved{}, badRequest("Reviewer not available: " + err.Error())
	}
	resolved.BinaryPath = path
	return resolved, nil
}

func (a *App) newImplementerRunner(ctx context.Context) (AgentRunner, error) {
	resolved, err := a.resolveImplementer(ctx)
	if err != nil {
		return nil, err
	}
	return &CodexCLIRunner{
		BinaryPath:   resolved.BinaryPath,
		providerID:   resolved.ProviderID,
		providerName: resolved.ProviderName,
	}, nil
}

func (a *App) newReviewerRunner(ctx context.Context) (AgentRunner, error) {
	resolved, err := a.resolveReviewer(ctx)
	if err != nil {
		return nil, err
	}
	if resolved.Kind == ProviderKindAPI {
		return &HTTPAPIRunner{
			providerID:   resolved.ProviderID,
			providerName: resolved.ProviderName,
			Config:       resolved.API,
		}, nil
	}
	return &GrokCLIRunner{
		BinaryPath:   resolved.BinaryPath,
		providerID:   resolved.ProviderID,
		providerName: resolved.ProviderName,
	}, nil
}

func newAPIProviderID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("api_%d", time.Now().UnixNano())
	}
	return "api_" + hex.EncodeToString(b[:])
}

func (a *App) createAPIProvider(ctx context.Context, name, baseURL, apiKey, model string) (AgentProvider, error) {
	name = strings.TrimSpace(name)
	baseURL = strings.TrimSpace(baseURL)
	apiKey = strings.TrimSpace(apiKey)
	model = strings.TrimSpace(model)
	if name == "" {
		return AgentProvider{}, badRequest("API provider name is required")
	}
	if baseURL == "" {
		return AgentProvider{}, badRequest("API base URL is required")
	}
	if apiKey == "" {
		return AgentProvider{}, badRequest("API key is required")
	}
	if model == "" {
		return AgentProvider{}, badRequest("Model is required")
	}
	cfg := APIProviderConfig{BaseURL: baseURL, APIKey: apiKey, Model: model}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return AgentProvider{}, err
	}
	id := newAPIProviderID()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = a.db.ExecContext(ctx, `INSERT INTO agent_providers (id, kind, name, config_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`, id, ProviderKindAPI, name, string(raw), now, now)
	if err != nil {
		return AgentProvider{}, err
	}
	return a.getAgentProvider(ctx, id)
}

func (a *App) updateAPIProvider(ctx context.Context, id, name, baseURL, apiKey, model string) error {
	provider, err := a.getAgentProvider(ctx, id)
	if err != nil {
		return err
	}
	if provider.Kind != ProviderKindAPI {
		return badRequest("provider is not an API integration")
	}
	cfg := provider.APIConfig()
	if u := strings.TrimSpace(baseURL); u != "" {
		cfg.BaseURL = u
	}
	if m := strings.TrimSpace(model); m != "" {
		cfg.Model = m
	}
	// Empty apiKey means keep existing key.
	if k := strings.TrimSpace(apiKey); k != "" {
		cfg.APIKey = k
	}
	if cfg.BaseURL == "" || cfg.Model == "" {
		return badRequest("API base URL and model are required")
	}
	if cfg.APIKey == "" {
		return badRequest("API key is required")
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	displayName := strings.TrimSpace(name)
	if displayName == "" {
		displayName = provider.Name
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = a.db.ExecContext(ctx, `UPDATE agent_providers SET name = ?, config_json = ?, updated_at = ? WHERE id = ?`,
		displayName, string(raw), now, id)
	return err
}

func (a *App) deleteAPIProvider(ctx context.Context, id string) error {
	provider, err := a.getAgentProvider(ctx, id)
	if err != nil {
		return err
	}
	if provider.Kind != ProviderKindAPI {
		return badRequest("only API providers can be deleted")
	}
	cfg, err := a.getAppAgentConfig(ctx)
	if err != nil {
		return err
	}
	if cfg.ReviewerProviderID == id {
		if err := a.updateAppAgentRoles(ctx, cfg.ImplementerProviderID, ProviderIDGrokCLI); err != nil {
			return err
		}
	}
	if cfg.ImplementerProviderID == id {
		return badRequest("cannot delete the active implementer provider")
	}
	_, err = a.db.ExecContext(ctx, `DELETE FROM agent_providers WHERE id = ? AND kind = ?`, id, ProviderKindAPI)
	return err
}

func (a *App) listAPISecrets(ctx context.Context) ([]string, error) {
	providers, err := a.listAgentProviders(ctx)
	if err != nil {
		return nil, err
	}
	var secrets []string
	for _, p := range providers {
		if p.Kind != ProviderKindAPI {
			continue
		}
		if key := strings.TrimSpace(p.APIConfig().APIKey); key != "" {
			secrets = append(secrets, key)
		}
	}
	return secrets, nil
}

func (a *App) redactKnownSecrets(err error) error {
	if err == nil {
		return nil
	}
	secrets, listErr := a.listAPISecrets(context.Background())
	if listErr != nil {
		return err
	}
	return redactSecrets(err, secrets...)
}

// testAPIProviderConnection sends a minimal chat completion request.
func (a *App) testAPIProviderConnection(ctx context.Context, id string) error {
	provider, err := a.getAgentProvider(ctx, id)
	if err != nil {
		return err
	}
	if provider.Kind != ProviderKindAPI {
		return badRequest("only API providers support connection tests")
	}
	runner := &HTTPAPIRunner{
		providerID:   provider.ID,
		providerName: provider.Name,
		Config:       provider.APIConfig(),
		Client:       &http.Client{Timeout: 20 * time.Second},
	}
	result, err := runner.Run(ctx, AgentRunRequest{
		Role:   AgentRunRoleReview,
		Prompt: `Respond with JSON only: {"approved":true,"summary":"ok","comments":[]}`,
	})
	if err != nil {
		return a.redactKnownSecrets(err)
	}
	if _, err := parseAgentReview(result.FinalMessage); err != nil {
		// Connection worked even if JSON shape is loose; accept non-empty content.
		if strings.TrimSpace(result.FinalMessage) == "" && strings.TrimSpace(result.Stdout) == "" {
			return badRequest("provider returned an empty response")
		}
	}
	return nil
}

func resolveCodexBinary() (string, error) {
	return resolveCodexBinaryWithSettings("")
}

func resolveGrokBinary() (string, error) {
	return resolveGrokBinaryWithSettings("")
}

// resolveCodexBinaryWithSettings uses env > settings path > auto-detect.
func resolveCodexBinaryWithSettings(settingsPath string) (string, error) {
	candidates := []string{}
	if configured := firstEnv("RIPPLE_CODEX_BIN", "TASKMANAGER_CODEX_BIN"); configured != "" {
		candidates = append(candidates, configured)
	}
	if path := strings.TrimSpace(settingsPath); path != "" {
		candidates = append(candidates, path)
	}
	candidates = append(candidates,
		"codex",
		"/opt/homebrew/bin/codex",
		"/usr/local/bin/codex",
		"/Applications/Codex.app/Contents/Resources/codex",
	)
	if path, ok := firstUsableBinary(candidates); ok {
		return path, nil
	}
	return "", badRequest("Codex CLI was not found. Set a path in Settings → Agents, or set RIPPLE_CODEX_BIN (env overrides settings).")
}

// resolveGrokBinaryWithSettings uses env > settings path > auto-detect.
func resolveGrokBinaryWithSettings(settingsPath string) (string, error) {
	candidates := []string{}
	if configured := firstEnv("RIPPLE_GROK_BIN", "TASKMANAGER_GROK_BIN"); configured != "" {
		candidates = append(candidates, configured)
	}
	if path := strings.TrimSpace(settingsPath); path != "" {
		candidates = append(candidates, path)
	}
	candidates = append(candidates,
		"grok",
		filepath.Join(os.Getenv("HOME"), ".grok", "bin", "grok"),
		"/opt/homebrew/bin/grok",
		"/usr/local/bin/grok",
	)
	if path, ok := firstUsableBinary(candidates); ok {
		return path, nil
	}
	return "", badRequest("Grok CLI was not found. Set a path in Settings → Agents, or set RIPPLE_GROK_BIN (env overrides settings).")
}

func firstUsableBinary(candidates []string) (string, bool) {
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if strings.ContainsRune(candidate, filepath.Separator) {
			expanded, err := expandUserPath(candidate)
			if err != nil {
				continue
			}
			info, err := os.Stat(expanded)
			if err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
				return expanded, true
			}
			continue
		}
		if path, err := osexec.LookPath(candidate); err == nil {
			return path, true
		}
	}
	return "", false
}

func (a *App) settingsAgentsData(ctx context.Context, flash string) (SettingsAgentsData, error) {
	providers, err := a.listAgentProviders(ctx)
	if err != nil {
		return SettingsAgentsData{}, err
	}
	cfg, err := a.getAppAgentConfig(ctx)
	if err != nil {
		return SettingsAgentsData{}, err
	}
	var codexPath, grokPath string
	var implementerOpts, reviewerOpts []AgentProvider
	var apiViews []APIProviderView
	for _, p := range providers {
		switch p.ID {
		case ProviderIDCodexCLI:
			codexPath = p.CLIConfig().BinaryPath
		case ProviderIDGrokCLI:
			grokPath = p.CLIConfig().BinaryPath
		}
		if p.IsImplementerCapable() {
			implementerOpts = append(implementerOpts, p)
		}
		if p.IsReviewerCapable() {
			reviewerOpts = append(reviewerOpts, p)
		}
		if p.Kind == ProviderKindAPI {
			api := p.APIConfig()
			apiViews = append(apiViews, APIProviderView{
				ID:        p.ID,
				Name:      p.Name,
				BaseURL:   api.BaseURL,
				Model:     api.Model,
				HasAPIKey: p.HasAPIKey(),
				MaskedKey: p.MaskedAPIKey(),
				Selected:  p.ID == cfg.ReviewerProviderID,
			})
		}
	}
	data := SettingsAgentsData{
		Providers:             providers,
		ImplementerOptions:    implementerOpts,
		ReviewerOptions:       reviewerOpts,
		APIProviders:          apiViews,
		ImplementerProviderID: cfg.ImplementerProviderID,
		ReviewerProviderID:    cfg.ReviewerProviderID,
		CodexBinaryPath:       codexPath,
		GrokBinaryPath:        grokPath,
		Probes:                a.probeAgentTools(ctx, codexPath, grokPath, cfg.ReviewerProviderID),
		Saved:                 flash == "saved" || flash == "api_saved" || flash == "api_deleted" || flash == "api_tested",
		Flash:                 flash,
		PathPrecedence:        "env override > settings path > auto-detect",
	}
	switch flash {
	case "api_saved":
		data.TestResult = "API provider saved."
		data.TestOK = true
	case "api_deleted":
		data.TestResult = "API provider deleted."
		data.TestOK = true
	case "api_tested":
		data.TestResult = "Connection test succeeded."
		data.TestOK = true
	case "api_test_failed":
		data.TestResult = "Connection test failed. Check base URL, model, and API key."
		data.TestOK = false
	}
	return data, nil
}

func (a *App) probeAgentTools(ctx context.Context, codexSettingsPath, grokSettingsPath, reviewerID string) []ToolProbe {
	probes := make([]ToolProbe, 0, 4)

	codexPath, codexErr := resolveCodexBinaryWithSettings(codexSettingsPath)
	probes = append(probes, probeCLITool("Codex (implementer)", codexPath, codexErr))

	grokPath, grokErr := resolveGrokBinaryWithSettings(grokSettingsPath)
	probes = append(probes, probeCLITool("Grok CLI", grokPath, grokErr))

	ghPath, ghErr := resolveGhBinary()
	probes = append(probes, probeCLITool("GitHub CLI (gh)", ghPath, ghErr))

	if strings.TrimSpace(reviewerID) != "" {
		if rev, err := a.resolveReviewer(ctx); err == nil {
			if rev.Kind == ProviderKindAPI {
				probes = append(probes, ToolProbe{
					Name:   "Active reviewer (API)",
					Found:  true,
					Path:   rev.API.BaseURL,
					Detail: rev.ProviderName + " · " + rev.API.Model,
				})
			} else {
				probes = append(probes, ToolProbe{
					Name:   "Active reviewer (CLI)",
					Found:  true,
					Path:   rev.BinaryPath,
					Detail: rev.ProviderName,
				})
			}
		} else {
			probes = append(probes, ToolProbe{Name: "Active reviewer", Found: false, Detail: err.Error()})
		}
	}

	return probes
}

func probeCLITool(name, path string, resolveErr error) ToolProbe {
	if resolveErr != nil || path == "" {
		detail := "Not found"
		if resolveErr != nil {
			detail = resolveErr.Error()
		}
		return ToolProbe{Name: name, Found: false, Detail: detail}
	}
	version := probeBinaryVersion(path)
	return ToolProbe{
		Name:    name,
		Found:   true,
		Path:    path,
		Version: version,
		Detail:  "Found",
	}
}

func probeBinaryVersion(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := osexec.CommandContext(ctx, path, "--version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Some tools use -v
		cmd = osexec.CommandContext(ctx, path, "-v")
		out, err = cmd.CombinedOutput()
		if err != nil {
			return ""
		}
	}
	line := strings.TrimSpace(string(out))
	if idx := strings.IndexByte(line, '\n'); idx >= 0 {
		line = strings.TrimSpace(line[:idx])
	}
	return truncate(line, 120)
}

// saveAgentSettingsFromForm updates role bindings and CLI path overrides.
func (a *App) saveAgentSettingsFromForm(ctx context.Context, implementerID, reviewerID, codexPath, grokPath string) error {
	codexPath = strings.TrimSpace(codexPath)
	grokPath = strings.TrimSpace(grokPath)
	// Validate explicit paths before persisting. Do not fall through to auto-detect here —
	// the user typed this path and it should work as-is.
	if codexPath != "" {
		if err := validateExecutablePath(codexPath); err != nil {
			return badRequest(fmt.Sprintf("Codex path could not be used: %s", err.Error()))
		}
	}
	if grokPath != "" {
		if err := validateExecutablePath(grokPath); err != nil {
			return badRequest(fmt.Sprintf("Grok path could not be used: %s", err.Error()))
		}
	}
	if err := a.updateAppAgentRoles(ctx, implementerID, reviewerID); err != nil {
		return err
	}
	if err := a.updateAgentProviderCLIPath(ctx, ProviderIDCodexCLI, codexPath); err != nil {
		return err
	}
	return a.updateAgentProviderCLIPath(ctx, ProviderIDGrokCLI, grokPath)
}

func validateExecutablePath(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("path is empty")
	}
	expanded, err := expandUserPath(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(expanded)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("path is a directory")
	}
	if info.Mode()&0111 == 0 {
		return fmt.Errorf("path is not executable")
	}
	return nil
}
