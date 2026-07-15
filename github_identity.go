package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"time"
)

// GitHub identity modes (hybrid by default: PR/comments as you, commits as Ripple + co-author).
const (
	GitHubActorMe  = "me"
	GitHubActorBot = "bot"

	GitHubCommitMe            = "me"
	GitHubCommitRipple        = "ripple"
	GitHubCommitRippleCoauthor = "ripple_coauthor"

	DefaultRippleCommitName  = "Ripple"
	DefaultRippleCommitEmail = "ripple[bot]@users.noreply.github.com"
)

// GitHubIdentityConfig is stored as JSON on app_config.github_identity_json.
type GitHubIdentityConfig struct {
	// PRAuthorMode: me | bot — who opens pull requests.
	PRAuthorMode string `json:"prAuthorMode"`
	// CommitMode: me | ripple | ripple_coauthor — git author for Ripple-made commits.
	CommitMode string `json:"commitMode"`
	// CommentMode: me | bot — who posts agent review comments.
	CommentMode string `json:"commentMode"`
	// BotToken is a GitHub PAT (or App token) for bot-attributed gh actions. Never logged.
	BotToken string `json:"botToken,omitempty"`
	// Ripple commit identity (used when CommitMode is ripple*).
	CommitName  string `json:"commitName"`
	CommitEmail string `json:"commitEmail"`
	// Optional human identity for Co-authored-by (falls back to git config in the repo).
	HumanName  string `json:"humanName"`
	HumanEmail string `json:"humanEmail"`
}

// GitHubIdentityView is safe for settings UI (token masked).
type GitHubIdentityView struct {
	PRAuthorMode  string
	CommitMode    string
	CommentMode   string
	CommitName    string
	CommitEmail   string
	HumanName     string
	HumanEmail    string
	HasBotToken   bool
	MaskedToken   string
	Saved         bool
	Flash         string
	NeedsBotToken bool // true when a mode requires bot but token missing
}

func defaultGitHubIdentity() GitHubIdentityConfig {
	return GitHubIdentityConfig{
		PRAuthorMode: GitHubActorMe,
		CommitMode:   GitHubCommitRippleCoauthor,
		CommentMode:  GitHubActorMe,
		CommitName:   DefaultRippleCommitName,
		CommitEmail:  DefaultRippleCommitEmail,
	}
}

func normalizeGitHubIdentity(c GitHubIdentityConfig) GitHubIdentityConfig {
	d := defaultGitHubIdentity()
	switch strings.TrimSpace(c.PRAuthorMode) {
	case GitHubActorMe, GitHubActorBot:
		d.PRAuthorMode = strings.TrimSpace(c.PRAuthorMode)
	}
	switch strings.TrimSpace(c.CommitMode) {
	case GitHubCommitMe, GitHubCommitRipple, GitHubCommitRippleCoauthor:
		d.CommitMode = strings.TrimSpace(c.CommitMode)
	}
	switch strings.TrimSpace(c.CommentMode) {
	case GitHubActorMe, GitHubActorBot:
		d.CommentMode = strings.TrimSpace(c.CommentMode)
	}
	if name := strings.TrimSpace(c.CommitName); name != "" {
		d.CommitName = name
	}
	if email := strings.TrimSpace(c.CommitEmail); email != "" {
		d.CommitEmail = email
	}
	d.HumanName = strings.TrimSpace(c.HumanName)
	d.HumanEmail = strings.TrimSpace(c.HumanEmail)
	d.BotToken = strings.TrimSpace(c.BotToken)
	return d
}

func (c GitHubIdentityConfig) requiresBotToken() bool {
	return c.PRAuthorMode == GitHubActorBot || c.CommentMode == GitHubActorBot
}

func (c GitHubIdentityConfig) maskedBotToken() string {
	if strings.TrimSpace(c.BotToken) == "" {
		return ""
	}
	return "••••••••"
}

func (a *App) ensureGitHubIdentityColumn(ctx context.Context) error {
	return a.ensureColumn(ctx, "app_config", "github_identity_json", "TEXT NOT NULL DEFAULT '{}'")
}

func (a *App) getGitHubIdentity(ctx context.Context) (GitHubIdentityConfig, error) {
	if err := a.ensureGitHubIdentityColumn(ctx); err != nil {
		return GitHubIdentityConfig{}, err
	}
	var raw string
	err := a.db.QueryRowContext(ctx, `SELECT github_identity_json FROM app_config WHERE id = 1`).Scan(&raw)
	if err == sql.ErrNoRows {
		if seedErr := a.seedDefaultAgentSettings(ctx); seedErr != nil {
			return GitHubIdentityConfig{}, seedErr
		}
		return a.getGitHubIdentity(ctx)
	}
	if err != nil {
		// Column missing mid-migration path.
		if strings.Contains(err.Error(), "no such column") {
			if ensureErr := a.ensureGitHubIdentityColumn(ctx); ensureErr != nil {
				return GitHubIdentityConfig{}, ensureErr
			}
			return defaultGitHubIdentity(), nil
		}
		return GitHubIdentityConfig{}, err
	}
	var c GitHubIdentityConfig
	_ = json.Unmarshal([]byte(strings.TrimSpace(raw)), &c)
	return normalizeGitHubIdentity(c), nil
}

func (a *App) saveGitHubIdentity(ctx context.Context, c GitHubIdentityConfig) error {
	if err := a.ensureGitHubIdentityColumn(ctx); err != nil {
		return err
	}
	c = normalizeGitHubIdentity(c)
	if c.requiresBotToken() && c.BotToken == "" {
		return badRequest("Bot token is required when PR author or agent comments use the Ripple bot identity. Add a GitHub PAT for the bot account, or switch those modes to “Me”.")
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = a.db.ExecContext(ctx, `UPDATE app_config SET github_identity_json = ?, updated_at = ? WHERE id = 1`, string(raw), now)
	return err
}

func (a *App) saveGitHubIdentityFromForm(ctx context.Context, form map[string]string) error {
	cur, err := a.getGitHubIdentity(ctx)
	if err != nil {
		return err
	}
	next := GitHubIdentityConfig{
		PRAuthorMode: form["prAuthorMode"],
		CommitMode:   form["commitMode"],
		CommentMode:  form["commentMode"],
		CommitName:   form["commitName"],
		CommitEmail:  form["commitEmail"],
		HumanName:    form["humanName"],
		HumanEmail:   form["humanEmail"],
		BotToken:     cur.BotToken, // keep existing unless replaced
	}
	if tok := strings.TrimSpace(form["botToken"]); tok != "" {
		next.BotToken = tok
	}
	if form["clearBotToken"] == "1" {
		next.BotToken = ""
	}
	return a.saveGitHubIdentity(ctx, next)
}

func (a *App) githubIdentityView(ctx context.Context, flash string) (GitHubIdentityView, error) {
	c, err := a.getGitHubIdentity(ctx)
	if err != nil {
		return GitHubIdentityView{}, err
	}
	v := GitHubIdentityView{
		PRAuthorMode:  c.PRAuthorMode,
		CommitMode:    c.CommitMode,
		CommentMode:   c.CommentMode,
		CommitName:    c.CommitName,
		CommitEmail:   c.CommitEmail,
		HumanName:     c.HumanName,
		HumanEmail:    c.HumanEmail,
		HasBotToken:   c.BotToken != "",
		MaskedToken:   c.maskedBotToken(),
		Saved:         flash == "github_saved",
		Flash:         flash,
		NeedsBotToken: c.requiresBotToken() && c.BotToken == "",
	}
	return v, nil
}

// commitEnv returns env vars for git commit author/committer. Empty means use repo defaults.
func (c GitHubIdentityConfig) commitEnv(ctx context.Context, dir string) []string {
	switch c.CommitMode {
	case GitHubCommitMe:
		return nil
	case GitHubCommitRipple, GitHubCommitRippleCoauthor:
		name := c.CommitName
		email := c.CommitEmail
		if name == "" {
			name = DefaultRippleCommitName
		}
		if email == "" {
			email = DefaultRippleCommitEmail
		}
		return []string{
			"GIT_AUTHOR_NAME=" + name,
			"GIT_AUTHOR_EMAIL=" + email,
			"GIT_COMMITTER_NAME=" + name,
			"GIT_COMMITTER_EMAIL=" + email,
		}
	default:
		return nil
	}
}

func (c GitHubIdentityConfig) decorateCommitMessage(ctx context.Context, dir, message string) string {
	if c.CommitMode != GitHubCommitRippleCoauthor {
		return message
	}
	name, email := c.resolveHumanIdentity(ctx, dir)
	if name == "" || email == "" {
		return message
	}
	// Avoid duplicating if already present.
	trailer := "Co-authored-by: " + name + " <" + email + ">"
	if strings.Contains(message, trailer) {
		return message
	}
	return strings.TrimRight(message, "\n") + "\n\n" + trailer + "\n"
}

func (c GitHubIdentityConfig) resolveHumanIdentity(ctx context.Context, dir string) (name, email string) {
	name = strings.TrimSpace(c.HumanName)
	email = strings.TrimSpace(c.HumanEmail)
	if name == "" {
		if out, _, err := runCommand(ctx, dir, "git", "config", "user.name"); err == nil {
			name = strings.TrimSpace(out)
		}
	}
	if email == "" {
		if out, _, err := runCommand(ctx, dir, "git", "config", "user.email"); err == nil {
			email = strings.TrimSpace(out)
		}
	}
	return name, email
}

// ghEnv returns extra env for gh when this action should use the bot token.
func (c GitHubIdentityConfig) ghEnvFor(actorMode string) []string {
	if actorMode != GitHubActorBot {
		return nil
	}
	tok := strings.TrimSpace(c.BotToken)
	if tok == "" {
		return nil
	}
	return []string{"GH_TOKEN=" + tok, "GITHUB_TOKEN=" + tok}
}

func (c GitHubIdentityConfig) validateBotFor(actorMode, action string) error {
	if actorMode != GitHubActorBot {
		return nil
	}
	if strings.TrimSpace(c.BotToken) == "" {
		return badRequest("GitHub bot token is not configured. Open Settings → GitHub identity, or set this action to “Me”.")
	}
	return nil
}

// mergeEnv overlays extra KEY=val pairs onto the process environment for a child command.
func mergeEnv(extra []string) []string {
	if len(extra) == 0 {
		return nil // signal: use default env
	}
	base := os.Environ()
	// Later entries win for exec; append overrides.
	return append(append([]string{}, base...), extra...)
}
