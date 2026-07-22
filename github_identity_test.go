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

func TestDefaultGitHubIdentityHybrid(t *testing.T) {
	c := defaultGitHubIdentity()
	if c.PRAuthorMode != GitHubActorMe || c.CommentMode != GitHubActorMe {
		t.Fatalf("PR/comments should default to me: %+v", c)
	}
	if c.CommitMode != GitHubCommitRippleCoauthor {
		t.Fatalf("commits should default to ripple_coauthor: %+v", c)
	}
	if c.CommitName != DefaultRippleCommitName || c.CommitEmail != DefaultRippleCommitEmail {
		t.Fatalf("commit identity defaults: %+v", c)
	}
}

func TestGitHubIdentitySaveAndLoad(t *testing.T) {
	app := testApp(t)
	cfg := GitHubIdentityConfig{
		PRAuthorMode: GitHubActorMe,
		CommitMode:   GitHubCommitRipple,
		CommentMode:  GitHubActorMe,
		CommitName:   "Ripple Agent",
		CommitEmail:  "ripple@example.com",
	}
	if err := app.saveGitHubIdentity(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	got, err := app.getGitHubIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.CommitMode != GitHubCommitRipple || got.CommitName != "Ripple Agent" {
		t.Fatalf("got %+v", got)
	}
}

func TestGitHubIdentityBotRequiresToken(t *testing.T) {
	app := testApp(t)
	err := app.saveGitHubIdentity(context.Background(), GitHubIdentityConfig{
		PRAuthorMode: GitHubActorBot,
		CommitMode:   GitHubCommitMe,
		CommentMode:  GitHubActorMe,
	})
	if err == nil || !strings.Contains(err.Error(), "Bot token") {
		t.Fatalf("expected bot token error, got %v", err)
	}
}

func TestDecorateCommitMessageCoauthor(t *testing.T) {
	dir := t.TempDir()
	mustRunGit(t, dir, "init")
	mustRunGit(t, dir, "config", "user.email", "ada@example.com")
	mustRunGit(t, dir, "config", "user.name", "Ada")
	c := defaultGitHubIdentity()
	msg := c.decorateCommitMessage(context.Background(), dir, "CODE-001: work")
	if !strings.Contains(msg, "Co-authored-by: Ada <ada@example.com>") {
		t.Fatalf("message = %q", msg)
	}
	// me mode leaves message alone
	c.CommitMode = GitHubCommitMe
	if got := c.decorateCommitMessage(context.Background(), dir, "x"); got != "x" {
		t.Fatalf("me mode = %q", got)
	}
}

func TestCommitEnvRipple(t *testing.T) {
	c := defaultGitHubIdentity()
	c.CommitMode = GitHubCommitRipple
	env := c.commitEnv(context.Background(), "")
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "GIT_AUTHOR_NAME=Ripple") || !strings.Contains(joined, "GIT_AUTHOR_EMAIL="+DefaultRippleCommitEmail) {
		t.Fatalf("env = %#v", env)
	}
	c.CommitMode = GitHubCommitMe
	if c.commitEnv(context.Background(), "") != nil {
		t.Fatal("me mode should not set author env")
	}
}

func TestGitCommitAllUsesRippleAuthor(t *testing.T) {
	dir := t.TempDir()
	mustRunGit(t, dir, "init")
	mustRunGit(t, dir, "config", "user.email", "human@example.com")
	mustRunGit(t, dir, "config", "user.name", "Human")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi\n"), 0600); err != nil {
		t.Fatal(err)
	}
	id := defaultGitHubIdentity()
	id.CommitMode = GitHubCommitRipple
	id.CommitName = "Ripple"
	id.CommitEmail = "ripple[bot]@users.noreply.github.com"
	if err := gitCommitAll(context.Background(), dir, "agent work", id); err != nil {
		t.Fatal(err)
	}
	out, _, err := runCommand(context.Background(), dir, "git", "log", "-1", "--format=%an <%ae>%n%B")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Ripple <ripple[bot]@users.noreply.github.com>") {
		t.Fatalf("author not Ripple: %s", out)
	}
}

func TestSettingsPageShowsGitHubIdentity(t *testing.T) {
	app := testApp(t)
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d", res.Code)
	}
	body := res.Body.String()
	for _, marker := range []string{
		`id="github-identity"`,
		"GitHub identity",
		`name="prAuthorMode"`,
		`name="commitMode"`,
		`name="commentMode"`,
		`action="/settings/github-identity"`,
		"ripple_coauthor",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("settings missing %q", marker)
		}
	}
}

func TestSaveGitHubIdentityForm(t *testing.T) {
	app := testApp(t)
	form := url.Values{
		"prAuthorMode": {"me"},
		"commitMode":   {"ripple"},
		"commentMode":  {"me"},
		"commitName":   {"Ripple Bot"},
		"commitEmail":  {"bot@example.com"},
	}
	req := httptest.NewRequest(http.MethodPost, "/settings/github-identity", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, req)
	if res.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}
	if loc := res.Header().Get("Location"); !strings.Contains(loc, "github_saved") {
		t.Fatalf("location = %q", loc)
	}
	got, err := app.getGitHubIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.CommitMode != GitHubCommitRipple || got.CommitName != "Ripple Bot" {
		t.Fatalf("got %+v", got)
	}
}
