package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	AgentRunRoleImplement = "implement"
	AgentRunRoleReview    = "review"
	AgentRunRoleFix       = "fix"

	ProviderKindAPI = "api"
)

// AgentRunner is the shared call path for CLI and HTTP agents.
type AgentRunner interface {
	ProviderID() string
	ProviderName() string
	Kind() string
	Run(ctx context.Context, req AgentRunRequest) (AgentRunResult, error)
}

// AgentRunRequest is a single implement / review / fix invocation.
type AgentRunRequest struct {
	Role             string
	Prompt           string
	WorkingDir       string
	BaseURL          string
	StoryID          string
	FinalMessagePath string // used by Codex CLI for --output-last-message
	Stdout           io.Writer
	Stderr           io.Writer
}

// AgentRunResult is the durable outcome of a runner invocation.
type AgentRunResult struct {
	FinalMessage string
	Stdout       string
	Stderr       string
}

// APIProviderConfig is stored in agent_providers.config_json for kind=api.
// Never log APIKey or write it to story events / transcripts.
type APIProviderConfig struct {
	BaseURL string            `json:"baseUrl"`
	APIKey  string            `json:"apiKey"`
	Model   string            `json:"model"`
	Headers map[string]string `json:"headers,omitempty"`
}

// AgentReview is the structured review contract shared by CLI and API reviewers.
type AgentReviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Body string `json:"body"`
}

type AgentReview struct {
	Approved bool                 `json:"approved"`
	Summary  string               `json:"summary"`
	Comments []AgentReviewComment `json:"comments"`
}

// GrokReview is kept as an alias so existing pipeline code continues to compile.
type GrokReviewComment = AgentReviewComment
type GrokReview = AgentReview

// CodexCLIRunner runs `codex exec` for implement/fix roles.
type CodexCLIRunner struct {
	BinaryPath   string
	providerID   string
	providerName string
}

func (r *CodexCLIRunner) ProviderID() string   { return r.providerID }
func (r *CodexCLIRunner) ProviderName() string { return r.providerName }
func (r *CodexCLIRunner) Kind() string         { return ProviderKindCLI }

func (r *CodexCLIRunner) Run(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
	if r.BinaryPath == "" {
		return AgentRunResult{}, badRequest("Codex binary path is empty")
	}
	finalPath := req.FinalMessagePath
	if finalPath == "" {
		runDir := filepath.Join(os.TempDir(), "ripple", "runs")
		if err := os.MkdirAll(runDir, 0755); err != nil {
			return AgentRunResult{}, err
		}
		finalPath = filepath.Join(runDir, req.StoryID+"-codex-final.md")
	}
	args := []string{
		"exec",
		"--cd", req.WorkingDir,
		"--sandbox", "workspace-write",
		"-c", `approval_policy="never"`,
		"--json",
		"--output-last-message", finalPath,
	}
	if !isGitWorkTree(req.WorkingDir) {
		args = append(args, "--skip-git-repo-check")
	}
	args = append(args, req.Prompt)

	var stdoutBuf, stderrBuf bytes.Buffer
	stdoutW := multiWriter(req.Stdout, &stdoutBuf)
	stderrW := multiWriter(req.Stderr, &stderrBuf)

	cmd := osexec.CommandContext(ctx, r.BinaryPath, args...)
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	cmd.Env = append(os.Environ(),
		"RIPPLE_BASE_URL="+req.BaseURL,
		"RIPPLE_STORY_ID="+req.StoryID,
		"TASKMANAGER_BASE_URL="+req.BaseURL,
		"TASKMANAGER_STORY_ID="+req.StoryID,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err := runCmdWithCancel(ctx, cmd)
	result := AgentRunResult{Stdout: stdoutBuf.String(), Stderr: stderrBuf.String()}
	if err != nil {
		return result, err
	}
	final, readErr := os.ReadFile(finalPath)
	if readErr != nil {
		result.FinalMessage = strings.TrimSpace(result.Stdout)
		return result, nil
	}
	result.FinalMessage = strings.TrimSpace(string(final))
	return result, nil
}

// GrokCLIRunner runs the Grok CLI headless for PR review.
type GrokCLIRunner struct {
	BinaryPath   string
	providerID   string
	providerName string
}

func (r *GrokCLIRunner) ProviderID() string   { return r.providerID }
func (r *GrokCLIRunner) ProviderName() string { return r.providerName }
func (r *GrokCLIRunner) Kind() string         { return ProviderKindCLI }

func (r *GrokCLIRunner) Run(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
	if r.BinaryPath == "" {
		return AgentRunResult{}, badRequest("Grok binary path is empty")
	}
	args := []string{
		"-p", req.Prompt,
		"--cwd", req.WorkingDir,
		"--sandbox", "workspace",
		"--always-approve",
		"--output-format", "json",
		"--no-auto-update",
	}
	var stdoutBuf, stderrBuf bytes.Buffer
	stdoutW := multiWriter(req.Stdout, &stdoutBuf)
	stderrW := multiWriter(req.Stderr, &stderrBuf)

	cmd := osexec.CommandContext(ctx, r.BinaryPath, args...)
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	cmd.Env = append(os.Environ(),
		"RIPPLE_BASE_URL="+req.BaseURL,
		"RIPPLE_STORY_ID="+req.StoryID,
		"TASKMANAGER_BASE_URL="+req.BaseURL,
		"TASKMANAGER_STORY_ID="+req.StoryID,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err := runCmdWithCancel(ctx, cmd)
	result := AgentRunResult{Stdout: stdoutBuf.String(), Stderr: stderrBuf.String()}
	if err != nil {
		return result, err
	}
	text, extractErr := extractGrokJSONText(result.Stdout)
	if extractErr != nil {
		return result, extractErr
	}
	result.FinalMessage = text
	return result, nil
}

// HTTPAPIRunner calls an OpenAI-compatible chat/completions endpoint (reviewer only in v1).
type HTTPAPIRunner struct {
	providerID   string
	providerName string
	Config       APIProviderConfig
	Client       *http.Client
}

func (r *HTTPAPIRunner) ProviderID() string   { return r.providerID }
func (r *HTTPAPIRunner) ProviderName() string { return r.providerName }
func (r *HTTPAPIRunner) Kind() string         { return ProviderKindAPI }

func (r *HTTPAPIRunner) Run(ctx context.Context, req AgentRunRequest) (AgentRunResult, error) {
	if strings.TrimSpace(r.Config.BaseURL) == "" {
		return AgentRunResult{}, badRequest("API provider base URL is not configured")
	}
	if strings.TrimSpace(r.Config.APIKey) == "" {
		return AgentRunResult{}, badRequest("API provider key is not configured")
	}
	if strings.TrimSpace(r.Config.Model) == "" {
		return AgentRunResult{}, badRequest("API provider model is not configured")
	}
	endpoint := openAIChatCompletionsURL(r.Config.BaseURL)
	body := map[string]any{
		"model": r.Config.Model,
		"messages": []map[string]string{
			{
				"role":    "system",
				"content": "You are a careful code reviewer. Respond with JSON only matching the schema requested by the user. Do not wrap the JSON in markdown fences.",
			},
			{
				"role":    "user",
				"content": req.Prompt,
			},
		},
		"temperature": 0,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return AgentRunResult{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return AgentRunResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+r.Config.APIKey)
	for k, v := range r.Config.Headers {
		k = strings.TrimSpace(k)
		if k == "" || strings.EqualFold(k, "Authorization") {
			continue
		}
		httpReq.Header.Set(k, v)
	}

	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Minute}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return AgentRunResult{}, redactSecrets(fmt.Errorf("API review request failed: %w", err), r.Config.APIKey)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	// Never include the API key in stored stdout/stderr.
	safeBody := redactSecretString(string(raw), r.Config.APIKey)
	if req.Stdout != nil {
		_, _ = io.WriteString(req.Stdout, safeBody)
	}
	result := AgentRunResult{Stdout: safeBody}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := redactSecretString(openAIErrorMessage(raw, resp.StatusCode), r.Config.APIKey)
		err := badRequest(msg)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			err = badRequest("API key was rejected by the provider (unauthorized). Check the key in Settings → Agents.")
		}
		result.Stderr = msg
		if req.Stderr != nil {
			_, _ = io.WriteString(req.Stderr, msg)
		}
		return result, err
	}
	content, err := extractOpenAIChatContent(raw)
	if err != nil {
		return result, redactSecrets(err, r.Config.APIKey)
	}
	result.FinalMessage = redactSecretString(strings.TrimSpace(content), r.Config.APIKey)
	return result, nil
}

func openAIChatCompletionsURL(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	return base + "/chat/completions"
}

func extractOpenAIChatContent(raw []byte) (string, error) {
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		// Fall back: treat body as plain text review JSON.
		text := strings.TrimSpace(string(raw))
		if text != "" {
			return text, nil
		}
		return "", fmt.Errorf("invalid API response: %w", err)
	}
	if resp.Error != nil && strings.TrimSpace(resp.Error.Message) != "" {
		return "", badRequest(resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("API response contained no choices")
	}
	content := strings.TrimSpace(resp.Choices[0].Message.Content)
	if content == "" {
		return "", errors.New("API response message content was empty")
	}
	return content, nil
}

func openAIErrorMessage(raw []byte, status int) string {
	var resp struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err == nil && resp.Error != nil && strings.TrimSpace(resp.Error.Message) != "" {
		return fmt.Sprintf("API error (%d): %s", status, resp.Error.Message)
	}
	body := strings.TrimSpace(string(raw))
	if body == "" {
		return fmt.Sprintf("API error (HTTP %d)", status)
	}
	return fmt.Sprintf("API error (HTTP %d): %s", status, truncate(body, 400))
}

func redactSecrets(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	return errors.New(redactSecretString(err.Error(), secrets...))
}

func redactSecretString(msg string, secrets ...string) string {
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret == "" {
			continue
		}
		msg = strings.ReplaceAll(msg, secret, "[redacted]")
	}
	return msg
}

func multiWriter(primary io.Writer, buf *bytes.Buffer) io.Writer {
	if primary == nil {
		return buf
	}
	return io.MultiWriter(primary, buf)
}

func runCmdWithCancel(ctx context.Context, cmd *osexec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		<-done
		return ctx.Err()
	}
}

// parseAgentReview extracts the structured review JSON contract from model output.
func parseAgentReview(text string) (AgentReview, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return AgentReview{}, errors.New("empty review response")
	}
	if review, err := decodeAgentReviewJSON(text); err == nil {
		return review, nil
	}
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start >= 0 && end > start {
		if review, err := decodeAgentReviewJSON(text[start : end+1]); err == nil {
			return review, nil
		}
	}
	return AgentReview{}, fmt.Errorf("could not parse review JSON from response: %s", truncate(text, 400))
}

func decodeAgentReviewJSON(text string) (AgentReview, error) {
	var review AgentReview
	if err := json.Unmarshal([]byte(text), &review); err != nil {
		return AgentReview{}, err
	}
	return review, nil
}

// parseGrokReview is retained as a thin alias for existing call sites/tests.
func parseGrokReview(text string) (GrokReview, error) {
	return parseAgentReview(text)
}

func parseAPIProviderConfig(configJSON string) APIProviderConfig {
	var cfg APIProviderConfig
	_ = json.Unmarshal([]byte(strings.TrimSpace(configJSON)), &cfg)
	cfg.BaseURL = strings.TrimSpace(cfg.BaseURL)
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	cfg.Model = strings.TrimSpace(cfg.Model)
	if cfg.Headers == nil {
		cfg.Headers = map[string]string{}
	}
	return cfg
}

func (p AgentProvider) APIConfig() APIProviderConfig {
	return parseAPIProviderConfig(p.ConfigJSON)
}

// IsImplementerCapable reports whether this provider can write code for stories.
// CLI tools can implement; HTTP API providers cannot (no file-edit tools in v1).
func (p AgentProvider) IsImplementerCapable() bool {
	return p.Kind == ProviderKindCLI
}

// IsReviewerCapable reports whether this provider can review pull requests.
// Any CLI or OpenAI-compatible API provider may review.
func (p AgentProvider) IsReviewerCapable() bool {
	return p.Kind == ProviderKindCLI || p.Kind == ProviderKindAPI
}

func (p AgentProvider) MaskedAPIKey() string {
	if p.Kind != ProviderKindAPI {
		return ""
	}
	key := p.APIConfig().APIKey
	if key == "" {
		return ""
	}
	return "••••••••"
}

func (p AgentProvider) HasAPIKey() bool {
	return p.Kind == ProviderKindAPI && strings.TrimSpace(p.APIConfig().APIKey) != ""
}

// buildAgentReviewPrompt is the shared review prompt for CLI and API reviewers.
func buildAgentReviewPrompt(story Story, prNumber int, prURL, diff string) string {
	return buildGrokReviewPrompt(story, prNumber, prURL, diff)
}
