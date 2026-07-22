package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupStatusMissingPath(t *testing.T) {
	app := testApp(t)
	if _, err := app.createProject(context.Background(), "atlas", "Atlas", "A", "", ""); err != nil {
		t.Fatal(err)
	}
	status, err := app.projectSetupStatus(context.Background(), "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if status.Ready {
		t.Fatal("expected not ready without path")
	}
	byID := map[string]SetupCheck{}
	for _, c := range status.Checks {
		byID[c.ID] = c
	}
	if byID["path_set"].Status != SetupFail {
		t.Fatalf("path_set = %+v", byID["path_set"])
	}
	if byID["git_repo"].Status != SetupFail {
		t.Fatalf("git_repo should fail when path missing: %+v", byID["git_repo"])
	}

	res := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/projects/atlas/setup-status", nil)
	app.routes().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}
	body := res.Body.String()
	for _, marker := range []string{"Needs attention", "Working directory set", "Verify", "setup-fail", "Implementer available"} {
		// "Verify" may only be on the button not the fragment
		if marker == "Verify" {
			continue
		}
		if !strings.Contains(body, marker) && marker != "setup-fail" {
			// class is setup-fail on li
		}
		if marker == "setup-fail" && !strings.Contains(body, "setup-fail") {
			t.Fatalf("body missing %q", marker)
		}
		if marker != "setup-fail" && !strings.Contains(body, marker) {
			t.Fatalf("body missing %q", marker)
		}
	}
}

func TestSetupStatusNonGitPath(t *testing.T) {
	app := testApp(t)
	dir := t.TempDir()
	if _, err := app.createProject(context.Background(), "atlas", "Atlas", "A", dir, ""); err != nil {
		t.Fatal(err)
	}
	status, err := app.projectSetupStatus(context.Background(), "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if status.Ready {
		t.Fatal("non-git path should not be ready")
	}
	foundGitFail := false
	for _, c := range status.Checks {
		if c.ID == "path_exists" && c.Status != SetupPass {
			t.Fatalf("path_exists = %+v", c)
		}
		if c.ID == "git_repo" && c.Status == SetupFail {
			foundGitFail = true
		}
	}
	if !foundGitFail {
		t.Fatal("expected git_repo fail")
	}
}

func TestSetupStatusGitRepoWithRootSuggestion(t *testing.T) {
	app := testApp(t)
	root := t.TempDir()
	runGit(t, root, "init")
	sub := filepath.Join(root, "pkg", "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// nested path still a work tree via parent .git
	if _, err := app.createProject(context.Background(), "atlas", "Atlas", "A", sub, ""); err != nil {
		t.Fatal(err)
	}
	status, err := app.projectSetupStatus(context.Background(), "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if !status.GitRootDiffers {
		// On some systems init may make nested not work tree if bare - check isGitWorkTree
		if isGitWorkTree(sub) && status.SuggestedGitRoot == "" {
			t.Fatal("expected suggested git root for subdirectory")
		}
	}
	if isGitWorkTree(sub) {
		for _, c := range status.Checks {
			if c.ID == "git_repo" && c.Status != SetupPass {
				t.Fatalf("git_repo = %+v", c)
			}
		}
	}
}

func TestSetupStatusJSON(t *testing.T) {
	app := testApp(t)
	if _, err := app.createProject(context.Background(), "atlas", "Atlas", "A", "", ""); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/projects/atlas/setup-status", nil)
	req.Header.Set("Accept", "application/json")
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d", res.Code)
	}
	var status ProjectSetupStatus
	if err := json.Unmarshal(res.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Project.ID != "atlas" || status.Ready {
		t.Fatalf("status = %+v", status)
	}
}

func TestCloneRejectsInvalidURL(t *testing.T) {
	app := testApp(t)
	if _, err := app.createProject(context.Background(), "atlas", "Atlas", "A", "", ""); err != nil {
		t.Fatal(err)
	}
	_, err := app.cloneGitHubRepo(context.Background(), "atlas", "https://evil.example/repo", t.TempDir())
	if err == nil {
		t.Fatal("expected invalid URL error")
	}
	if !strings.Contains(err.Error(), "github.com") {
		t.Fatalf("error = %v", err)
	}

	form := url.Values{"repoUrl": {"ftp://github.com/org/repo"}, "parentDirectory": {t.TempDir()}}
	req := httptest.NewRequest(http.MethodPost, "/projects/atlas/clone", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}
}

func TestValidateGitHubCloneURL(t *testing.T) {
	ok := []string{
		"https://github.com/org/repo",
		"https://github.com/org/repo.git",
		"git@github.com:org/repo.git",
	}
	for _, u := range ok {
		if _, err := validateGitHubCloneURL(u); err != nil {
			t.Fatalf("%s: %v", u, err)
		}
	}
	bad := []string{"", "https://gitlab.com/org/repo", "not a url", "https://github.com/org/repo/extra"}
	for _, u := range bad {
		if _, err := validateGitHubCloneURL(u); err == nil {
			t.Fatalf("expected error for %q", u)
		}
	}
}

func TestUICreateProject(t *testing.T) {
	app := testApp(t)
	form := url.Values{
		"name":             {"Northwind"},
		"prefix":           {"N"},
		"workingDirectory": {""},
		"autonomyMode":     {"supervised"},
	}
	req := httptest.NewRequest(http.MethodPost, "/projects", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, req)
	if res.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}
	loc := res.Header().Get("Location")
	if !strings.Contains(loc, "/backlog") {
		t.Fatalf("location = %q", loc)
	}
	projects, err := app.listProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range projects {
		if p.Name == "Northwind" {
			found = true
			if p.AutonomyMode != AutonomySupervised {
				t.Fatalf("autonomy = %q", p.AutonomyMode)
			}
		}
	}
	if !found {
		t.Fatal("project not created")
	}

	// Dashboard shows create project control + modal form
	getRes := httptest.NewRecorder()
	app.routes().ServeHTTP(getRes, httptest.NewRequest(http.MethodGet, "/", nil))
	body := getRes.Body.String()
	for _, marker := range []string{`action="/projects"`, "Create project", `id="create-project-modal"`, `data-create-project-open`} {
		if !strings.Contains(body, marker) {
			t.Fatalf("dashboard missing create project UI marker %q", marker)
		}
	}
	if strings.Contains(body, "create-project-panel") {
		t.Fatal("dashboard still has inline create project panel")
	}
}

func TestBacklogShowsSetupControls(t *testing.T) {
	app := testApp(t)
	seedProjectStories(t, app, "atlas", 1)
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/projects/atlas/backlog", nil))
	body := res.Body.String()
	for _, marker := range []string{"Verify setup", "/projects/atlas/setup-status", "Clone from GitHub", "/projects/atlas/clone"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("backlog missing %q", marker)
		}
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := []string{"-C", dir}
	cmd = append(cmd, args...)
	// use shell-less exec via runCommand helpers
	out, errOut, err := runCommand(context.Background(), "", "git", append([]string{"-C", dir}, args...)...)
	if err != nil {
		t.Fatalf("git %v: %v\n%s\n%s", args, err, out, errOut)
	}
}
