package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	RunKindCodexImplement = "codex_implement"
	RunKindGrokReview     = "grok_review"
	RunKindCodexFix       = "codex_fix"

	PipelinePhasePreflight   = "preflight"
	PipelinePhaseBranching   = "branching"
	PipelinePhaseImplement   = "implementing"
	PipelinePhasePush        = "pushing"
	PipelinePhaseCreatePR    = "creating_pr"
	PipelinePhaseReview      = "reviewing"
	PipelinePhaseFix         = "fixing"
	PipelinePhaseQualityGate = "quality_gate"
	PipelinePhaseMerge       = "merging"
	PipelinePhaseCompleted   = "completed"
)

type GrokReviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Body string `json:"body"`
}

type GrokReview struct {
	Approved bool                `json:"approved"`
	Summary  string              `json:"summary"`
	Comments []GrokReviewComment `json:"comments"`
}

type StoryPipeline struct {
	ID            int64
	QueueRunID    int64
	StoryID       string
	Phase         string
	Branch        string
	DefaultBranch string
	PRNumber      int
	PRURL         string
	ReviewJSON    string
	Error         string
}

type pipelineContext struct {
	QueueRunID    int64
	BaseURL       string
	Project       Project
	Story         Story
	Branch        string
	DefaultBranch string
	PRNumber      int
	PRURL         string
}

func storyBranchName(story Story) string {
	slug := slugifyBranchSegment(story.Title)
	if slug == "" {
		slug = "work"
	}
	return fmt.Sprintf("ripple/%s-%s", story.ID, slug)
}

var branchSlugPattern = regexp.MustCompile(`[^a-z0-9]+`)

func slugifyBranchSegment(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	text = branchSlugPattern.ReplaceAllString(text, "-")
	text = strings.Trim(text, "-")
	if len(text) > 48 {
		text = strings.Trim(text[:48], "-")
	}
	return text
}

func resolveGhBinary() (string, error) {
	if configured := firstEnv("RIPPLE_GH_BIN", "TASKMANAGER_GH_BIN"); configured != "" {
		return configured, nil
	}
	path, err := osexec.LookPath("gh")
	if err != nil {
		return "", badRequest("GitHub CLI (gh) was not found. Install gh and authenticate with `gh auth login`, or set RIPPLE_GH_BIN.")
	}
	return path, nil
}

func resolveGrokBinary() (string, error) {
	candidates := []string{}
	if configured := firstEnv("RIPPLE_GROK_BIN", "TASKMANAGER_GROK_BIN"); configured != "" {
		candidates = append(candidates, configured)
	}
	candidates = append(candidates,
		"grok",
		filepath.Join(os.Getenv("HOME"), ".grok", "bin", "grok"),
		"/opt/homebrew/bin/grok",
		"/usr/local/bin/grok",
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
	return "", badRequest("Grok CLI was not found. Set RIPPLE_GROK_BIN to the grok executable path, for example ~/.grok/bin/grok.")
}

func runCommand(ctx context.Context, dir string, name string, args ...string) (string, string, error) {
	cmd := osexec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func gitPreflight(ctx context.Context, dir string) (string, error) {
	if !isGitWorkTree(dir) {
		return "", fmt.Errorf("working directory is not a git repository: %s", dir)
	}
	if _, _, err := runCommand(ctx, dir, "git", "rev-parse", "--is-inside-work-tree"); err != nil {
		return "", err
	}
	status, _, err := runCommand(ctx, dir, "git", "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(status) != "" {
		return "", fmt.Errorf("repository has uncommitted changes; commit, stash, or discard local changes before running the queue:\n%s", truncate(status, 1200))
	}
	defaultBranch, err := detectDefaultBranch(ctx, dir)
	if err != nil {
		return "", err
	}
	current, _, err := runCommand(ctx, dir, "git", "branch", "--show-current")
	if err != nil {
		return "", err
	}
	current = strings.TrimSpace(current)
	if current != defaultBranch {
		if _, stderr, err := runCommand(ctx, dir, "git", "checkout", defaultBranch); err != nil {
			detail := strings.TrimSpace(stderr)
			if detail == "" {
				detail = err.Error()
			}
			return "", fmt.Errorf("could not checkout %s: %s", defaultBranch, detail)
		}
	}
	return defaultBranch, nil
}

func detectDefaultBranch(ctx context.Context, dir string) (string, error) {
	for _, candidate := range []string{"main", "master"} {
		if _, _, err := runCommand(ctx, dir, "git", "show-ref", "--verify", "--quiet", "refs/heads/"+candidate); err == nil {
			return candidate, nil
		}
		if _, _, err := runCommand(ctx, dir, "git", "show-ref", "--verify", "--quiet", "refs/remotes/origin/"+candidate); err == nil {
			return candidate, nil
		}
	}
	stdout, _, err := runCommand(ctx, dir, "git", "symbolic-ref", "refs/remotes/origin/HEAD")
	if err == nil {
		ref := strings.TrimSpace(stdout)
		if parts := strings.Split(ref, "/"); len(parts) > 0 {
			return parts[len(parts)-1], nil
		}
	}
	return "", errors.New("could not detect default branch; expected main or master")
}

func gitCreateFeatureBranch(ctx context.Context, dir, defaultBranch, branch string) error {
	if _, stderr, err := runCommand(ctx, dir, "git", "checkout", defaultBranch); err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("checkout %s failed: %s", defaultBranch, detail)
	}
	if _, stderr, err := runCommand(ctx, dir, "git", "checkout", "-b", branch); err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("create branch %s failed: %s", branch, detail)
	}
	return nil
}

func gitCommitAll(ctx context.Context, dir, message string) error {
	status, _, err := runCommand(ctx, dir, "git", "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) == "" {
		return nil
	}
	if _, stderr, err := runCommand(ctx, dir, "git", "add", "-A"); err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("git add failed: %s", detail)
	}
	if _, stderr, err := runCommand(ctx, dir, "git", "commit", "-m", message); err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("git commit failed: %s", detail)
	}
	return nil
}

func gitBranchAheadCount(ctx context.Context, dir, baseBranch, branch string) (int, error) {
	stdout, _, err := runCommand(ctx, dir, "git", "rev-list", "--count", baseBranch+".."+branch)
	if err != nil {
		return 0, err
	}
	var count int
	if _, scanErr := fmt.Sscanf(strings.TrimSpace(stdout), "%d", &count); scanErr != nil {
		return 0, scanErr
	}
	return count, nil
}

func gitPushBranch(ctx context.Context, dir, branch string) error {
	if _, stderr, err := runCommand(ctx, dir, "git", "push", "-u", "origin", branch); err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("push branch %s failed: %s", branch, detail)
	}
	return nil
}

func gitCheckoutBranch(ctx context.Context, dir, branch string) error {
	if _, stderr, err := runCommand(ctx, dir, "git", "checkout", branch); err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("checkout %s failed: %s", branch, detail)
	}
	return nil
}

func gitDeleteLocalBranch(ctx context.Context, dir, branch, defaultBranch string) error {
	_ = gitCheckoutBranch(ctx, dir, defaultBranch)
	if _, _, err := runCommand(ctx, dir, "git", "branch", "-D", branch); err != nil {
		return err
	}
	return nil
}

func ghCreatePR(ctx context.Context, ghBin, dir, baseBranch, headBranch, title, body string) (int, string, error) {
	stdout, stderr, err := runCommand(ctx, dir, ghBin, "pr", "create",
		"--base", baseBranch,
		"--head", headBranch,
		"--title", title,
		"--body", body,
	)
	if err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = strings.TrimSpace(stdout)
		}
		if detail == "" {
			detail = err.Error()
		}
		return 0, "", fmt.Errorf("create PR failed: %s", detail)
	}
	prNumber, prURL, err := parseCreatedPRURL(stdout)
	if err != nil {
		return 0, "", fmt.Errorf("parse PR create response: %w", err)
	}
	return prNumber, prURL, nil
}

func parseCreatedPRURL(output string) (int, string, error) {
	lines := strings.Fields(output)
	if len(lines) == 0 {
		return 0, "", errors.New("create PR returned no URL")
	}
	prURL := lines[len(lines)-1]
	parsed, err := url.Parse(prURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return 0, "", fmt.Errorf("create PR returned invalid URL %q", prURL)
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 2 || parts[len(parts)-2] != "pull" {
		return 0, "", fmt.Errorf("create PR returned unexpected URL %q", prURL)
	}
	prNumber, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil || prNumber <= 0 {
		return 0, "", fmt.Errorf("create PR returned invalid PR number in %q", prURL)
	}
	return prNumber, prURL, nil
}

func ghPRDiff(ctx context.Context, ghBin, dir string, prNumber int) (string, error) {
	stdout, stderr, err := runCommand(ctx, dir, ghBin, "pr", "diff", fmt.Sprintf("%d", prNumber))
	if err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("gh pr diff failed: %s", detail)
	}
	return stdout, nil
}

func ghPRComment(ctx context.Context, ghBin, dir string, prNumber int, body string) error {
	_, stderr, err := runCommand(ctx, dir, ghBin, "pr", "comment", fmt.Sprintf("%d", prNumber), "--body", body)
	if err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("post PR comment failed: %s", detail)
	}
	return nil
}

func ghPRMerge(ctx context.Context, ghBin, dir string, prNumber int) error {
	_, stderr, err := runCommand(ctx, dir, ghBin, "pr", "merge", fmt.Sprintf("%d", prNumber), "--merge", "--delete-branch")
	if err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("merge PR failed: %s", detail)
	}
	return nil
}

func runQualityGate(ctx context.Context, dir string) error {
	checks := qualityGateChecks(dir)
	if len(checks) == 0 {
		return nil
	}
	var failures []string
	for _, check := range checks {
		parts := strings.Fields(check)
		if len(parts) == 0 {
			continue
		}
		_, stderr, err := runCommand(ctx, dir, parts[0], parts[1:]...)
		if err != nil {
			detail := strings.TrimSpace(stderr)
			if detail == "" {
				detail = err.Error()
			}
			failures = append(failures, fmt.Sprintf("%s failed:\n%s", check, truncate(detail, 1200)))
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "\n\n"))
	}
	return nil
}

func qualityGateChecks(dir string) []string {
	var checks []string
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		checks = append(checks, "go test ./...")
		checks = append(checks, "go vet ./...")
	}
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err == nil {
		for _, script := range []string{"test", "lint", "typecheck", "build"} {
			if hasNPMScript(dir, script) {
				checks = append(checks, "npm run "+script)
			}
		}
	}
	return checks
}

func hasNPMScript(dir, name string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return false
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return false
	}
	script, ok := pkg.Scripts[name]
	return ok && strings.TrimSpace(script) != ""
}

func buildCodexImplementPrompt(baseURL, botDocs string, project Project, story Story, branch, previousSummary string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are being run by Ripple to implement one queued story on a feature branch.\n\n")
	fmt.Fprintf(&b, "# Working Rules\n\n")
	fmt.Fprintf(&b, "- You are already on feature branch `%s`. Do not create, rename, push, or merge branches.\n", branch)
	fmt.Fprintf(&b, "- Do not open pull requests, push commits, or merge. Ripple handles git and GitHub after you finish.\n")
	fmt.Fprintf(&b, "- Do not change story status through the API. The orchestrator marks the story done after the PR is merged.\n")
	fmt.Fprintf(&b, "- Work only on the current story unless a tiny adjacent change is required.\n")
	fmt.Fprintf(&b, "- Before editing, inspect the project structure and look for AGENTS.md, README files, styleguides, shared components, existing similar screens, routes, tests, and CSS patterns.\n")
	fmt.Fprintf(&b, "- Follow the style and conventions of the existing app. Prefer existing helpers and patterns over new abstractions.\n")
	fmt.Fprintf(&b, "- Run relevant tests and linting before finishing. If checks cannot run, explain why in your final response.\n")
	fmt.Fprintf(&b, "- The sandbox may block network access, writes outside the project, and localhost servers. Do not repeatedly retry commands that fail for those reasons.\n")
	fmt.Fprintf(&b, "- Do not use network-backed package loaders such as `pnpm dlx` or `npx` unless the required package is already cached. Prefer installed package sources and repository documentation.\n")
	fmt.Fprintf(&b, "- If repository guidance references a missing file or unmatched glob, note it once and continue with the guidance that is available.\n")
	fmt.Fprintf(&b, "- Do not launch a long-running development server for verification. Use finite tests and builds; explain any browser-test limitation in your final response.\n")
	fmt.Fprintf(&b, "- Leave all completed work as local file changes. The orchestrator will commit and push after you finish.\n")
	fmt.Fprintf(&b, "- If you cannot complete the work, explain the blocker in your final response.\n\n")
	fmt.Fprintf(&b, "# Ripple API\n\n")
	fmt.Fprintf(&b, "Base URL: %s\n\n", baseURL)
	fmt.Fprintf(&b, "Bot docs from %s/api/docs:\n\n%s\n\n", baseURL, botDocs)
	if strings.TrimSpace(previousSummary) != "" {
		fmt.Fprintf(&b, "# Previous Completed Story\n\n%s\n\n", previousSummary)
	}
	fmt.Fprintf(&b, "# Project\n\n")
	fmt.Fprintf(&b, "Name: %s\nID: %s\nPrefix: %s\nWorking directory: %s\nFeature branch: %s\n\n", project.Name, project.ID, project.Prefix, project.WorkingDirectory, branch)
	fmt.Fprintf(&b, "# Current Story\n\n")
	fmt.Fprintf(&b, "ID: %s\nTitle: %s\nStatus: %s\n", story.ID, story.Title, story.Status)
	if story.EpicName != nil {
		fmt.Fprintf(&b, "Epic: %s\n", *story.EpicName)
	}
	fmt.Fprintf(&b, "\nDescription:\n%s\n", story.Description)
	return b.String()
}

func buildCodexFixPrompt(baseURL, botDocs string, project Project, story Story, branch string, prNumber int, prURL, reviewJSON string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are being run by Ripple to address one round of pull request review feedback.\n\n")
	fmt.Fprintf(&b, "# Working Rules\n\n")
	fmt.Fprintf(&b, "- You are on feature branch `%s` for PR #%d (%s).\n", branch, prNumber, prURL)
	fmt.Fprintf(&b, "- Read the Grok review feedback below and address valid issues.\n")
	fmt.Fprintf(&b, "- Do not merge the PR, change story status, or create a new branch.\n")
	fmt.Fprintf(&b, "- Run relevant tests and linting and fix failures before finishing.\n")
	fmt.Fprintf(&b, "- The sandbox may block network access, writes outside the project, and localhost servers. Do not repeatedly retry commands that fail for those reasons.\n")
	fmt.Fprintf(&b, "- Do not use network-backed package loaders such as `pnpm dlx` or `npx` unless the required package is already cached. Prefer installed package sources and repository documentation.\n")
	fmt.Fprintf(&b, "- If repository guidance references a missing file or unmatched glob, note it once and continue with the guidance that is available.\n")
	fmt.Fprintf(&b, "- Leave all fixes as local file changes. The orchestrator will commit and push after you finish.\n\n")
	fmt.Fprintf(&b, "# Grok Review Feedback\n\n%s\n\n", reviewJSON)
	fmt.Fprintf(&b, "# Ripple API\n\nBase URL: %s\n\nBot docs:\n\n%s\n\n", baseURL, botDocs)
	fmt.Fprintf(&b, "# Story\n\nID: %s\nTitle: %s\n\nDescription:\n%s\n", story.ID, story.Title, story.Description)
	return b.String()
}

func buildGrokReviewPrompt(story Story, prNumber int, prURL, diff string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are reviewing a pull request for Ripple.\n\n")
	fmt.Fprintf(&b, "Story: %s — %s\n", story.ID, story.Title)
	fmt.Fprintf(&b, "PR: #%d %s\n\n", prNumber, prURL)
	fmt.Fprintf(&b, "Review the diff for bugs, regressions, missing tests, style issues, and incomplete work.\n")
	fmt.Fprintf(&b, "Review only the supplied diff. Do not modify files or invoke editing tools.\n")
	fmt.Fprintf(&b, "Respond with JSON only, no markdown fences:\n")
	fmt.Fprintf(&b, `{"approved":true,"summary":"short summary","comments":[{"path":"optional/file.go","line":0,"body":"actionable feedback"}]}`+"\n\n")
	fmt.Fprintf(&b, "Set approved to true only if the change is ready to merge after tests pass.\n")
	fmt.Fprintf(&b, "Use comments only for actionable fixes. Keep the summary concise.\n\n")
	fmt.Fprintf(&b, "# PR Diff\n\n```diff\n%s\n```\n", truncate(diff, 120000))
	return b.String()
}

func parseGrokReview(text string) (GrokReview, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return GrokReview{}, errors.New("empty Grok review response")
	}
	if review, err := decodeGrokReviewJSON(text); err == nil {
		return review, nil
	}
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start >= 0 && end > start {
		if review, err := decodeGrokReviewJSON(text[start : end+1]); err == nil {
			return review, nil
		}
	}
	return GrokReview{}, fmt.Errorf("could not parse Grok review JSON from response: %s", truncate(text, 400))
}

func decodeGrokReviewJSON(text string) (GrokReview, error) {
	var review GrokReview
	if err := json.Unmarshal([]byte(text), &review); err != nil {
		return GrokReview{}, err
	}
	return review, nil
}

func reviewNeedsFix(review GrokReview) bool {
	if !review.Approved {
		return true
	}
	for _, comment := range review.Comments {
		if strings.TrimSpace(comment.Body) != "" {
			return true
		}
	}
	return false
}

func formatReviewForPR(review GrokReview) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Grok review\n\n%s\n", strings.TrimSpace(review.Summary))
	if len(review.Comments) > 0 {
		fmt.Fprintf(&b, "\n### Feedback\n")
		for _, comment := range review.Comments {
			body := strings.TrimSpace(comment.Body)
			if body == "" {
				continue
			}
			if strings.TrimSpace(comment.Path) != "" {
				if comment.Line > 0 {
					fmt.Fprintf(&b, "\n- `%s:%d` — %s", comment.Path, comment.Line, body)
				} else {
					fmt.Fprintf(&b, "\n- `%s` — %s", comment.Path, body)
				}
			} else {
				fmt.Fprintf(&b, "\n- %s", body)
			}
		}
	}
	return b.String()
}

func (a *App) runGrokHeadless(ctx context.Context, queueRunID int64, baseURL string, project Project, story Story, prompt, runKind string) (string, error) {
	grokBin, err := resolveGrokBinary()
	if err != nil {
		return "", err
	}
	agentRunID, err := a.createAgentRun(context.Background(), queueRunID, project, story, prompt, runKind, "", 0, "")
	if err != nil {
		return "", err
	}
	output := &agentRunOutput{
		flush: func(stdout, stderr string) {
			_ = a.updateAgentStoryRunOutput(context.Background(), agentRunID, stdout, stderr)
		},
	}
	args := []string{
		"-p", prompt,
		"-m", "grok-build",
		"--cwd", project.WorkingDirectory,
		"--sandbox", "workspace",
		"--always-approve",
		"--output-format", "json",
		"--no-auto-update",
	}
	cmd := osexec.CommandContext(ctx, grokBin, args...)
	cmd.Stdout = output.stdoutWriter()
	cmd.Stderr = output.stderrWriter()
	cmd.Env = append(os.Environ(),
		"RIPPLE_BASE_URL="+baseURL,
		"RIPPLE_STORY_ID="+story.ID,
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
	text, err := extractGrokJSONText(stdoutText)
	if err != nil {
		_ = a.finishAgentStoryRun(context.Background(), agentRunID, "failed", stdoutText, stderrText, "", err)
		return "", err
	}
	_ = a.finishAgentStoryRun(context.Background(), agentRunID, "completed", stdoutText, stderrText, text, nil)
	return text, nil
}

func extractGrokJSONText(stdout string) (string, error) {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return "", errors.New("empty Grok stdout")
	}
	type grokResponse struct {
		Text  string `json:"text"`
		Type  string `json:"type"`
		Error string `json:"message"`
	}
	var resp grokResponse
	if err := json.Unmarshal([]byte(stdout), &resp); err == nil {
		if resp.Type == "error" {
			if resp.Error != "" {
				return "", errors.New(resp.Error)
			}
			return "", errors.New("Grok returned an error response")
		}
		if strings.TrimSpace(resp.Text) != "" {
			return strings.TrimSpace(resp.Text), nil
		}
	}
	return stdout, nil
}

func (a *App) upsertStoryPipeline(ctx context.Context, pipeline StoryPipeline) error {
	now := formatTime(timeNowUTC())
	_, err := a.db.ExecContext(ctx, `INSERT INTO story_pipelines (queue_run_id, story_id, phase, branch, default_branch, pr_number, pr_url, review_json, error, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(queue_run_id, story_id) DO UPDATE SET
			phase = excluded.phase,
			branch = excluded.branch,
			default_branch = excluded.default_branch,
			pr_number = excluded.pr_number,
			pr_url = excluded.pr_url,
			review_json = excluded.review_json,
			error = excluded.error,
			updated_at = excluded.updated_at`,
		pipeline.QueueRunID, pipeline.StoryID, pipeline.Phase, pipeline.Branch, pipeline.DefaultBranch,
		pipeline.PRNumber, pipeline.PRURL, pipeline.ReviewJSON, pipeline.Error, now)
	return err
}

func (a *App) runStoryPipeline(ctx context.Context, pc pipelineContext, previousSummary string) (string, error) {
	ghBin, err := resolveGhBinary()
	if err != nil {
		return "", err
	}
	dir := pc.Project.WorkingDirectory
	pipeline := StoryPipeline{
		QueueRunID: pc.QueueRunID,
		StoryID:    pc.Story.ID,
	}

	a.updateAgentProgress(pc.Story.ID, fmt.Sprintf("Preflight %s", pc.Story.ID), 0, 0)
	pipeline.Phase = PipelinePhasePreflight
	if err := a.upsertStoryPipeline(ctx, pipeline); err != nil {
		return "", err
	}
	defaultBranch, err := gitPreflight(ctx, dir)
	if err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		return "", err
	}
	pc.DefaultBranch = defaultBranch
	pipeline.DefaultBranch = defaultBranch

	branch := storyBranchName(pc.Story)
	pc.Branch = branch
	pipeline.Branch = branch
	pipeline.Phase = PipelinePhaseBranching
	if err := a.upsertStoryPipeline(ctx, pipeline); err != nil {
		return "", err
	}
	if err := gitCreateFeatureBranch(ctx, dir, defaultBranch, branch); err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		return "", err
	}

	docs, err := embeddedFiles.ReadFile("docs/bot-api.md")
	if err != nil {
		return "", err
	}
	pipeline.Phase = PipelinePhaseImplement
	_ = a.upsertStoryPipeline(ctx, pipeline)
	implementPrompt := buildCodexImplementPrompt(pc.BaseURL, string(docs), pc.Project, pc.Story, branch, previousSummary)
	finalMessage, err := a.runCodexForStoryWithKind(ctx, pc.QueueRunID, pc.BaseURL, pc.Project, pc.Story, implementPrompt, RunKindCodexImplement, branch, 0, "")
	if err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		return "", err
	}

	pipeline.Phase = PipelinePhasePush
	_ = a.upsertStoryPipeline(ctx, pipeline)
	if err := gitCommitAll(ctx, dir, fmt.Sprintf("%s: %s", pc.Story.ID, pc.Story.Title)); err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		return "", err
	}
	ahead, err := gitBranchAheadCount(ctx, dir, defaultBranch, branch)
	if err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		return "", err
	}
	if ahead == 0 {
		err := errors.New("implementation produced no commits on the feature branch")
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		return "", err
	}
	if err := gitPushBranch(ctx, dir, branch); err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		return "", err
	}

	pipeline.Phase = PipelinePhaseCreatePR
	_ = a.upsertStoryPipeline(ctx, pipeline)
	prTitle := fmt.Sprintf("%s: %s", pc.Story.ID, pc.Story.Title)
	prBody := fmt.Sprintf("Automated PR for story **%s**.\n\n## Description\n\n%s", pc.Story.ID, pc.Story.Description)
	prNumber, prURL, err := ghCreatePR(ctx, ghBin, dir, defaultBranch, branch, prTitle, prBody)
	if err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		return "", err
	}
	pc.PRNumber = prNumber
	pc.PRURL = prURL
	pipeline.PRNumber = prNumber
	pipeline.PRURL = prURL

	pipeline.Phase = PipelinePhaseReview
	_ = a.upsertStoryPipeline(ctx, pipeline)
	diff, err := ghPRDiff(ctx, ghBin, dir, prNumber)
	if err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		return "", err
	}
	reviewPrompt := buildGrokReviewPrompt(pc.Story, prNumber, prURL, diff)
	reviewText, err := a.runGrokHeadless(ctx, pc.QueueRunID, pc.BaseURL, pc.Project, pc.Story, reviewPrompt, RunKindGrokReview)
	if err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		return "", err
	}
	review, err := parseGrokReview(reviewText)
	if err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		return "", err
	}
	reviewJSON, _ := json.Marshal(review)
	pipeline.ReviewJSON = string(reviewJSON)
	_ = a.upsertStoryPipeline(ctx, pipeline)
	if err := ghPRComment(ctx, ghBin, dir, prNumber, formatReviewForPR(review)); err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		return "", err
	}

	if reviewNeedsFix(review) {
		pipeline.Phase = PipelinePhaseFix
		_ = a.upsertStoryPipeline(ctx, pipeline)
		fixPrompt := buildCodexFixPrompt(pc.BaseURL, string(docs), pc.Project, pc.Story, branch, prNumber, prURL, string(reviewJSON))
		if _, err := a.runCodexForStoryWithKind(ctx, pc.QueueRunID, pc.BaseURL, pc.Project, pc.Story, fixPrompt, RunKindCodexFix, branch, prNumber, prURL); err != nil {
			pipeline.Error = err.Error()
			_ = a.upsertStoryPipeline(ctx, pipeline)
			return "", err
		}
		if err := gitCommitAll(ctx, dir, fmt.Sprintf("%s: address review feedback", pc.Story.ID)); err != nil {
			pipeline.Error = err.Error()
			_ = a.upsertStoryPipeline(ctx, pipeline)
			return "", err
		}
		if err := gitPushBranch(ctx, dir, branch); err != nil {
			pipeline.Error = err.Error()
			_ = a.upsertStoryPipeline(ctx, pipeline)
			return "", err
		}
	}

	pipeline.Phase = PipelinePhaseQualityGate
	_ = a.upsertStoryPipeline(ctx, pipeline)
	if err := runQualityGate(ctx, dir); err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		return "", err
	}

	pipeline.Phase = PipelinePhaseMerge
	_ = a.upsertStoryPipeline(ctx, pipeline)
	if err := ghPRMerge(ctx, ghBin, dir, prNumber); err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		return "", err
	}
	_ = gitCheckoutBranch(ctx, dir, defaultBranch)
	_ = gitDeleteLocalBranch(ctx, dir, branch, defaultBranch)

	pipeline.Phase = PipelinePhaseCompleted
	pipeline.Error = ""
	_ = a.upsertStoryPipeline(ctx, pipeline)
	return finalMessage, nil
}

func timeNowUTC() time.Time {
	return time.Now().UTC()
}
