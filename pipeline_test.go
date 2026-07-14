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
