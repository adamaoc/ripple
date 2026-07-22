package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	SetupPass = "pass"
	SetupWarn = "warn"
	SetupFail = "fail"
)

// SetupCheck is one row in the project readiness checklist.
type SetupCheck struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Status string `json:"status"` // pass | warn | fail
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

// ProjectSetupStatus is the full readiness report for a project workspace.
type ProjectSetupStatus struct {
	Project          Project      `json:"project"`
	Checks           []SetupCheck `json:"checks"`
	Ready            bool         `json:"ready"`
	Summary          string       `json:"summary"`
	SuggestedGitRoot string       `json:"suggestedGitRoot,omitempty"`
	GitRootDiffers   bool         `json:"gitRootDiffers"`
}

var githubCloneURLPattern = regexp.MustCompile(`(?i)^(https://github\.com/[\w.-]+/[\w.-]+(?:\.git)?/?|git@github\.com:[\w.-]+/[\w.-]+(?:\.git)?)$`)

func (a *App) projectSetupStatus(ctx context.Context, projectID string) (ProjectSetupStatus, error) {
	project, err := a.getProject(ctx, projectID)
	if err != nil {
		return ProjectSetupStatus{}, err
	}
	status := ProjectSetupStatus{Project: project}
	dir := strings.TrimSpace(project.WorkingDirectory)

	// 1. Path set
	if dir == "" {
		status.Checks = append(status.Checks, SetupCheck{
			ID: "path_set", Label: "Working directory set", Status: SetupFail,
			Detail: "No path configured for this project.",
			Hint:   "Choose a folder in Project settings, or clone a GitHub repo below.",
		})
		status.Checks = append(status.Checks, skippedPathChecks("set a working directory first")...)
	} else {
		status.Checks = append(status.Checks, SetupCheck{
			ID: "path_set", Label: "Working directory set", Status: SetupPass,
			Detail: dir,
		})

		// 2. Path exists
		info, statErr := os.Stat(dir)
		if statErr != nil || !info.IsDir() {
			status.Checks = append(status.Checks, SetupCheck{
				ID: "path_exists", Label: "Path exists", Status: SetupFail,
				Detail: "Directory does not exist or is not a folder.",
				Hint:   "Pick an existing repository root on this machine.",
			})
			status.Checks = append(status.Checks, skippedGitChecks("path must exist first")...)
		} else {
			status.Checks = append(status.Checks, SetupCheck{
				ID: "path_exists", Label: "Path exists", Status: SetupPass,
				Detail: "Directory is present on disk.",
			})

			// 3. Git repo + optional git root suggestion
			if !isGitWorkTree(dir) {
				status.Checks = append(status.Checks, SetupCheck{
					ID: "git_repo", Label: "Git repository", Status: SetupFail,
					Detail: "This folder is not a git work tree.",
					Hint:   "Point at a repository root (the folder that contains .git), or clone a repo.",
				})
				status.Checks = append(status.Checks, skippedGitChecksAfterRepo()...)
			} else {
				gitRoot, rootErr := detectGitRoot(ctx, dir)
				if rootErr == nil && gitRoot != "" {
					absDir, _ := filepath.Abs(dir)
					absRoot, _ := filepath.Abs(gitRoot)
					if absDir != absRoot {
						status.SuggestedGitRoot = absRoot
						status.GitRootDiffers = true
					}
				}
				detail := "Git work tree detected."
				if status.GitRootDiffers {
					detail = fmt.Sprintf("Git work tree detected (repo root is %s).", status.SuggestedGitRoot)
				}
				status.Checks = append(status.Checks, SetupCheck{
					ID: "git_repo", Label: "Git repository", Status: SetupPass, Detail: detail,
					Hint: ternary(status.GitRootDiffers, "Prefer the repository root so agent commands run at the project top level.", ""),
				})

				// 4. GitHub remote
				remote, remoteOK, remoteDetail := inspectGitHubRemote(ctx, dir)
				if remoteOK {
					status.Checks = append(status.Checks, SetupCheck{
						ID: "github_remote", Label: "GitHub remote", Status: SetupPass,
						Detail: remoteDetail,
					})
				} else {
					status.Checks = append(status.Checks, SetupCheck{
						ID: "github_remote", Label: "GitHub remote", Status: SetupFail,
						Detail: remoteDetail,
						Hint:   "Add an origin remote that points at GitHub, e.g. git remote add origin https://github.com/org/repo.git",
					})
				}
				_ = remote

				// 5. Clean tree (warn only)
				dirty, dirtyErr := gitWorkingTreeDirty(ctx, dir)
				if dirtyErr != nil {
					status.Checks = append(status.Checks, SetupCheck{
						ID: "clean_tree", Label: "Working tree clean", Status: SetupWarn,
						Detail: "Could not check git status: " + dirtyErr.Error(),
						Hint:   "Runs require a clean tree before the queue starts.",
					})
				} else if dirty {
					status.Checks = append(status.Checks, SetupCheck{
						ID: "clean_tree", Label: "Working tree clean", Status: SetupWarn,
						Detail: "There are uncommitted local changes.",
						Hint:   "Commit, stash, or discard changes before running the queue (preflight will block otherwise).",
					})
				} else {
					status.Checks = append(status.Checks, SetupCheck{
						ID: "clean_tree", Label: "Working tree clean", Status: SetupPass,
						Detail: "No uncommitted changes.",
					})
				}
			}
		}
	}

	// 6. Implementer
	if impl, err := a.resolveImplementer(ctx); err != nil {
		status.Checks = append(status.Checks, SetupCheck{
			ID: "implementer", Label: "Implementer available", Status: SetupFail,
			Detail: err.Error(),
			Hint:   "Open Settings → Agents and configure Codex CLI (path or env RIPPLE_CODEX_BIN).",
		})
	} else {
		status.Checks = append(status.Checks, SetupCheck{
			ID: "implementer", Label: "Implementer available", Status: SetupPass,
			Detail: fmt.Sprintf("%s · %s", impl.ProviderName, impl.BinaryPath),
		})
	}

	// 7. Reviewer
	if rev, err := a.resolveReviewer(ctx); err != nil {
		status.Checks = append(status.Checks, SetupCheck{
			ID: "reviewer", Label: "Reviewer available", Status: SetupFail,
			Detail: err.Error(),
			Hint:   "Open Settings → Agents and configure Grok CLI or an API reviewer.",
		})
	} else {
		detail := rev.ProviderName
		if rev.Kind == ProviderKindAPI {
			detail = fmt.Sprintf("%s · %s · %s", rev.ProviderName, rev.API.BaseURL, rev.API.Model)
		} else if rev.BinaryPath != "" {
			detail = fmt.Sprintf("%s · %s", rev.ProviderName, rev.BinaryPath)
		}
		status.Checks = append(status.Checks, SetupCheck{
			ID: "reviewer", Label: "Reviewer available", Status: SetupPass, Detail: detail,
		})
	}

	// 8. gh auth
	ghPath, ghErr := resolveGhBinary()
	if ghErr != nil {
		status.Checks = append(status.Checks, SetupCheck{
			ID: "gh_auth", Label: "GitHub CLI authenticated", Status: SetupFail,
			Detail: ghErr.Error(),
			Hint:   "Install gh and run `gh auth login`, or set RIPPLE_GH_BIN.",
		})
	} else if err := checkGhAuth(ctx, ghPath); err != nil {
		status.Checks = append(status.Checks, SetupCheck{
			ID: "gh_auth", Label: "GitHub CLI authenticated", Status: SetupFail,
			Detail: err.Error(),
			Hint:   "Run `gh auth login` in a terminal, then verify again.",
		})
	} else {
		status.Checks = append(status.Checks, SetupCheck{
			ID: "gh_auth", Label: "GitHub CLI authenticated", Status: SetupPass,
			Detail: "gh is installed and authenticated (" + ghPath + ").",
		})
	}

	failCount, warnCount := 0, 0
	for _, c := range status.Checks {
		switch c.Status {
		case SetupFail:
			failCount++
		case SetupWarn:
			warnCount++
		}
	}
	status.Ready = failCount == 0
	switch {
	case status.Ready && warnCount == 0:
		status.Summary = "Ready to run — all checks passed."
	case status.Ready:
		status.Summary = fmt.Sprintf("Ready with %d warning(s). Queue preflight may still require a clean tree.", warnCount)
	default:
		status.Summary = fmt.Sprintf("%d check(s) need attention before this project can run reliably.", failCount)
	}
	return status, nil
}

func skippedPathChecks(reason string) []SetupCheck {
	return []SetupCheck{
		{ID: "path_exists", Label: "Path exists", Status: SetupFail, Detail: "Skipped — " + reason + ".", Hint: "Configure the project path."},
		{ID: "git_repo", Label: "Git repository", Status: SetupFail, Detail: "Skipped — " + reason + ".", Hint: "Configure the project path."},
		{ID: "github_remote", Label: "GitHub remote", Status: SetupFail, Detail: "Skipped — " + reason + ".", Hint: "Configure the project path."},
		{ID: "clean_tree", Label: "Working tree clean", Status: SetupWarn, Detail: "Skipped — " + reason + ".", Hint: "Configure the project path."},
	}
}

func skippedGitChecks(reason string) []SetupCheck {
	return []SetupCheck{
		{ID: "git_repo", Label: "Git repository", Status: SetupFail, Detail: "Skipped — " + reason + ".", Hint: "Fix the path first."},
		{ID: "github_remote", Label: "GitHub remote", Status: SetupFail, Detail: "Skipped — " + reason + ".", Hint: "Fix the path first."},
		{ID: "clean_tree", Label: "Working tree clean", Status: SetupWarn, Detail: "Skipped — " + reason + ".", Hint: "Runs require a clean tree; preflight enforces this."},
	}
}

func skippedGitChecksAfterRepo() []SetupCheck {
	return []SetupCheck{
		{ID: "github_remote", Label: "GitHub remote", Status: SetupFail, Detail: "Skipped until the path is a git repo.", Hint: "Fix the git repository check first."},
		{ID: "clean_tree", Label: "Working tree clean", Status: SetupWarn, Detail: "Skipped until the path is a git repo.", Hint: "Runs require a clean tree; preflight enforces this."},
	}
}

func detectGitRoot(ctx context.Context, dir string) (string, error) {
	stdout, stderr, err := runCommand(ctx, dir, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("%s", detail)
	}
	root := strings.TrimSpace(stdout)
	if root == "" {
		return "", fmt.Errorf("empty git root")
	}
	return root, nil
}

func inspectGitHubRemote(ctx context.Context, dir string) (remote string, ok bool, detail string) {
	stdout, _, err := runCommand(ctx, dir, "git", "remote", "get-url", "origin")
	if err != nil {
		// Try any remote
		list, _, listErr := runCommand(ctx, dir, "git", "remote", "-v")
		if listErr != nil || strings.TrimSpace(list) == "" {
			return "", false, "No git remotes configured."
		}
		return "", false, "No origin remote; remotes found but origin is required for PR workflow."
	}
	remote = strings.TrimSpace(stdout)
	if remote == "" {
		return "", false, "origin remote URL is empty."
	}
	if isGitHubRemoteURL(remote) {
		return remote, true, remote
	}
	// Still usable if gh can see the repo
	if ghBin, ghErr := resolveGhBinary(); ghErr == nil {
		if _, _, viewErr := runCommand(ctx, dir, ghBin, "repo", "view", "--json", "nameWithOwner", "-q", ".nameWithOwner"); viewErr == nil {
			return remote, true, remote + " (visible via gh)"
		}
	}
	return remote, false, "origin is not a GitHub remote: " + remote
}

func isGitHubRemoteURL(remote string) bool {
	remote = strings.TrimSpace(remote)
	lower := strings.ToLower(remote)
	if strings.HasPrefix(lower, "git@github.com:") {
		return true
	}
	if strings.Contains(lower, "github.com/") {
		return true
	}
	return false
}

func checkGhAuth(ctx context.Context, ghBin string) error {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cmd := osexec.CommandContext(ctx, ghBin, "auth", "status")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", truncate(msg, 300))
	}
	return nil
}

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// validateGitHubCloneURL accepts https://github.com/org/repo(.git) or git@github.com:org/repo(.git).
func validateGitHubCloneURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", badRequest("repository URL is required")
	}
	// Normalize trailing slash
	raw = strings.TrimRight(raw, "/")
	if !githubCloneURLPattern.MatchString(raw) {
		return "", badRequest("only github.com HTTPS or SSH clone URLs are allowed (e.g. https://github.com/org/repo)")
	}
	// Strip credentials if someone pastes them
	if strings.HasPrefix(strings.ToLower(raw), "https://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", badRequest("invalid repository URL")
		}
		if !strings.EqualFold(u.Host, "github.com") {
			return "", badRequest("only github.com URLs are allowed")
		}
		u.User = nil
		raw = u.String()
	}
	return raw, nil
}

func repoNameFromCloneURL(raw string) string {
	raw = strings.TrimSuffix(strings.TrimSpace(raw), ".git")
	raw = strings.TrimRight(raw, "/")
	if i := strings.LastIndex(raw, "/"); i >= 0 {
		return raw[i+1:]
	}
	if i := strings.LastIndex(raw, ":"); i >= 0 {
		return raw[i+1:]
	}
	return "repo"
}

func defaultCloneParent() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "RippleWorkspaces"), nil
}

// sanitizeCloneParent ensures parent is an absolute existing-or-creatable directory under home (or absolute path without ..).
func sanitizeCloneParent(parent string) (string, error) {
	parent = strings.TrimSpace(parent)
	if parent == "" {
		return defaultCloneParent()
	}
	parent, err := expandUserPath(parent)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(parent)
	if err != nil {
		return "", err
	}
	// Reject path components that escape via ".."
	clean := filepath.Clean(abs)
	if strings.Contains(clean, "..") {
		return "", badRequest("invalid parent directory")
	}
	return clean, nil
}

func (a *App) cloneGitHubRepo(ctx context.Context, projectID, repoURL, parentDir string) (string, error) {
	project, err := a.getProject(ctx, projectID)
	if err != nil {
		return "", err
	}
	repoURL, err = validateGitHubCloneURL(repoURL)
	if err != nil {
		return "", err
	}
	parent, err := sanitizeCloneParent(parentDir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", badRequest("could not create parent directory: " + err.Error())
	}
	name := repoNameFromCloneURL(repoURL)
	if name == "" || name == "." || name == ".." {
		return "", badRequest("could not derive repository folder name")
	}
	target := filepath.Join(parent, name)
	// Ensure target is strictly inside parent
	rel, err := filepath.Rel(parent, target)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", badRequest("invalid clone target path")
	}
	if info, err := os.Stat(target); err == nil {
		if info.IsDir() {
			// Already cloned — use it if git
			if isGitWorkTree(target) {
				if err := a.updateProjectWorkingDirectory(ctx, project.ID, target); err != nil {
					return "", err
				}
				return target, nil
			}
			return "", badRequest("target folder already exists and is not a git repository: " + target)
		}
		return "", badRequest("target path exists and is not a directory: " + target)
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	stdout, stderr, err := runCommand(ctx, parent, "git", "clone", repoURL, name)
	if err != nil {
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = strings.TrimSpace(stdout)
		}
		if detail == "" {
			detail = err.Error()
		}
		return "", badRequest("git clone failed: " + truncate(detail, 500))
	}
	if err := a.updateProjectWorkingDirectory(ctx, project.ID, target); err != nil {
		return "", err
	}
	return target, nil
}

func (a *App) useGitRootForProject(ctx context.Context, projectID string) (string, error) {
	project, err := a.getProject(ctx, projectID)
	if err != nil {
		return "", err
	}
	dir := strings.TrimSpace(project.WorkingDirectory)
	if dir == "" {
		return "", badRequest("set a working directory first")
	}
	if !isGitWorkTree(dir) {
		return "", badRequest("working directory is not a git repository")
	}
	root, err := detectGitRoot(ctx, dir)
	if err != nil {
		return "", badRequest("could not detect git root: " + err.Error())
	}
	if err := a.updateProjectWorkingDirectory(ctx, projectID, root); err != nil {
		return "", err
	}
	return root, nil
}
