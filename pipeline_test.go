package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseCreatedPRURL(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   int
		url    string
	}{
		{
			name:   "github URL",
			output: "https://github.com/acme/widgets/pull/42\n",
			want:   42,
			url:    "https://github.com/acme/widgets/pull/42",
		},
		{
			name:   "enterprise URL after informational output",
			output: "Creating pull request for feature into main in acme/widgets\n\nhttps://github.example.com/acme/widgets/pull/123\n",
			want:   123,
			url:    "https://github.example.com/acme/widgets/pull/123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, gotURL, err := parseCreatedPRURL(tt.output)
			if err != nil {
				t.Fatalf("parseCreatedPRURL() error = %v", err)
			}
			if got != tt.want || gotURL != tt.url {
				t.Fatalf("parseCreatedPRURL() = (%d, %q), want (%d, %q)", got, gotURL, tt.want, tt.url)
			}
		})
	}
}

func TestParseCreatedPRURLRejectsUnexpectedOutput(t *testing.T) {
	for _, output := range []string{"", "not-a-url", "https://github.com/acme/widgets/issues/42", "https://github.com/acme/widgets/pull/nope"} {
		if _, _, err := parseCreatedPRURL(output); err == nil {
			t.Fatalf("parseCreatedPRURL(%q) unexpectedly succeeded", output)
		}
	}
}

func TestStoryBranchNameTemplate(t *testing.T) {
	project := Project{Prefix: "A", BranchNameTemplate: "feat/{prefix}/{id}-{slug}"}
	story := Story{ID: "A-001", Title: "Hello World", ProjectPrefix: "A"}
	got := storyBranchName(project, story)
	if got != "feat/A/A-001-hello-world" {
		t.Fatalf("branch = %q", got)
	}
	// Empty / invalid template falls back to default pattern.
	project.BranchNameTemplate = "no-id-here"
	got = storyBranchName(project, story)
	if !strings.HasPrefix(got, "ripple/A-001-") {
		t.Fatalf("fallback branch = %q", got)
	}
}

func TestResolvePRBaseAndDefaultBranch(t *testing.T) {
	p := Project{DefaultBranchOverride: "develop", PRBaseBranch: ""}
	if p.resolvePRBaseBranch("main") != "develop" && p.resolvePRBaseBranch(p.DefaultBranchOverride) != "develop" {
		// resolvePRBase with empty pr base uses argument
	}
	if got := p.resolvePRBaseBranch("main"); got != "main" {
		// PR base empty → use passed default branch string
		t.Fatalf("pr base = %q want main (explicit default branch arg)", got)
	}
	p.PRBaseBranch = "release"
	if got := p.resolvePRBaseBranch("main"); got != "release" {
		t.Fatalf("pr base = %q", got)
	}
	// Override used when resolving default without git (unit-level)
	if strings.TrimSpace(p.DefaultBranchOverride) != "develop" {
		t.Fatal("override missing")
	}
}

func TestRunKindSource(t *testing.T) {
	tests := map[string]string{
		RunKindCodexImplement:       "implementer",
		RunKindCodexFix:             "implementer",
		RunKindCodexAddressFeedback: "implementer",
		RunKindGrokReview:           "reviewer",
		"":                          "implementer",
	}
	for kind, want := range tests {
		if got := runKindSource(kind); got != want {
			t.Errorf("runKindSource(%q) = %q, want %q", kind, got, want)
		}
	}
}

func TestPRFeedbackActionableAndFingerprint(t *testing.T) {
	empty := PRFeedback{}
	if empty.HasActionableComments() {
		t.Fatal("empty feedback should not be actionable")
	}
	onlyWhitespace := PRFeedback{Items: []PRFeedbackItem{{Kind: "review", Body: "  "}}}
	if onlyWhitespace.HasActionableComments() {
		t.Fatal("whitespace-only body should not be actionable")
	}
	withComment := PRFeedback{Items: []PRFeedbackItem{{Kind: "review_comment", Author: "ada", Body: "Please fix the nil check", Path: "main.go", Line: 10}}}
	if !withComment.HasActionableComments() {
		t.Fatal("comment body should be actionable")
	}
	agentOnly := PRFeedback{AgentReviewJSON: `{"approved":false,"summary":"needs tests","comments":[{"path":"a.go","line":1,"body":"add test"}]}`}
	if !agentOnly.HasActionableComments() {
		t.Fatal("agent review with content should be actionable")
	}
	fp1 := feedbackFingerprint(withComment)
	fp2 := feedbackFingerprint(withComment)
	if fp1 == "" || fp1 != fp2 {
		t.Fatalf("fingerprint unstable: %q vs %q", fp1, fp2)
	}
	changed := withComment
	changed.Items[0].Body = "Please fix the nil check and add a test"
	if feedbackFingerprint(changed) == fp1 {
		t.Fatal("fingerprint should change when comments change")
	}
}

func TestEvaluateAddressFeedback(t *testing.T) {
	fb := PRFeedback{Items: []PRFeedbackItem{{Kind: "issue_comment", Author: "bob", Body: "Rename this helper"}}}
	if err := evaluateAddressFeedback(fb, nil); err != nil {
		t.Fatalf("first pass should accept feedback: %v", err)
	}
	fp := feedbackFingerprint(fb)
	events := []StoryEvent{{Type: eventFeedbackAddressed, Message: "done " + feedbackFingerprintPrefix + fp}}
	err := evaluateAddressFeedback(fb, events)
	if err == nil || !strings.Contains(err.Error(), "No new review comments") {
		t.Fatalf("expected no-new-comments error, got %v", err)
	}
	err = evaluateAddressFeedback(PRFeedback{}, nil)
	if err == nil || !strings.Contains(err.Error(), "No review comments found") {
		t.Fatalf("expected no-comments error, got %v", err)
	}
}

func TestBuildCodexAddressFeedbackPromptPrioritizesHuman(t *testing.T) {
	fb := PRFeedback{
		Items: []PRFeedbackItem{
			{Kind: "issue_comment", Author: "human", Body: "Please rename Foo to Bar"},
		},
		AgentReviewJSON: `{"approved":false,"summary":"agent note","comments":[]}`,
	}
	prompt := buildCodexAddressFeedbackPrompt("http://localhost:8080", "docs", Project{Name: "Atlas", WorkingDirectory: "/tmp"}, Story{ID: "A-001", Title: "T", Description: "D"}, "ripple/A-001-t", 7, "https://example.com/pull/7", fb)
	for _, marker := range []string{
		"Prioritize **human** review comments",
		"Please rename Foo to Bar",
		"secondary context",
		"agent note",
		"Do not merge the PR",
		"PR #7",
	} {
		if !strings.Contains(prompt, marker) {
			t.Fatalf("prompt missing %q", marker)
		}
	}
}

func TestAppendPRInlineReviewComments(t *testing.T) {
	// REST /pulls/{n}/comments shape (inline review comments).
	raw := `[
		{"user":{"login":"adamaoc"},"body":"let's change the title here to \"Ripple Todo\"","path":"todo-rip/index.html","line":12,"original_line":12},
		{"user":{"login":"bot"},"body":"","path":"x.go","line":1}
	]`
	var fb PRFeedback
	if err := appendPRInlineReviewComments(&fb, raw); err != nil {
		t.Fatal(err)
	}
	if len(fb.Items) != 1 {
		t.Fatalf("items = %#v, want 1 non-empty comment", fb.Items)
	}
	got := fb.Items[0]
	if got.Kind != "review_comment" || got.Author != "adamaoc" || got.Path != "todo-rip/index.html" || got.Line != 12 {
		t.Fatalf("item = %#v", got)
	}
	if !strings.Contains(got.Body, "Ripple Todo") {
		t.Fatalf("body = %q", got.Body)
	}
}

func TestAppendPRInlineReviewCommentsUsesOriginalLine(t *testing.T) {
	// line omitted / zero — use original_line
	raw := `[{"user":{"login":"ada"},"body":"nit","path":"a.go","original_line":8}]`
	var fb PRFeedback
	if err := appendPRInlineReviewComments(&fb, raw); err != nil {
		t.Fatal(err)
	}
	if len(fb.Items) != 1 || fb.Items[0].Line != 8 {
		t.Fatalf("items = %#v", fb.Items)
	}
}

func TestPrioritizeHumanFeedbackOrdersInlineFirst(t *testing.T) {
	fb := PRFeedback{
		Items: []PRFeedbackItem{
			{Kind: "issue_comment", Author: "bot", Body: "## Agent review\n\nLooks mostly fine."},
			{Kind: "review", Author: "rev", Body: "(COMMENTED) overall notes"},
			{Kind: "review_comment", Author: "ada", Body: `change title to "Ripple Todo"`, Path: "index.html", Line: 12},
			{Kind: "issue_comment", Author: "ada", Body: "Also wire up the form submit handler."},
		},
	}
	prioritizeHumanFeedback(&fb)
	if fb.Items[0].Kind != "review_comment" || !strings.Contains(fb.Items[0].Body, "Ripple Todo") {
		t.Fatalf("expected inline human comment first, got %#v", fb.Items[0])
	}
	if fb.Items[1].Kind != "issue_comment" || !strings.Contains(fb.Items[1].Body, "form submit") {
		t.Fatalf("expected human issue comment second, got %#v", fb.Items[1])
	}
	if !strings.HasPrefix(strings.TrimSpace(fb.Items[len(fb.Items)-1].Body), "## Agent review") {
		t.Fatalf("expected agent review last, got %#v", fb.Items[len(fb.Items)-1])
	}
}

func TestAppendPRReviewBodiesSkipsEmpty(t *testing.T) {
	raw := `{"reviews":[{"author":{"login":"ada"},"body":"","state":"COMMENTED"},{"author":{"login":"ada"},"body":"Please add tests","state":"CHANGES_REQUESTED"}]}`
	var fb PRFeedback
	if err := appendPRReviewBodies(&fb, raw); err != nil {
		t.Fatal(err)
	}
	if len(fb.Items) != 1 || !strings.Contains(fb.Items[0].Body, "Please add tests") {
		t.Fatalf("items = %#v", fb.Items)
	}
}

func TestQualityGateChecksIncludeBuildAndTypecheck(t *testing.T) {
	dir := t.TempDir()
	packageJSON := `{"scripts":{"test":"vitest run","lint":"eslint","typecheck":"tsc --noEmit","build":"vite build","format":"prettier --write ."}}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(packageJSON), 0600); err != nil {
		t.Fatal(err)
	}
	want := []string{"npm run test", "npm run lint", "npm run typecheck", "npm run build"}
	if got := qualityGateChecks(dir); !reflect.DeepEqual(got, want) {
		t.Fatalf("qualityGateChecks() = %#v, want %#v", got, want)
	}
}

func TestBuildCodexResolveConflictsPrompt(t *testing.T) {
	prompt := buildCodexResolveConflictsPrompt(
		"http://localhost:8080", "docs",
		Project{Name: "Atlas", WorkingDirectory: "/tmp"},
		Story{ID: "A-001", Title: "T", Description: "D"},
		"ripple/A-001-t", "main", 9, "https://example.com/pull/9",
	)
	for _, marker := range []string{
		"resolve merge conflicts",
		"ripple/A-001-t",
		"main",
		"<<<<<<<",
		"#9",
		"https://example.com/pull/9",
		"Do not push",
		"A-001",
	} {
		if !strings.Contains(prompt, marker) {
			t.Fatalf("prompt missing %q", marker)
		}
	}
}

func TestGitPreflightUpdatesFromOrigin(t *testing.T) {
	// origin (bare) <- main repo; main is updated on origin after local clone diverges.
	bare := t.TempDir()
	mustRunGit(t, bare, "init", "--bare")
	seed := t.TempDir()
	mustRunGit(t, seed, "init")
	mustRunGit(t, seed, "config", "user.email", "t@example.com")
	mustRunGit(t, seed, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(seed, "a.txt"), []byte("one\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, seed, "add", ".")
	mustRunGit(t, seed, "commit", "-m", "init")
	mustRunGit(t, seed, "branch", "-M", "main")
	mustRunGit(t, seed, "remote", "add", "origin", bare)
	mustRunGit(t, seed, "push", "-u", "origin", "main")

	work := t.TempDir()
	mustRunGit(t, work, "clone", bare, work)
	mustRunGit(t, work, "config", "user.email", "t@example.com")
	mustRunGit(t, work, "config", "user.name", "t")

	// Advance origin/main after the clone.
	if err := os.WriteFile(filepath.Join(seed, "a.txt"), []byte("two\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, seed, "add", ".")
	mustRunGit(t, seed, "commit", "-m", "second")
	mustRunGit(t, seed, "push", "origin", "main")

	branch, err := gitPreflight(t.Context(), work, "")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "main" {
		t.Fatalf("branch = %q", branch)
	}
	body, err := os.ReadFile(filepath.Join(work, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "two\n" {
		t.Fatalf("local main not updated from origin; a.txt = %q", body)
	}
}

func TestGitMergeBaseIntoFeatureDetectsConflicts(t *testing.T) {
	dir := t.TempDir()
	mustRunGit(t, dir, "init")
	mustRunGit(t, dir, "config", "user.email", "t@example.com")
	mustRunGit(t, dir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, dir, "add", ".")
	mustRunGit(t, dir, "commit", "-m", "base")
	mustRunGit(t, dir, "branch", "-M", "main")
	mustRunGit(t, dir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("feature\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, dir, "add", ".")
	mustRunGit(t, dir, "commit", "-m", "feature change")
	mustRunGit(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("mainline\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, dir, "add", ".")
	mustRunGit(t, dir, "commit", "-m", "main change")

	// Simulate origin/* refs for merge helper (no real remote).
	mustRunGit(t, dir, "update-ref", "refs/remotes/origin/main", "main")
	mustRunGit(t, dir, "update-ref", "refs/remotes/origin/feature", "feature")

	hadConflicts, err := gitMergeBaseIntoFeature(t.Context(), dir, "feature", "main")
	if err != nil {
		t.Fatal(err)
	}
	if !hadConflicts {
		t.Fatal("expected merge conflicts")
	}
	ok, err := gitHasUnresolvedConflicts(t.Context(), dir)
	if err != nil || !ok {
		t.Fatalf("expected unresolved conflicts, ok=%v err=%v", ok, err)
	}
}

func TestGitCompleteMergeAfterAgentRewroteFileWithoutStaging(t *testing.T) {
	dir := t.TempDir()
	mustRunGit(t, dir, "init")
	mustRunGit(t, dir, "config", "user.email", "t@example.com")
	mustRunGit(t, dir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, dir, "add", ".")
	mustRunGit(t, dir, "commit", "-m", "base")
	mustRunGit(t, dir, "branch", "-M", "main")
	mustRunGit(t, dir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("feature\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, dir, "add", ".")
	mustRunGit(t, dir, "commit", "-m", "feature")
	mustRunGit(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("mainline\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, dir, "add", ".")
	mustRunGit(t, dir, "commit", "-m", "main")
	mustRunGit(t, dir, "update-ref", "refs/remotes/origin/main", "main")

	hadConflicts, err := gitMergeBaseIntoFeature(t.Context(), dir, "feature", "main")
	if err != nil || !hadConflicts {
		t.Fatalf("setup conflicts: had=%v err=%v", hadConflicts, err)
	}

	// Simulate agent: rewrite file with a clean resolution, but do not git-add.
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("resolved\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Index still unmerged...
	unmerged, err := gitHasUnresolvedConflicts(t.Context(), dir)
	if err != nil || !unmerged {
		t.Fatalf("expected unmerged index before staging, unmerged=%v err=%v", unmerged, err)
	}
	// ...but working tree has no markers.
	markers, err := gitWorktreeHasConflictMarkers(t.Context(), dir)
	if err != nil || markers {
		t.Fatalf("markers=%v err=%v", markers, err)
	}

	// Old bug: treating unmerged index as failure. New path: complete merge after staging.
	if err := gitCompleteMergeCommit(t.Context(), dir, "resolve", defaultGitHubIdentity()); err != nil {
		t.Fatal(err)
	}
	if gitMergeInProgress(t.Context(), dir) {
		t.Fatal("merge should be finished")
	}
	body, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(body) != "resolved\n" {
		t.Fatalf("body = %q", body)
	}
}

func TestGitMergeBaseResumesInProgressMerge(t *testing.T) {
	dir := t.TempDir()
	mustRunGit(t, dir, "init")
	mustRunGit(t, dir, "config", "user.email", "t@example.com")
	mustRunGit(t, dir, "config", "user.name", "t")
	_ = os.WriteFile(filepath.Join(dir, "f.txt"), []byte("base\n"), 0600)
	mustRunGit(t, dir, "add", ".")
	mustRunGit(t, dir, "commit", "-m", "base")
	mustRunGit(t, dir, "branch", "-M", "main")
	mustRunGit(t, dir, "checkout", "-b", "feature")
	_ = os.WriteFile(filepath.Join(dir, "f.txt"), []byte("feature\n"), 0600)
	mustRunGit(t, dir, "add", ".")
	mustRunGit(t, dir, "commit", "-m", "feature")
	mustRunGit(t, dir, "checkout", "main")
	_ = os.WriteFile(filepath.Join(dir, "f.txt"), []byte("mainline\n"), 0600)
	mustRunGit(t, dir, "add", ".")
	mustRunGit(t, dir, "commit", "-m", "main")
	mustRunGit(t, dir, "update-ref", "refs/remotes/origin/main", "main")

	had, err := gitMergeBaseIntoFeature(t.Context(), dir, "feature", "main")
	if err != nil || !had {
		t.Fatalf("first merge: had=%v err=%v", had, err)
	}
	// Second call should resume rather than failing checkout.
	had2, err := gitMergeBaseIntoFeature(t.Context(), dir, "feature", "main")
	if err != nil {
		t.Fatal(err)
	}
	if !had2 {
		t.Fatal("expected still-conflicted merge on resume")
	}
}

func mustRunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, stderr, err := runCommand(t.Context(), dir, "git", args...); err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, stderr)
	}
}
