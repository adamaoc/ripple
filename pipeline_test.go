package main

import "testing"

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

func TestRunKindSource(t *testing.T) {
	tests := map[string]string{
		RunKindCodexImplement: "codex",
		RunKindCodexFix:       "codex",
		RunKindGrokReview:     "grok",
		"":                    "codex",
	}
	for kind, want := range tests {
		if got := runKindSource(kind); got != want {
			t.Errorf("runKindSource(%q) = %q, want %q", kind, got, want)
		}
	}
}
