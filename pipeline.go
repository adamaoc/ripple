package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	RunKindCodexImplement          = "codex_implement"
	RunKindGrokReview              = "grok_review"
	RunKindCodexFix                = "codex_fix"
	RunKindCodexAddressFeedback    = "codex_address_feedback"
	RunKindCodexResolveConflicts   = "codex_resolve_conflicts"

	feedbackFingerprintPrefix = "feedback_fingerprint:"
	eventAwaitingHumanReview  = "awaiting_human_review"
	eventAddressingFeedback   = "addressing_feedback"
	eventFeedbackAddressed    = "feedback_addressed"
	eventFeedbackNoChanges    = "feedback_no_changes"
	eventResolvingConflicts   = "resolving_conflicts"
	eventConflictsResolved    = "conflicts_resolved"
	eventMergeConflictDetected = "merge_conflict_detected"

	PipelinePhasePreflight         = "preflight"
	PipelinePhaseBranching         = "branching"
	PipelinePhaseImplement         = "implementing"
	PipelinePhasePush              = "pushing"
	PipelinePhaseCreatePR          = "creating_pr"
	PipelinePhaseReview            = "reviewing"
	PipelinePhaseFix               = "fixing"
	PipelinePhaseQualityGate       = "quality_gate"
	PipelinePhaseMerge             = "merging"
	PipelinePhaseAwaitingHuman     = "awaiting_human"
	PipelinePhaseAddressFeedback   = "addressing_feedback"
	PipelinePhaseResolveConflicts  = "resolving_conflicts"
	PipelinePhaseCompleted         = "completed"
)

// pipelineResult is the successful outcome of runStoryPipeline.
// AwaitingHuman means supervised mode paused after PR + agent review (story is in_review).
type pipelineResult struct {
	FinalMessage  string
	AwaitingHuman bool
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
	// MergeConflict is set when GitHub reports the PR cannot merge cleanly (usually after another PR landed).
	MergeConflict bool
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

func storyBranchName(project Project, story Story) string {
	slug := slugifyBranchSegment(story.Title)
	if slug == "" {
		slug = "work"
	}
	prefix := strings.TrimSpace(project.Prefix)
	if prefix == "" {
		prefix = strings.TrimSpace(story.ProjectPrefix)
	}
	tmpl := normalizeBranchNameTemplate(project.BranchNameTemplate)
	branch := tmpl
	branch = strings.ReplaceAll(branch, "{id}", story.ID)
	branch = strings.ReplaceAll(branch, "{slug}", slug)
	branch = strings.ReplaceAll(branch, "{prefix}", prefix)
	branch = strings.Trim(branch, "/")
	if branch == "" || strings.Contains(branch, "{") {
		return fmt.Sprintf("ripple/%s-%s", story.ID, slug)
	}
	return branch
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

func runCommand(ctx context.Context, dir string, name string, args ...string) (string, string, error) {
	return runCommandEnv(ctx, dir, nil, name, args...)
}

func runCommandEnv(ctx context.Context, dir string, env []string, name string, args ...string) (string, string, error) {
	cmd := osexec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		cmd.Env = env
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func gitPreflight(ctx context.Context, dir string, defaultBranchOverride string) (string, error) {
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
	var defaultBranch string
	if override := strings.TrimSpace(defaultBranchOverride); override != "" {
		defaultBranch = override
	} else {
		defaultBranch, err = detectDefaultBranch(ctx, dir)
		if err != nil {
			return "", err
		}
	}
	// Always refresh from origin so feature branches are cut from current main/master,
	// not a stale local copy left behind by earlier PR merges.
	if err := gitFetchOrigin(ctx, dir); err != nil {
		return "", err
	}
	if err := gitUpdateLocalBranchFromOrigin(ctx, dir, defaultBranch); err != nil {
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

func gitFetchOrigin(ctx context.Context, dir string) error {
	// Skip when there is no origin remote (local-only / test repos).
	if _, _, err := runCommand(ctx, dir, "git", "remote", "get-url", "origin"); err != nil {
		return nil
	}
	if _, stderr, err := runCommand(ctx, dir, "git", "fetch", "origin"); err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("git fetch origin failed: %s", detail)
	}
	return nil
}

// gitUpdateLocalBranchFromOrigin fast-forwards local defaultBranch to origin/defaultBranch.
func gitUpdateLocalBranchFromOrigin(ctx context.Context, dir, branch string) error {
	remoteRef := "origin/" + branch
	if _, _, err := runCommand(ctx, dir, "git", "rev-parse", "--verify", remoteRef); err != nil {
		// No remote tracking ref yet (offline / first clone without origin) — keep local branch.
		return nil
	}
	if _, stderr, err := runCommand(ctx, dir, "git", "checkout", branch); err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("checkout %s failed: %s", branch, detail)
	}
	if _, stderr, err := runCommand(ctx, dir, "git", "merge", "--ff-only", remoteRef); err != nil {
		// Fall back to hard reset when local main diverged (common after GitHub-side merges).
		if _, stderr2, err2 := runCommand(ctx, dir, "git", "reset", "--hard", remoteRef); err2 != nil {
			detail := strings.TrimSpace(stderr2)
			if detail == "" {
				detail = strings.TrimSpace(stderr)
			}
			if detail == "" {
				detail = err2.Error()
			}
			return fmt.Errorf("update %s from %s failed: %s", branch, remoteRef, detail)
		}
	}
	return nil
}

func gitHasUnresolvedConflicts(ctx context.Context, dir string) (bool, error) {
	stdout, _, err := runCommand(ctx, dir, "git", "ls-files", "-u")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(stdout) != "", nil
}

func gitMergeInProgress(ctx context.Context, dir string) bool {
	// MERGE_HEAD exists while a merge is unfinished.
	if _, _, err := runCommand(ctx, dir, "git", "rev-parse", "-q", "--verify", "MERGE_HEAD"); err == nil {
		return true
	}
	return false
}

func gitCurrentBranch(ctx context.Context, dir string) (string, error) {
	stdout, stderr, err := runCommand(ctx, dir, "git", "branch", "--show-current")
	if err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("current branch: %s", detail)
	}
	return strings.TrimSpace(stdout), nil
}

func gitAbortMerge(ctx context.Context, dir string) {
	_, _, _ = runCommand(ctx, dir, "git", "merge", "--abort")
}

// gitWorktreeHasConflictMarkers reports whether any tracked file still contains merge markers.
// Agents often rewrite conflicted files without git-add; the index can still look "unmerged"
// even when the working tree content is clean — so marker scan is the source of truth after an agent pass.
func gitWorktreeHasConflictMarkers(ctx context.Context, dir string) (bool, error) {
	// git grep exits 1 when there are no matches.
	stdout, stderr, err := runCommand(ctx, dir, "git", "grep", "-n", "-E", `^<<<<<<< |^>>>>>>> `, "--", ".")
	if strings.TrimSpace(stdout) != "" {
		return true, nil
	}
	if err == nil {
		return false, nil
	}
	if strings.Contains(err.Error(), "exit status 1") {
		return false, nil
	}
	detail := strings.TrimSpace(stderr)
	if detail == "" {
		detail = err.Error()
	}
	return false, fmt.Errorf("scan conflict markers: %s", detail)
}

// gitMergeBaseIntoFeature merges origin/defaultBranch into featureBranch.
// Returns hadConflicts=true when the merge stopped with unresolved paths (agent should fix).
// If a merge is already in progress on featureBranch (retry after a partial agent pass), resumes that merge.
func gitMergeBaseIntoFeature(ctx context.Context, dir, featureBranch, defaultBranch string) (hadConflicts bool, err error) {
	if err := gitFetchOrigin(ctx, dir); err != nil {
		return false, err
	}

	if gitMergeInProgress(ctx, dir) {
		current, curErr := gitCurrentBranch(ctx, dir)
		if curErr != nil {
			gitAbortMerge(ctx, dir)
		} else if current == featureBranch {
			conflicts, confErr := gitHasUnresolvedConflicts(ctx, dir)
			if confErr != nil {
				return false, confErr
			}
			if conflicts {
				return true, nil
			}
			// Merge in progress but index is clean — ready to commit.
			return false, nil
		} else {
			// Stale merge on another branch — reset so we can start cleanly.
			gitAbortMerge(ctx, dir)
		}
	}

	if err := gitCheckoutBranch(ctx, dir, featureBranch); err != nil {
		// Index may still be locked from a prior failed merge attempt.
		if gitMergeInProgress(ctx, dir) {
			gitAbortMerge(ctx, dir)
			if err2 := gitCheckoutBranch(ctx, dir, featureBranch); err2 != nil {
				return false, err2
			}
		} else {
			return false, err
		}
	}
	// Ensure we have the latest pushed feature tip when the branch tracks origin.
	remoteFeature := "origin/" + featureBranch
	if _, _, revErr := runCommand(ctx, dir, "git", "rev-parse", "--verify", remoteFeature); revErr == nil {
		_, _, _ = runCommand(ctx, dir, "git", "merge", "--ff-only", remoteFeature)
	}
	// Prefer origin/default when available; also update local default so later ops are consistent.
	_ = gitUpdateLocalBranchFromOrigin(ctx, dir, defaultBranch)
	if err := gitCheckoutBranch(ctx, dir, featureBranch); err != nil {
		return false, err
	}
	remoteBase := "origin/" + defaultBranch
	if _, _, revErr := runCommand(ctx, dir, "git", "rev-parse", "--verify", remoteBase); revErr != nil {
		// Fall back to local default branch if remote is missing.
		remoteBase = defaultBranch
	}
	_, stderr, mergeErr := runCommand(ctx, dir, "git", "merge", "--no-edit", remoteBase)
	if mergeErr == nil {
		return false, nil
	}
	conflicts, confErr := gitHasUnresolvedConflicts(ctx, dir)
	if confErr != nil {
		return false, confErr
	}
	if conflicts {
		return true, nil
	}
	detail := strings.TrimSpace(stderr)
	if detail == "" {
		detail = mergeErr.Error()
	}
	return false, fmt.Errorf("merge %s into %s failed: %s", remoteBase, featureBranch, detail)
}

func gitCompleteMergeCommit(ctx context.Context, dir, message string, id GitHubIdentityConfig) error {
	if _, stderr, err := runCommand(ctx, dir, "git", "add", "-A"); err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("git add failed: %s", detail)
	}
	if still, err := gitHasUnresolvedConflicts(ctx, dir); err != nil {
		return err
	} else if still {
		return errors.New("unmerged paths remain after staging; conflict markers may still be present")
	}
	message = id.decorateCommitMessage(ctx, dir, message)
	env := mergeEnv(id.commitEnv(ctx, dir))
	if gitMergeInProgress(ctx, dir) {
		if _, stderr, err := runCommandEnv(ctx, dir, env, "git", "commit", "--no-edit", "-m", message); err != nil {
			// --no-edit may fail if no MERGE_MSG; retry with explicit message only.
			if _, stderr2, err2 := runCommandEnv(ctx, dir, env, "git", "commit", "-m", message); err2 != nil {
				detail := strings.TrimSpace(stderr2)
				if detail == "" {
					detail = strings.TrimSpace(stderr)
				}
				if detail == "" {
					detail = err2.Error()
				}
				return fmt.Errorf("git commit (merge) failed: %s", detail)
			}
		}
		return nil
	}
	return gitCommitAll(ctx, dir, message, id)
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

func gitCommitAll(ctx context.Context, dir, message string, id GitHubIdentityConfig) error {
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
	message = id.decorateCommitMessage(ctx, dir, message)
	env := mergeEnv(id.commitEnv(ctx, dir))
	if _, stderr, err := runCommandEnv(ctx, dir, env, "git", "commit", "-m", message); err != nil {
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

func ghCreatePR(ctx context.Context, ghBin, dir, baseBranch, headBranch, title, body string, id GitHubIdentityConfig) (int, string, error) {
	if err := id.validateBotFor(id.PRAuthorMode, "open pull requests"); err != nil {
		return 0, "", err
	}
	env := mergeEnv(id.ghEnvFor(id.PRAuthorMode))
	stdout, stderr, err := runCommandEnv(ctx, dir, env, ghBin, "pr", "create",
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

func ghPRComment(ctx context.Context, ghBin, dir string, prNumber int, body string, id GitHubIdentityConfig) error {
	if err := id.validateBotFor(id.CommentMode, "post agent review comments"); err != nil {
		return err
	}
	env := mergeEnv(id.ghEnvFor(id.CommentMode))
	_, stderr, err := runCommandEnv(ctx, dir, env, ghBin, "pr", "comment", fmt.Sprintf("%d", prNumber), "--body", body)
	if err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("post PR comment failed: %s", detail)
	}
	return nil
}

func ghPRMerge(ctx context.Context, ghBin, dir string, prNumber int, deleteRemoteBranch bool) error {
	args := []string{"pr", "merge", fmt.Sprintf("%d", prNumber), "--merge"}
	if deleteRemoteBranch {
		args = append(args, "--delete-branch")
	}
	_, stderr, err := runCommand(ctx, dir, ghBin, args...)
	if err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("merge PR failed: %s", detail)
	}
	return nil
}

// ghPRIsMerged reports whether the given pull request is already merged on GitHub.
func ghPRIsMerged(ctx context.Context, ghBin, dir string, prNumber int) (bool, error) {
	stdout, stderr, err := runCommand(ctx, dir, ghBin, "pr", "view", fmt.Sprintf("%d", prNumber), "--json", "state,mergedAt")
	if err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = err.Error()
		}
		return false, fmt.Errorf("check PR status failed: %s", detail)
	}
	var view struct {
		State    string `json:"state"`
		MergedAt string `json:"mergedAt"`
	}
	if err := json.Unmarshal([]byte(stdout), &view); err != nil {
		return false, fmt.Errorf("parse PR status: %w", err)
	}
	if strings.EqualFold(view.State, "MERGED") {
		return true, nil
	}
	if strings.TrimSpace(view.MergedAt) != "" {
		return true, nil
	}
	return false, nil
}

// ghPRHasMergeConflicts reports whether GitHub currently sees the PR as conflicting with its base.
// UNKNOWN is treated as not conflicting so we do not block on transient GitHub calculation delays.
func ghPRHasMergeConflicts(ctx context.Context, ghBin, dir string, prNumber int) (bool, string, error) {
	conflicted, unknown, detail, err := inspectPRMergeConflicts(ctx, ghBin, dir, prNumber)
	if err != nil {
		return false, "", err
	}
	if unknown {
		return false, "", nil
	}
	return conflicted, detail, nil
}

func looksLikeMergeConflictError(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "merge conflict") || strings.Contains(lower, "has merge conflicts")
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

// PRFeedback is review input collected for a supervised "Act on review comments" pass.
type PRFeedback struct {
	Items           []PRFeedbackItem
	AgentReviewJSON string
}

type PRFeedbackItem struct {
	Kind   string // review | review_comment | issue_comment
	Author string
	Body   string
	Path   string
	Line   int
}

func (f PRFeedback) HasActionableComments() bool {
	for _, item := range f.Items {
		if strings.TrimSpace(item.Body) != "" {
			return true
		}
	}
	return agentReviewHasActionableContent(f.AgentReviewJSON)
}

func agentReviewHasActionableContent(reviewJSON string) bool {
	reviewJSON = strings.TrimSpace(reviewJSON)
	if reviewJSON == "" {
		return false
	}
	review, err := parseGrokReview(reviewJSON)
	if err != nil {
		return true
	}
	if strings.TrimSpace(review.Summary) != "" {
		return true
	}
	for _, c := range review.Comments {
		if strings.TrimSpace(c.Body) != "" {
			return true
		}
	}
	return !review.Approved
}

func feedbackFingerprint(f PRFeedback) string {
	var b strings.Builder
	for _, item := range f.Items {
		fmt.Fprintf(&b, "%s|%s|%s|%s|%d\n", item.Kind, item.Author, strings.TrimSpace(item.Body), item.Path, item.Line)
	}
	b.WriteString("agent:")
	b.WriteString(strings.TrimSpace(f.AgentReviewJSON))
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func lastFeedbackFingerprint(events []StoryEvent) string {
	for _, ev := range events {
		if ev.Type != eventFeedbackAddressed && ev.Type != eventFeedbackNoChanges {
			continue
		}
		if idx := strings.Index(ev.Message, feedbackFingerprintPrefix); idx >= 0 {
			return strings.TrimSpace(ev.Message[idx+len(feedbackFingerprintPrefix):])
		}
	}
	return ""
}

func evaluateAddressFeedback(feedback PRFeedback, events []StoryEvent) error {
	if !feedback.HasActionableComments() {
		return badRequest("No review comments found to act on. Leave feedback on the pull request, then try again. The story stays in review.")
	}
	fp := feedbackFingerprint(feedback)
	if prev := lastFeedbackFingerprint(events); prev != "" && prev == fp {
		return badRequest("No new review comments since the last fix pass. Add feedback on the pull request, then try again. The story stays in review.")
	}
	return nil
}

func (f PRFeedback) FormatForPrompt() string {
	var b strings.Builder
	if len(f.Items) == 0 {
		b.WriteString("(No pull request comments were collected from GitHub.)\n")
	} else {
		for i, item := range f.Items {
			fmt.Fprintf(&b, "%d. [%s] %s", i+1, item.Kind, item.Author)
			if item.Path != "" {
				fmt.Fprintf(&b, " · %s", item.Path)
				if item.Line > 0 {
					fmt.Fprintf(&b, ":%d", item.Line)
				}
			}
			fmt.Fprintf(&b, "\n%s\n\n", strings.TrimSpace(item.Body))
		}
	}
	if strings.TrimSpace(f.AgentReviewJSON) != "" {
		fmt.Fprintf(&b, "---\nPrior agent review JSON (secondary context):\n%s\n", f.AgentReviewJSON)
	}
	return b.String()
}

func buildCodexAddressFeedbackPrompt(baseURL, botDocs string, project Project, story Story, branch string, prNumber int, prURL string, feedback PRFeedback) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are being run by Ripple to address pull request review comments for a supervised story.\n\n")
	fmt.Fprintf(&b, "# Working Rules\n\n")
	fmt.Fprintf(&b, "- You are on feature branch `%s` for PR #%d (%s).\n", branch, prNumber, prURL)
	fmt.Fprintf(&b, "- Prioritize **human** review comments and issue comments on the PR. Treat them as the primary source of truth.\n")
	fmt.Fprintf(&b, "- Use the prior agent review only as secondary context when it does not conflict with human feedback.\n")
	fmt.Fprintf(&b, "- Do not merge the PR, push, change story status, or create a new branch. Ripple commits and pushes after you finish.\n")
	fmt.Fprintf(&b, "- Run relevant tests and linting and fix failures before finishing.\n")
	fmt.Fprintf(&b, "- The sandbox may block network access, writes outside the project, and localhost servers. Do not repeatedly retry commands that fail for those reasons.\n")
	fmt.Fprintf(&b, "- Do not use network-backed package loaders such as `pnpm dlx` or `npx` unless the required package is already cached.\n")
	fmt.Fprintf(&b, "- Leave all fixes as local file changes. The orchestrator will commit and push after you finish.\n")
	fmt.Fprintf(&b, "- If nothing needs changing, explain that briefly in your final response and leave the tree clean.\n\n")
	fmt.Fprintf(&b, "# Review Feedback (human comments first)\n\n%s\n", feedback.FormatForPrompt())
	fmt.Fprintf(&b, "# Ripple API\n\nBase URL: %s\n\nBot docs:\n\n%s\n\n", baseURL, botDocs)
	fmt.Fprintf(&b, "# Project\n\nName: %s\nWorking directory: %s\n\n", project.Name, project.WorkingDirectory)
	fmt.Fprintf(&b, "# Story\n\nID: %s\nTitle: %s\n\nDescription:\n%s\n", story.ID, story.Title, story.Description)
	return b.String()
}

func ghCollectPRFeedback(ctx context.Context, ghBin, dir string, prNumber int, agentReviewJSON string) (PRFeedback, error) {
	feedback := PRFeedback{AgentReviewJSON: strings.TrimSpace(agentReviewJSON)}

	// Review submission bodies (top-level review text). Note: gh pr view --json comments
	// is conversation/issue comments, NOT inline review comments — those come from the REST API below.
	stdout, stderr, err := runCommand(ctx, dir, ghBin, "pr", "view", fmt.Sprintf("%d", prNumber), "--json", "reviews")
	if err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = err.Error()
		}
		return PRFeedback{}, fmt.Errorf("collect PR reviews failed: %s", detail)
	}
	if err := appendPRReviewBodies(&feedback, stdout); err != nil {
		return PRFeedback{}, err
	}

	repoStdout, _, repoErr := runCommand(ctx, dir, ghBin, "repo", "view", "--json", "nameWithOwner", "-q", ".nameWithOwner")
	if repoErr != nil {
		// Still return review bodies if we cannot resolve the repo for comment APIs.
		prioritizeHumanFeedback(&feedback)
		return feedback, nil
	}
	repo := strings.TrimSpace(repoStdout)
	if repo == "" {
		prioritizeHumanFeedback(&feedback)
		return feedback, nil
	}

	// Inline code review comments (line comments on the diff).
	reviewCommentsPath := fmt.Sprintf("repos/%s/pulls/%d/comments", repo, prNumber)
	reviewOut, reviewErrOut, reviewErr := runCommand(ctx, dir, ghBin, "api", reviewCommentsPath)
	if reviewErr != nil {
		detail := strings.TrimSpace(reviewErrOut)
		if detail == "" {
			detail = reviewErr.Error()
		}
		return PRFeedback{}, fmt.Errorf("collect PR review comments failed: %s", detail)
	}
	if err := appendPRInlineReviewComments(&feedback, reviewOut); err != nil {
		return PRFeedback{}, err
	}

	// Conversation / issue comments on the PR.
	issuePath := fmt.Sprintf("repos/%s/issues/%d/comments", repo, prNumber)
	issueOut, issueErrOut, issueErr := runCommand(ctx, dir, ghBin, "api", issuePath)
	if issueErr != nil {
		detail := strings.TrimSpace(issueErrOut)
		if detail == "" {
			detail = issueErr.Error()
		}
		return PRFeedback{}, fmt.Errorf("collect PR issue comments failed: %s", detail)
	}
	if err := appendPRIssueComments(&feedback, issueOut); err != nil {
		return PRFeedback{}, err
	}

	prioritizeHumanFeedback(&feedback)
	return feedback, nil
}

func appendPRReviewBodies(feedback *PRFeedback, raw string) error {
	var view struct {
		Reviews []struct {
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
			Body  string `json:"body"`
			State string `json:"state"`
		} `json:"reviews"`
	}
	if err := json.Unmarshal([]byte(raw), &view); err != nil {
		return fmt.Errorf("parse PR review payload: %w", err)
	}
	for _, review := range view.Reviews {
		body := strings.TrimSpace(review.Body)
		if body == "" {
			continue
		}
		author := strings.TrimSpace(review.Author.Login)
		if author == "" {
			author = "unknown"
		}
		if state := strings.TrimSpace(review.State); state != "" {
			body = fmt.Sprintf("(%s) %s", state, body)
		}
		feedback.Items = append(feedback.Items, PRFeedbackItem{
			Kind: "review", Author: author, Body: body,
		})
	}
	return nil
}

func appendPRInlineReviewComments(feedback *PRFeedback, raw string) error {
	var comments []struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Body string `json:"body"`
		Path string `json:"path"`
		Line int    `json:"line"`
		// GitHub may only populate original_line when the line moved.
		OriginalLine int `json:"original_line"`
	}
	if err := json.Unmarshal([]byte(raw), &comments); err != nil {
		return fmt.Errorf("parse PR review comments: %w", err)
	}
	for _, comment := range comments {
		body := strings.TrimSpace(comment.Body)
		if body == "" {
			continue
		}
		author := strings.TrimSpace(comment.User.Login)
		if author == "" {
			author = "unknown"
		}
		line := comment.Line
		if line == 0 {
			line = comment.OriginalLine
		}
		feedback.Items = append(feedback.Items, PRFeedbackItem{
			Kind: "review_comment", Author: author, Body: body, Path: comment.Path, Line: line,
		})
	}
	return nil
}

func appendPRIssueComments(feedback *PRFeedback, raw string) error {
	var issueComments []struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(raw), &issueComments); err != nil {
		return fmt.Errorf("parse PR issue comments: %w", err)
	}
	for _, comment := range issueComments {
		body := strings.TrimSpace(comment.Body)
		if body == "" {
			continue
		}
		author := strings.TrimSpace(comment.User.Login)
		if author == "" {
			author = "unknown"
		}
		feedback.Items = append(feedback.Items, PRFeedbackItem{
			Kind: "issue_comment", Author: author, Body: body,
		})
	}
	return nil
}

// prioritizeHumanFeedback sorts items so human/inline feedback is listed before
// auto-posted agent reviews (bodies starting with "## Agent review").
func prioritizeHumanFeedback(feedback *PRFeedback) {
	if len(feedback.Items) < 2 {
		return
	}
	sort.SliceStable(feedback.Items, func(i, j int) bool {
		return feedbackPriority(feedback.Items[i]) < feedbackPriority(feedback.Items[j])
	})
}

func feedbackPriority(item PRFeedbackItem) int {
	body := strings.TrimSpace(item.Body)
	if strings.HasPrefix(body, "## Agent review") || strings.Contains(body, "## Agent review\n") {
		return 3
	}
	switch item.Kind {
	case "review_comment":
		return 0 // inline code comments from humans are highest priority
	case "issue_comment":
		return 1
	case "review":
		return 2
	default:
		return 4
	}
}

func gitWorkingTreeDirty(ctx context.Context, dir string) (bool, error) {
	status, _, err := runCommand(ctx, dir, "git", "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(status) != "", nil
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
	fmt.Fprintf(&b, "## Agent review\n\n%s\n", strings.TrimSpace(review.Summary))
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

// runReviewerForStory runs the configured reviewer (Grok CLI or HTTP API).
func (a *App) runReviewerForStory(ctx context.Context, queueRunID int64, baseURL string, project Project, story Story, prompt, runKind string) (string, error) {
	runner, err := a.newReviewerRunner(context.Background())
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
	result, err := runner.Run(ctx, AgentRunRequest{
		Role:       AgentRunRoleReview,
		Prompt:     prompt,
		WorkingDir: project.WorkingDirectory,
		BaseURL:    baseURL,
		StoryID:    story.ID,
		Stdout:     output.stdoutWriter(),
		Stderr:     output.stderrWriter(),
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
		// Ensure secrets never land in stored error text.
		safeErr := a.redactKnownSecrets(err)
		_ = a.finishAgentStoryRun(context.Background(), agentRunID, status, stdoutText, stderrText, "", safeErr)
		if detail != "" {
			return "", fmt.Errorf("%w: %s", safeErr, truncate(detail, 800))
		}
		return "", safeErr
	}
	text := strings.TrimSpace(result.FinalMessage)
	if text == "" {
		// Grok CLI path may leave message only after extract; prefer final.
		var extractErr error
		text, extractErr = extractGrokJSONText(stdoutText)
		if extractErr != nil {
			_ = a.finishAgentStoryRun(context.Background(), agentRunID, "failed", stdoutText, stderrText, "", extractErr)
			return "", extractErr
		}
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
	conflict := 0
	if pipeline.MergeConflict {
		conflict = 1
	}
	_, err := a.db.ExecContext(ctx, `INSERT INTO story_pipelines (queue_run_id, story_id, phase, branch, default_branch, pr_number, pr_url, review_json, error, merge_conflict, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(queue_run_id, story_id) DO UPDATE SET
			phase = excluded.phase,
			branch = excluded.branch,
			default_branch = excluded.default_branch,
			pr_number = excluded.pr_number,
			pr_url = excluded.pr_url,
			review_json = excluded.review_json,
			error = excluded.error,
			merge_conflict = excluded.merge_conflict,
			updated_at = excluded.updated_at`,
		pipeline.QueueRunID, pipeline.StoryID, pipeline.Phase, pipeline.Branch, pipeline.DefaultBranch,
		pipeline.PRNumber, pipeline.PRURL, pipeline.ReviewJSON, pipeline.Error, conflict, now)
	return err
}

func (a *App) runStoryPipeline(ctx context.Context, pc pipelineContext, previousSummary string) (pipelineResult, error) {
	ghBin, err := resolveGhBinary()
	if err != nil {
		return pipelineResult{}, err
	}
	identity, err := a.getGitHubIdentity(ctx)
	if err != nil {
		return pipelineResult{}, err
	}
	dir := pc.Project.WorkingDirectory
	pipeline := StoryPipeline{
		QueueRunID: pc.QueueRunID,
		StoryID:    pc.Story.ID,
	}

	a.updateAgentProgress(pc.Story.ID, fmt.Sprintf("Preflight %s", pc.Story.ID), 0, 0)
	pipeline.Phase = PipelinePhasePreflight
	if err := a.upsertStoryPipeline(ctx, pipeline); err != nil {
		return pipelineResult{}, err
	}
	defaultBranch, err := gitPreflight(ctx, dir, pc.Project.DefaultBranchOverride)
	if err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		return pipelineResult{}, err
	}
	pc.DefaultBranch = defaultBranch
	pipeline.DefaultBranch = defaultBranch

	branch := storyBranchName(pc.Project, pc.Story)
	pc.Branch = branch
	pipeline.Branch = branch
	pipeline.Phase = PipelinePhaseBranching
	if err := a.upsertStoryPipeline(ctx, pipeline); err != nil {
		return pipelineResult{}, err
	}
	if err := gitCreateFeatureBranch(ctx, dir, defaultBranch, branch); err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		return pipelineResult{}, err
	}

	docs, err := embeddedFiles.ReadFile("docs/bot-api.md")
	if err != nil {
		return pipelineResult{}, err
	}
	pipeline.Phase = PipelinePhaseImplement
	_ = a.upsertStoryPipeline(ctx, pipeline)
	implementPrompt := buildCodexImplementPrompt(pc.BaseURL, string(docs), pc.Project, pc.Story, branch, previousSummary)
	finalMessage, err := a.runCodexForStoryWithKind(ctx, pc.QueueRunID, pc.BaseURL, pc.Project, pc.Story, implementPrompt, RunKindCodexImplement, branch, 0, "")
	if err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		return pipelineResult{}, err
	}

	pipeline.Phase = PipelinePhasePush
	_ = a.upsertStoryPipeline(ctx, pipeline)
	if err := gitCommitAll(ctx, dir, fmt.Sprintf("%s: %s", pc.Story.ID, pc.Story.Title), identity); err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		return pipelineResult{}, err
	}
	ahead, err := gitBranchAheadCount(ctx, dir, defaultBranch, branch)
	if err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		return pipelineResult{}, err
	}
	if ahead == 0 {
		err := errors.New("implementation produced no commits on the feature branch")
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		return pipelineResult{}, err
	}
	if err := gitPushBranch(ctx, dir, branch); err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		return pipelineResult{}, err
	}

	pipeline.Phase = PipelinePhaseCreatePR
	_ = a.upsertStoryPipeline(ctx, pipeline)
	prTitle := fmt.Sprintf("%s: %s", pc.Story.ID, pc.Story.Title)
	prBody := fmt.Sprintf("Automated PR for story **%s**.\n\n## Description\n\n%s", pc.Story.ID, pc.Story.Description)
	prBase := pc.Project.resolvePRBaseBranch(defaultBranch)
	prNumber, prURL, err := ghCreatePR(ctx, ghBin, dir, prBase, branch, prTitle, prBody, identity)
	if err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		return pipelineResult{}, err
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
		return pipelineResult{}, err
	}
	reviewPrompt := buildAgentReviewPrompt(pc.Story, prNumber, prURL, diff)
	reviewText, err := a.runReviewerForStory(ctx, pc.QueueRunID, pc.BaseURL, pc.Project, pc.Story, reviewPrompt, RunKindGrokReview)
	if err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		return pipelineResult{}, err
	}
	review, err := parseAgentReview(reviewText)
	if err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		return pipelineResult{}, err
	}
	reviewJSON, _ := json.Marshal(review)
	pipeline.ReviewJSON = string(reviewJSON)
	_ = a.upsertStoryPipeline(ctx, pipeline)
	if err := ghPRComment(ctx, ghBin, dir, prNumber, formatReviewForPR(review), identity); err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		return pipelineResult{}, err
	}

	// Supervised projects stop after PR + agent review. No auto-fix, quality gate, or merge.
	if normalizeAutonomyMode(pc.Project.AutonomyMode) == AutonomySupervised {
		return a.pausePipelineForHuman(ctx, pc, pipeline, finalMessage)
	}

	if reviewNeedsFix(review) {
		pipeline.Phase = PipelinePhaseFix
		_ = a.upsertStoryPipeline(ctx, pipeline)
		fixPrompt := buildCodexFixPrompt(pc.BaseURL, string(docs), pc.Project, pc.Story, branch, prNumber, prURL, string(reviewJSON))
		if _, err := a.runCodexForStoryWithKind(ctx, pc.QueueRunID, pc.BaseURL, pc.Project, pc.Story, fixPrompt, RunKindCodexFix, branch, prNumber, prURL); err != nil {
			pipeline.Error = err.Error()
			_ = a.upsertStoryPipeline(ctx, pipeline)
			return pipelineResult{}, err
		}
		if err := gitCommitAll(ctx, dir, fmt.Sprintf("%s: address review feedback", pc.Story.ID), identity); err != nil {
			pipeline.Error = err.Error()
			_ = a.upsertStoryPipeline(ctx, pipeline)
			return pipelineResult{}, err
		}
		if err := gitPushBranch(ctx, dir, branch); err != nil {
			pipeline.Error = err.Error()
			_ = a.upsertStoryPipeline(ctx, pipeline)
			return pipelineResult{}, err
		}
	}

	pipeline.Phase = PipelinePhaseQualityGate
	_ = a.upsertStoryPipeline(ctx, pipeline)
	if err := runQualityGate(ctx, dir); err != nil {
		if pc.Project.qualityGateIsWarn() {
			_ = a.addEvent(ctx, pc.Story.ID, eventQualityGateWarned, "Quality gate failed (warn mode); continuing to merge: "+err.Error())
		} else {
			pipeline.Error = err.Error()
			_ = a.upsertStoryPipeline(ctx, pipeline)
			return pipelineResult{}, err
		}
	}

	pipeline.Phase = PipelinePhaseMerge
	_ = a.upsertStoryPipeline(ctx, pipeline)
	if err := ghPRMerge(ctx, ghBin, dir, prNumber, pc.Project.DeleteBranchOnMerge); err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		return pipelineResult{}, err
	}
	_ = gitCheckoutBranch(ctx, dir, defaultBranch)
	if pc.Project.DeleteBranchOnMerge {
		_ = gitDeleteLocalBranch(ctx, dir, branch, defaultBranch)
	}

	pipeline.Phase = PipelinePhaseCompleted
	pipeline.Error = ""
	_ = a.upsertStoryPipeline(ctx, pipeline)
	return pipelineResult{FinalMessage: finalMessage}, nil
}

// pausePipelineForHuman stops a supervised delivery loop after agent review is posted.
// The story becomes in_review; the queue item is marked awaiting_human by the runner.
func (a *App) pausePipelineForHuman(ctx context.Context, pc pipelineContext, pipeline StoryPipeline, finalMessage string) (pipelineResult, error) {
	pipeline.Phase = PipelinePhaseAwaitingHuman
	pipeline.Error = ""
	if err := a.upsertStoryPipeline(ctx, pipeline); err != nil {
		return pipelineResult{}, err
	}
	if err := a.changeStoryStatus(ctx, pc.Story.ID, StatusInReview, false, "Awaiting human after agent review"); err != nil {
		return pipelineResult{}, err
	}
	eventMsg := "PR opened and agent review posted; waiting for human action"
	if strings.TrimSpace(pipeline.PRURL) != "" {
		eventMsg = fmt.Sprintf("PR opened and agent review posted; waiting for human action. %s", pipeline.PRURL)
	}
	if err := a.addEvent(ctx, pc.Story.ID, eventAwaitingHumanReview, eventMsg); err != nil {
		return pipelineResult{}, err
	}
	return pipelineResult{FinalMessage: finalMessage, AwaitingHuman: true}, nil
}

func (a *App) getStoryPipeline(ctx context.Context, queueRunID int64, storyID string) (StoryPipeline, error) {
	row := a.db.QueryRowContext(ctx, `SELECT id, queue_run_id, story_id, phase, branch, default_branch, pr_number, pr_url, review_json, error, COALESCE(merge_conflict, 0)
		FROM story_pipelines WHERE queue_run_id = ? AND story_id = ?`, queueRunID, storyID)
	return scanStoryPipeline(row)
}

func (a *App) getLatestStoryPipeline(ctx context.Context, storyID string) (StoryPipeline, error) {
	row := a.db.QueryRowContext(ctx, `SELECT id, queue_run_id, story_id, phase, branch, default_branch, pr_number, pr_url, review_json, error, COALESCE(merge_conflict, 0)
		FROM story_pipelines WHERE story_id = ? ORDER BY id DESC LIMIT 1`, storyID)
	return scanStoryPipeline(row)
}

func scanStoryPipeline(row rowScanner) (StoryPipeline, error) {
	var p StoryPipeline
	var conflict int
	if err := row.Scan(&p.ID, &p.QueueRunID, &p.StoryID, &p.Phase, &p.Branch, &p.DefaultBranch, &p.PRNumber, &p.PRURL, &p.ReviewJSON, &p.Error, &conflict); err != nil {
		return StoryPipeline{}, err
	}
	p.MergeConflict = conflict != 0
	return p, nil
}

// prepareAddressFeedback validates a supervised story and collects PR comments without starting an agent.
func (a *App) prepareAddressFeedback(ctx context.Context, storyID string) (Story, Project, StoryPipeline, PRFeedback, error) {
	story, err := a.getStory(ctx, storyID)
	if err != nil {
		return Story{}, Project{}, StoryPipeline{}, PRFeedback{}, err
	}
	if story.Status != StatusInReview {
		return Story{}, Project{}, StoryPipeline{}, PRFeedback{}, badRequest("Only stories in review can act on review comments")
	}
	project, err := a.getProject(ctx, story.ProjectID)
	if err != nil {
		return Story{}, Project{}, StoryPipeline{}, PRFeedback{}, err
	}
	if strings.TrimSpace(project.WorkingDirectory) == "" {
		return Story{}, Project{}, StoryPipeline{}, PRFeedback{}, badRequest("Set the project working directory before addressing feedback")
	}
	pipeline, err := a.getLatestStoryPipeline(ctx, story.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Story{}, Project{}, StoryPipeline{}, PRFeedback{}, badRequest("No pipeline/PR found for this story. Run the supervised queue first.")
		}
		return Story{}, Project{}, StoryPipeline{}, PRFeedback{}, err
	}
	if pipeline.PRNumber <= 0 || strings.TrimSpace(pipeline.PRURL) == "" {
		return Story{}, Project{}, StoryPipeline{}, PRFeedback{}, badRequest("This story does not have a pull request to collect comments from")
	}
	if strings.TrimSpace(pipeline.Branch) == "" {
		return Story{}, Project{}, StoryPipeline{}, PRFeedback{}, badRequest("This story is missing its feature branch on the pipeline record")
	}

	ghBin, err := resolveGhBinary()
	if err != nil {
		return Story{}, Project{}, StoryPipeline{}, PRFeedback{}, err
	}
	feedback, err := ghCollectPRFeedback(ctx, ghBin, project.WorkingDirectory, pipeline.PRNumber, pipeline.ReviewJSON)
	if err != nil {
		return Story{}, Project{}, StoryPipeline{}, PRFeedback{}, err
	}
	events, err := a.listEvents(ctx, story.ID)
	if err != nil {
		return Story{}, Project{}, StoryPipeline{}, PRFeedback{}, err
	}
	if err := evaluateAddressFeedback(feedback, events); err != nil {
		return Story{}, Project{}, StoryPipeline{}, PRFeedback{}, err
	}
	return story, project, pipeline, feedback, nil
}

// runAddressFeedback performs a supervised fix pass: agent edits, optional commit/push, back to in_review.
// No quality gate (locked D1). Caller must hold the global agent activity slot.
func (a *App) runAddressFeedback(ctx context.Context, baseURL string, story Story, project Project, pipeline StoryPipeline, feedback PRFeedback) error {
	if err := a.changeStoryStatus(ctx, story.ID, StatusInProgress, false, "Addressing review comments"); err != nil {
		return err
	}
	pipeline.Phase = PipelinePhaseAddressFeedback
	pipeline.Error = ""
	if err := a.upsertStoryPipeline(ctx, pipeline); err != nil {
		return err
	}
	if err := a.addEvent(ctx, story.ID, eventAddressingFeedback, fmt.Sprintf("Acting on review comments for PR #%d", pipeline.PRNumber)); err != nil {
		return err
	}
	a.publishAgentEvent("board")
	a.updateAgentProgress(story.ID, fmt.Sprintf("Addressing feedback for %s", story.ID), 0, 1)

	identity, err := a.getGitHubIdentity(ctx)
	if err != nil {
		return err
	}
	dir := project.WorkingDirectory
	if err := gitCheckoutBranch(ctx, dir, pipeline.Branch); err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		_ = a.changeStoryStatus(ctx, story.ID, StatusInReview, false, "Returned to review after address-feedback error")
		return err
	}

	docs, err := embeddedFiles.ReadFile("docs/bot-api.md")
	if err != nil {
		_ = a.changeStoryStatus(ctx, story.ID, StatusInReview, false, "Returned to review after address-feedback error")
		return err
	}
	prompt := buildCodexAddressFeedbackPrompt(baseURL, string(docs), project, story, pipeline.Branch, pipeline.PRNumber, pipeline.PRURL, feedback)
	finalMessage, err := a.runCodexForStoryWithKind(ctx, pipeline.QueueRunID, baseURL, project, story, prompt, RunKindCodexAddressFeedback, pipeline.Branch, pipeline.PRNumber, pipeline.PRURL)
	if err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		_ = a.changeStoryStatus(ctx, story.ID, StatusInReview, false, "Returned to review after address-feedback error")
		_ = a.addEvent(ctx, story.ID, "agent_failed", "Address feedback failed: "+err.Error())
		return err
	}

	dirty, err := gitWorkingTreeDirty(ctx, dir)
	if err != nil {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		_ = a.changeStoryStatus(ctx, story.ID, StatusInReview, false, "Returned to review after address-feedback error")
		return err
	}
	committed := false
	if dirty {
		if err := gitCommitAll(ctx, dir, fmt.Sprintf("%s: address review comments", story.ID), identity); err != nil {
			pipeline.Error = err.Error()
			_ = a.upsertStoryPipeline(ctx, pipeline)
			_ = a.changeStoryStatus(ctx, story.ID, StatusInReview, false, "Returned to review after address-feedback error")
			return err
		}
		committed = true
		if err := gitPushBranch(ctx, dir, pipeline.Branch); err != nil {
			pipeline.Error = err.Error()
			_ = a.upsertStoryPipeline(ctx, pipeline)
			_ = a.changeStoryStatus(ctx, story.ID, StatusInReview, false, "Returned to review after address-feedback error")
			return err
		}
	}

	return a.completeAddressFeedback(ctx, story, pipeline, feedback, committed, finalMessage)
}

func (a *App) completeAddressFeedback(ctx context.Context, story Story, pipeline StoryPipeline, feedback PRFeedback, committed bool, finalMessage string) error {
	pipeline.Phase = PipelinePhaseAwaitingHuman
	pipeline.Error = ""
	if err := a.upsertStoryPipeline(ctx, pipeline); err != nil {
		return err
	}
	if err := a.changeStoryStatus(ctx, story.ID, StatusInReview, false, "Awaiting human after addressing feedback"); err != nil {
		return err
	}
	fp := feedbackFingerprint(feedback)
	if committed {
		msg := "Review comments addressed and pushed; waiting for human again. " + feedbackFingerprintPrefix + fp
		if strings.TrimSpace(finalMessage) != "" {
			msg = truncate(finalMessage, 240) + " · " + feedbackFingerprintPrefix + fp
		}
		if err := a.addEvent(ctx, story.ID, eventFeedbackAddressed, msg); err != nil {
			return err
		}
	} else {
		msg := "Agent ran but produced no code changes; waiting for human again. " + feedbackFingerprintPrefix + fp
		if err := a.addEvent(ctx, story.ID, eventFeedbackNoChanges, msg); err != nil {
			return err
		}
	}
	a.publishAgentEvent("board")
	return nil
}

func buildCodexResolveConflictsPrompt(baseURL, botDocs string, project Project, story Story, branch, defaultBranch string, prNumber int, prURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are being run by Ripple to resolve merge conflicts on a supervised story.\n\n")
	fmt.Fprintf(&b, "# Situation\n\n")
	fmt.Fprintf(&b, "- Feature branch `%s` is in conflict with `%s` (the PR base).\n", branch, defaultBranch)
	fmt.Fprintf(&b, "- Ripple already ran `git merge` of the latest `%s` into this branch. Conflict markers are in the working tree.\n", defaultBranch)
	if prNumber > 0 {
		fmt.Fprintf(&b, "- Pull request: #%d %s\n", prNumber, prURL)
	}
	fmt.Fprintf(&b, "\n# Working Rules\n\n")
	fmt.Fprintf(&b, "- Stay on branch `%s`. Do not create or rename branches. Do not push. Do not merge the PR.\n", branch)
	fmt.Fprintf(&b, "- Open every conflicted file and resolve all `<<<<<<<`, `=======`, `>>>>>>>` markers.\n")
	fmt.Fprintf(&b, "- Keep the story's intended behavior while integrating the latest changes from `%s`.\n", defaultBranch)
	fmt.Fprintf(&b, "- Prefer preserving both sides when they do not truly conflict; never leave conflict markers in the tree.\n")
	fmt.Fprintf(&b, "- Run relevant tests/lint when practical. Do not start long-running servers.\n")
	fmt.Fprintf(&b, "- Leave resolved files as local changes. Ripple will stage, commit the merge, and push.\n")
	fmt.Fprintf(&b, "- If you cannot resolve a conflict safely, explain why in your final response and leave the tree as far along as possible.\n\n")
	fmt.Fprintf(&b, "# Ripple API\n\nBase URL: %s\n\nBot docs:\n\n%s\n\n", baseURL, botDocs)
	fmt.Fprintf(&b, "# Project\n\nName: %s\nWorking directory: %s\n\n", project.Name, project.WorkingDirectory)
	fmt.Fprintf(&b, "# Story\n\nID: %s\nTitle: %s\n\nDescription:\n%s\n", story.ID, story.Title, story.Description)
	return b.String()
}

// prepareResolveConflicts validates a supervised story can attempt conflict resolution.
func (a *App) prepareResolveConflicts(ctx context.Context, storyID string) (Story, Project, StoryPipeline, string, error) {
	story, err := a.getStory(ctx, storyID)
	if err != nil {
		return Story{}, Project{}, StoryPipeline{}, "", err
	}
	if story.Status != StatusInReview {
		return Story{}, Project{}, StoryPipeline{}, "", badRequest("Only stories in review can fix merge conflicts")
	}
	project, err := a.getProject(ctx, story.ProjectID)
	if err != nil {
		return Story{}, Project{}, StoryPipeline{}, "", err
	}
	if strings.TrimSpace(project.WorkingDirectory) == "" {
		return Story{}, Project{}, StoryPipeline{}, "", badRequest("Set the project working directory before fixing conflicts")
	}
	pipeline, err := a.getLatestStoryPipeline(ctx, story.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Story{}, Project{}, StoryPipeline{}, "", badRequest("No pipeline/PR found for this story. Run the supervised queue first.")
		}
		return Story{}, Project{}, StoryPipeline{}, "", err
	}
	if pipeline.PRNumber <= 0 || strings.TrimSpace(pipeline.PRURL) == "" {
		return Story{}, Project{}, StoryPipeline{}, "", badRequest("This story does not have a pull request to fix conflicts on")
	}
	if strings.TrimSpace(pipeline.Branch) == "" {
		return Story{}, Project{}, StoryPipeline{}, "", badRequest("This story is missing its feature branch on the pipeline record")
	}
	defaultBranch := strings.TrimSpace(pipeline.DefaultBranch)
	if defaultBranch == "" {
		if db, dbErr := project.resolveDefaultBranch(ctx, project.WorkingDirectory); dbErr == nil {
			defaultBranch = db
		} else {
			defaultBranch = "main"
		}
	}
	return story, project, pipeline, defaultBranch, nil
}

// runResolveConflicts merges the latest base into the feature branch and, if needed,
// runs the implementer to resolve conflict markers. Always returns the story to in_review.
func (a *App) runResolveConflicts(ctx context.Context, baseURL string, story Story, project Project, pipeline StoryPipeline, defaultBranch string) error {
	failBack := func(err error, eventMsg string) error {
		pipeline.Error = err.Error()
		_ = a.upsertStoryPipeline(ctx, pipeline)
		_ = a.changeStoryStatus(ctx, story.ID, StatusInReview, false, "Returned to review after conflict resolution error")
		if eventMsg != "" {
			_ = a.addEvent(ctx, story.ID, "agent_failed", eventMsg)
		}
		a.publishAgentEvent("board")
		return err
	}

	if err := a.changeStoryStatus(ctx, story.ID, StatusInProgress, false, "Resolving merge conflicts"); err != nil {
		return err
	}
	pipeline.Phase = PipelinePhaseResolveConflicts
	pipeline.Error = ""
	pipeline.DefaultBranch = defaultBranch
	if err := a.upsertStoryPipeline(ctx, pipeline); err != nil {
		return err
	}
	if err := a.addEvent(ctx, story.ID, eventResolvingConflicts, fmt.Sprintf("Merging latest %s into %s to fix conflicts for PR #%d", defaultBranch, pipeline.Branch, pipeline.PRNumber)); err != nil {
		return err
	}
	a.publishAgentEvent("board")
	a.updateAgentProgress(story.ID, fmt.Sprintf("Resolving conflicts for %s", story.ID), 0, 1)

	identity, idErr := a.getGitHubIdentity(ctx)
	if idErr != nil {
		return failBack(idErr, "Conflict resolution failed: "+idErr.Error())
	}
	dir := project.WorkingDirectory
	hadConflicts, err := gitMergeBaseIntoFeature(ctx, dir, pipeline.Branch, defaultBranch)
	if err != nil {
		return failBack(err, "Conflict merge failed: "+err.Error())
	}

	finishCleanMerge := func(agentResolved bool, message string) error {
		dirty, dirtyErr := gitWorkingTreeDirty(ctx, dir)
		if dirtyErr != nil {
			return failBack(dirtyErr, "Conflict check failed: "+dirtyErr.Error())
		}
		if dirty || gitMergeInProgress(ctx, dir) {
			if err := gitCompleteMergeCommit(ctx, dir, fmt.Sprintf("%s: resolve merge conflicts with %s", story.ID, defaultBranch), identity); err != nil {
				return failBack(err, "Conflict commit failed: "+err.Error())
			}
		}
		if err := gitPushBranch(ctx, dir, pipeline.Branch); err != nil {
			return failBack(err, "Push after conflict resolution failed: "+err.Error())
		}
		return a.completeResolveConflicts(ctx, story, pipeline, agentResolved, message)
	}

	if !hadConflicts {
		return finishCleanMerge(false, fmt.Sprintf("Merged latest %s without conflicts", defaultBranch))
	}

	// Index still lists unmerged paths, but a prior agent pass may already have
	// rewritten the files without staging. If markers are gone, just commit/push.
	markersLeft, markerErr := gitWorktreeHasConflictMarkers(ctx, dir)
	if markerErr != nil {
		return failBack(markerErr, "Conflict check failed: "+markerErr.Error())
	}
	if !markersLeft {
		return finishCleanMerge(true, fmt.Sprintf("Staged previously resolved conflicts with %s and pushed", defaultBranch))
	}

	docs, err := embeddedFiles.ReadFile("docs/bot-api.md")
	if err != nil {
		return failBack(err, "Conflict resolution failed: "+err.Error())
	}
	prompt := buildCodexResolveConflictsPrompt(baseURL, string(docs), project, story, pipeline.Branch, defaultBranch, pipeline.PRNumber, pipeline.PRURL)
	finalMessage, err := a.runCodexForStoryWithKind(ctx, pipeline.QueueRunID, baseURL, project, story, prompt, RunKindCodexResolveConflicts, pipeline.Branch, pipeline.PRNumber, pipeline.PRURL)
	if err != nil {
		return failBack(err, "Conflict resolution agent failed: "+err.Error())
	}

	// Source of truth after the agent: working-tree conflict markers (not the unmerged index).
	markersLeft, markerErr = gitWorktreeHasConflictMarkers(ctx, dir)
	if markerErr != nil {
		return failBack(markerErr, "Conflict check failed: "+markerErr.Error())
	}
	if markersLeft {
		return failBack(errors.New("unresolved conflict markers remain after the agent pass"), "Conflict resolution incomplete: unresolved markers remain in files")
	}

	msg := strings.TrimSpace(finalMessage)
	if msg == "" {
		msg = fmt.Sprintf("Resolved conflicts with %s and pushed", defaultBranch)
	}
	return finishCleanMerge(true, msg)
}

func (a *App) completeResolveConflicts(ctx context.Context, story Story, pipeline StoryPipeline, agentResolved bool, message string) error {
	pipeline.Phase = PipelinePhaseAwaitingHuman
	pipeline.Error = ""
	pipeline.MergeConflict = false
	if err := a.upsertStoryPipeline(ctx, pipeline); err != nil {
		return err
	}
	if err := a.changeStoryStatus(ctx, story.ID, StatusInReview, false, "Awaiting human after conflict resolution"); err != nil {
		return err
	}
	eventMsg := strings.TrimSpace(message)
	if eventMsg == "" {
		if agentResolved {
			eventMsg = "Merge conflicts resolved and pushed; waiting for human again"
		} else {
			eventMsg = "Base branch merged cleanly; waiting for human again"
		}
	}
	if err := a.addEvent(ctx, story.ID, eventConflictsResolved, eventMsg); err != nil {
		return err
	}
	a.publishAgentEvent("board")
	return nil
}

// Test hooks for human merge (production defaults; overridden in tests).
var (
	humanMergeQualityGate = runQualityGate
	humanMergePR          = func(ctx context.Context, ghBin, dir string, prNumber int, deleteRemoteBranch bool) error {
		return ghPRMerge(ctx, ghBin, dir, prNumber, deleteRemoteBranch)
	}
)

const eventMergedByHuman = "merged_by_human"
const eventMergedExternally = "merged_externally"
const eventQualityGateFailed = "quality_gate_failed"

// Test hook for external PR merge detection (production default; overridden in tests).
var checkPRMerged = ghPRIsMerged
var checkPRMergeConflicts = ghPRHasMergeConflicts
var afterMergeConflictProbeDelay = 1500 * time.Millisecond
var mergeConflictRetryDelay = 1 * time.Second

// prepareHumanMerge validates a supervised story is ready for human merge.
func (a *App) prepareHumanMerge(ctx context.Context, storyID string) (Story, Project, StoryPipeline, error) {
	story, err := a.getStory(ctx, storyID)
	if err != nil {
		return Story{}, Project{}, StoryPipeline{}, err
	}
	if story.Status != StatusInReview {
		return Story{}, Project{}, StoryPipeline{}, badRequest("Only stories in review can be merged from Ripple")
	}
	project, err := a.getProject(ctx, story.ProjectID)
	if err != nil {
		return Story{}, Project{}, StoryPipeline{}, err
	}
	if strings.TrimSpace(project.WorkingDirectory) == "" {
		return Story{}, Project{}, StoryPipeline{}, badRequest("Set the project working directory before merging")
	}
	pipeline, err := a.getLatestStoryPipeline(ctx, story.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Story{}, Project{}, StoryPipeline{}, badRequest("No pipeline/PR found for this story. Run the supervised queue first.")
		}
		return Story{}, Project{}, StoryPipeline{}, err
	}
	if pipeline.PRNumber <= 0 {
		return Story{}, Project{}, StoryPipeline{}, badRequest("This story does not have a pull request to merge")
	}
	if pipeline.MergeConflict {
		return Story{}, Project{}, StoryPipeline{}, badRequest("This pull request has merge conflicts. Use Fix merge conflicts, then try merge again.")
	}
	return story, project, pipeline, nil
}

// executeHumanMerge runs the quality gate, merges the PR, cleans up the local branch, and marks the story done.
// Quality gate is required (locked D1). On gate or merge failure the story stays in_review.
func (a *App) executeHumanMerge(ctx context.Context, story Story, project Project, pipeline StoryPipeline) error {
	dir := project.WorkingDirectory
	ghBin, err := resolveGhBinary()
	if err != nil {
		return err
	}

	defaultBranch := strings.TrimSpace(pipeline.DefaultBranch)
	if defaultBranch == "" {
		defaultBranch, err = project.resolveDefaultBranch(ctx, dir)
		if err != nil {
			return err
		}
		pipeline.DefaultBranch = defaultBranch
	}

	if strings.TrimSpace(pipeline.Branch) != "" {
		if err := gitCheckoutBranch(ctx, dir, pipeline.Branch); err != nil {
			// Best-effort: quality gate can still run on whatever is checked out if branch is gone.
			// Prefer failing clearly so humans fix the workspace.
			return fmt.Errorf("checkout feature branch %s: %w", pipeline.Branch, err)
		}
	}

	a.updateAgentProgress(story.ID, fmt.Sprintf("Quality gate for %s", story.ID), 0, 1)
	pipeline.Phase = PipelinePhaseQualityGate
	pipeline.Error = ""
	if err := a.upsertStoryPipeline(ctx, pipeline); err != nil {
		return err
	}
	if err := humanMergeQualityGate(ctx, dir); err != nil {
		if project.qualityGateIsWarn() {
			_ = a.addEvent(ctx, story.ID, eventQualityGateWarned, "Quality gate failed (warn mode); continuing to merge: "+err.Error())
		} else {
			pipeline.Error = err.Error()
			pipeline.Phase = PipelinePhaseAwaitingHuman
			_ = a.upsertStoryPipeline(ctx, pipeline)
			_ = a.addEvent(ctx, story.ID, eventQualityGateFailed, "Quality gate failed before merge: "+err.Error())
			a.publishAgentEvent("board")
			return badRequest("Quality gate failed; story stays in review. " + truncate(err.Error(), 400))
		}
	}

	a.updateAgentProgress(story.ID, fmt.Sprintf("Merging PR for %s", story.ID), 0, 1)
	pipeline.Phase = PipelinePhaseMerge
	pipeline.Error = ""
	if err := a.upsertStoryPipeline(ctx, pipeline); err != nil {
		return err
	}

	// Re-check mergeability right before gh merge — another PR may have landed since the page loaded.
	if conflicted, detail, err := checkPRMergeConflicts(ctx, ghBin, dir, pipeline.PRNumber); err != nil {
		_ = a.addEvent(ctx, story.ID, "merge_failed", "Mergeability check failed: "+err.Error())
		return badRequest("Could not verify PR mergeability. " + truncate(err.Error(), 300))
	} else if conflicted {
		pipeline.MergeConflict = true
		pipeline.Phase = PipelinePhaseAwaitingHuman
		pipeline.Error = detail
		_ = a.upsertStoryPipeline(ctx, pipeline)
		_ = a.addEvent(ctx, story.ID, eventMergeConflictDetected, detail)
		a.publishAgentEvent("board")
		return badRequest("Pull request has merge conflicts. Use Fix merge conflicts, then try merge again.")
	}

	if err := humanMergePR(ctx, ghBin, dir, pipeline.PRNumber, project.DeleteBranchOnMerge); err != nil {
		pipeline.Error = err.Error()
		pipeline.Phase = PipelinePhaseAwaitingHuman
		if looksLikeMergeConflictError(err.Error()) {
			pipeline.MergeConflict = true
			_ = a.addEvent(ctx, story.ID, eventMergeConflictDetected, "Merge blocked by conflicts: "+err.Error())
		} else {
			_ = a.addEvent(ctx, story.ID, "merge_failed", "Merge failed: "+err.Error())
		}
		_ = a.upsertStoryPipeline(ctx, pipeline)
		a.publishAgentEvent("board")
		if pipeline.MergeConflict {
			return badRequest("Pull request has merge conflicts. Use Fix merge conflicts, then try merge again.")
		}
		return badRequest("Merge failed; story stays in review. " + truncate(err.Error(), 400))
	}

	if strings.TrimSpace(pipeline.Branch) != "" {
		_ = gitCheckoutBranch(ctx, dir, defaultBranch)
		if project.DeleteBranchOnMerge {
			_ = gitDeleteLocalBranch(ctx, dir, pipeline.Branch, defaultBranch)
		}
	} else {
		_ = gitCheckoutBranch(ctx, dir, defaultBranch)
	}

	return a.completeHumanMerge(ctx, story, pipeline)
}

func (a *App) completeHumanMerge(ctx context.Context, story Story, pipeline StoryPipeline) error {
	msg := "Pull request merged by human; story marked done"
	if pipeline.PRNumber > 0 {
		msg = fmt.Sprintf("PR #%d merged by human; story marked done", pipeline.PRNumber)
		if strings.TrimSpace(pipeline.PRURL) != "" {
			msg += ". " + pipeline.PRURL
		}
	}
	return a.finalizeMergedStory(ctx, story, pipeline, eventMergedByHuman, "Merged by human", msg)
}

// syncExternalPRMerge marks an in_review story done when its PR was already merged outside Ripple.
// Explicit user control only (locked §2.7) — does not rewrite status on page load.
func (a *App) syncExternalPRMerge(ctx context.Context, storyID string) error {
	story, err := a.getStory(ctx, storyID)
	if err != nil {
		return err
	}
	if story.Status != StatusInReview {
		return badRequest("Only stories in review can sync PR status")
	}
	project, err := a.getProject(ctx, story.ProjectID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(project.WorkingDirectory) == "" {
		return badRequest("Set the project working directory before syncing PR status")
	}
	pipeline, err := a.getLatestStoryPipeline(ctx, story.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return badRequest("No pipeline/PR found for this story. Run the supervised queue first.")
		}
		return err
	}
	if pipeline.PRNumber <= 0 {
		return badRequest("This story does not have a pull request to sync")
	}

	ghBin, err := resolveGhBinary()
	if err != nil {
		return err
	}
	merged, err := checkPRMerged(ctx, ghBin, project.WorkingDirectory, pipeline.PRNumber)
	if err != nil {
		return err
	}
	if !merged {
		return badRequest("Pull request is not merged on GitHub yet. Merge it there, or use Merge pull request in Ripple.")
	}

	// Best-effort local cleanup after external merge (same as supervised merge).
	defaultBranch := strings.TrimSpace(pipeline.DefaultBranch)
	if defaultBranch == "" {
		if db, dbErr := project.resolveDefaultBranch(ctx, project.WorkingDirectory); dbErr == nil {
			defaultBranch = db
			pipeline.DefaultBranch = defaultBranch
		}
	}
	if defaultBranch != "" {
		_ = gitCheckoutBranch(ctx, project.WorkingDirectory, defaultBranch)
		if project.DeleteBranchOnMerge && strings.TrimSpace(pipeline.Branch) != "" {
			_ = gitDeleteLocalBranch(ctx, project.WorkingDirectory, pipeline.Branch, defaultBranch)
		}
	}

	msg := "Pull request was already merged on GitHub; story marked done"
	if pipeline.PRNumber > 0 {
		msg = fmt.Sprintf("PR #%d was already merged on GitHub; story marked done", pipeline.PRNumber)
		if strings.TrimSpace(pipeline.PRURL) != "" {
			msg += ". " + pipeline.PRURL
		}
	}
	return a.finalizeMergedStory(ctx, story, pipeline, eventMergedExternally, "Synced after external merge", msg)
}

// finalizeMergedStory marks pipeline completed, story done, records the merge event, and updates the queue item.
func (a *App) finalizeMergedStory(ctx context.Context, story Story, pipeline StoryPipeline, eventType, statusMessage, eventMsg string) error {
	pipeline.Phase = PipelinePhaseCompleted
	pipeline.Error = ""
	pipeline.MergeConflict = false
	if err := a.upsertStoryPipeline(ctx, pipeline); err != nil {
		return err
	}
	if err := a.changeStoryStatus(ctx, story.ID, StatusDone, false, statusMessage); err != nil {
		return err
	}
	if err := a.addEvent(ctx, story.ID, eventType, eventMsg); err != nil {
		return err
	}
	// Best-effort: if this story is still awaiting_human on its queue run, mark completed.
	if pipeline.QueueRunID > 0 {
		_ = a.updateQueueRunItemStatus(ctx, pipeline.QueueRunID, story.ID, QueueItemCompleted)
		_ = a.refreshAwaitingPRConflicts(ctx, pipeline.QueueRunID, story.ProjectID)
	}
	a.publishAgentEvent("board")
	return nil
}

// refreshAwaitingPRConflicts checks remaining awaiting-human PRs in a queue run for merge conflicts
// (typically after another story in the same run was merged into the base branch).
func (a *App) refreshAwaitingPRConflicts(ctx context.Context, queueRunID int64, projectID string) error {
	if queueRunID <= 0 {
		return nil
	}
	items, err := a.listQueueRunItems(ctx, queueRunID)
	if err != nil {
		return err
	}
	project, err := a.getProject(ctx, projectID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(project.WorkingDirectory) == "" {
		return nil
	}
	ghBin, err := resolveGhBinary()
	if err != nil {
		return err
	}

	awaiting := make([]QueueRunItem, 0)
	for _, item := range items {
		if item.Status == QueueItemAwaitingHuman {
			awaiting = append(awaiting, item)
		}
	}
	if len(awaiting) == 0 {
		return nil
	}

	// GitHub often returns UNKNOWN briefly after another PR lands; give it a moment, then retry.
	if afterMergeConflictProbeDelay > 0 {
		time.Sleep(afterMergeConflictProbeDelay)
	}

	for _, item := range awaiting {
		pipeline, err := a.getStoryPipeline(ctx, queueRunID, item.Story.ID)
		if err != nil || pipeline.PRNumber <= 0 {
			continue
		}
		conflicted, detail, err := checkPRMergeConflictsWithRetry(ctx, ghBin, project.WorkingDirectory, pipeline.PRNumber)
		if err != nil {
			continue
		}
		if pipeline.MergeConflict == conflicted {
			continue
		}
		pipeline.MergeConflict = conflicted
		if conflicted {
			if detail == "" {
				detail = "Pull request has merge conflicts with the base branch"
			}
			pipeline.Error = detail
			_ = a.addEvent(ctx, item.Story.ID, eventMergeConflictDetected, detail+". Merge is disabled until conflicts are fixed.")
		} else if strings.Contains(strings.ToLower(pipeline.Error), "conflict") {
			pipeline.Error = ""
		}
		_ = a.upsertStoryPipeline(ctx, pipeline)
	}
	a.publishAgentEvent("activity")
	return nil
}

func checkPRMergeConflictsWithRetry(ctx context.Context, ghBin, dir string, prNumber int) (bool, string, error) {
	if mergeConflictRetryDelay <= 0 {
		return checkPRMergeConflicts(ctx, ghBin, dir, prNumber)
	}
	for attempt := 0; attempt < 4; attempt++ {
		conflicted, unknown, detail, err := inspectPRMergeConflicts(ctx, ghBin, dir, prNumber)
		if err != nil {
			return false, "", err
		}
		if !unknown {
			return conflicted, detail, nil
		}
		if attempt == 3 {
			break
		}
		select {
		case <-ctx.Done():
			return false, "", ctx.Err()
		case <-time.After(time.Duration(attempt+1) * mergeConflictRetryDelay):
		}
	}
	return false, "", nil
}

// inspectPRMergeConflicts returns conflicted/unknown from GitHub's mergeable fields.
func inspectPRMergeConflicts(ctx context.Context, ghBin, dir string, prNumber int) (conflicted, unknown bool, detail string, err error) {
	stdout, stderr, err := runCommand(ctx, dir, ghBin, "pr", "view", fmt.Sprintf("%d", prNumber), "--json", "mergeable,mergeStateStatus")
	if err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = err.Error()
		}
		return false, false, "", fmt.Errorf("check PR mergeability failed: %s", detail)
	}
	var view struct {
		Mergeable        string `json:"mergeable"`
		MergeStateStatus string `json:"mergeStateStatus"`
	}
	if err := json.Unmarshal([]byte(stdout), &view); err != nil {
		return false, false, "", fmt.Errorf("parse PR mergeability: %w", err)
	}
	mergeable := strings.ToUpper(strings.TrimSpace(view.Mergeable))
	state := strings.ToUpper(strings.TrimSpace(view.MergeStateStatus))
	if mergeable == "UNKNOWN" || state == "UNKNOWN" || mergeable == "" {
		return false, true, "", nil
	}
	if mergeable == "CONFLICTING" || state == "DIRTY" {
		detail := "Pull request has merge conflicts with the base branch"
		if state != "" {
			detail = fmt.Sprintf("%s (GitHub: %s / %s)", detail, mergeable, state)
		}
		return true, false, detail, nil
	}
	return false, false, "", nil
}

func timeNowUTC() time.Time {
	return time.Now().UTC()
}
