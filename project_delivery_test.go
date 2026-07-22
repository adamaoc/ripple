package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestProjectDeliverySettingsDefaultsAndUpdate(t *testing.T) {
	app := testApp(t)
	project, err := app.createProject(context.Background(), "atlas", "Atlas", "A", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if project.QualityGateMode != QualityGateStrict {
		t.Fatalf("qualityGateMode = %q", project.QualityGateMode)
	}
	if !project.DeleteBranchOnMerge {
		t.Fatal("deleteBranchOnMerge should default true")
	}
	if project.BranchNameTemplate != DefaultBranchNameTemplate {
		t.Fatalf("template = %q", project.BranchNameTemplate)
	}

	if err := app.updateProjectDeliverySettings(context.Background(), "atlas", projectDeliverySettings{
		DefaultBranchOverride: "develop",
		PRBaseBranch:          "develop",
		QualityGateMode:       QualityGateWarn,
		DeleteBranchOnMerge:   false,
		BranchNameTemplate:    "work/{id}-{slug}",
	}); err != nil {
		t.Fatal(err)
	}
	project, err = app.getProject(context.Background(), "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if project.DefaultBranchOverride != "develop" || project.PRBaseBranch != "develop" {
		t.Fatalf("branches = %+v", project)
	}
	if project.QualityGateMode != QualityGateWarn {
		t.Fatalf("mode = %q", project.QualityGateMode)
	}
	if project.DeleteBranchOnMerge {
		t.Fatal("expected deleteBranchOnMerge false")
	}
	if project.BranchNameTemplate != "work/{id}-{slug}" {
		t.Fatalf("template = %q", project.BranchNameTemplate)
	}
}

func TestProjectDeliverySettingsFormPost(t *testing.T) {
	app := testApp(t)
	if _, err := app.createProject(context.Background(), "atlas", "Atlas", "A", "", ""); err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"deliverySettings":      {"1"},
		"workingDirectory":      {""},
		"autonomyMode":          {"supervised"},
		"defaultBranchOverride": {"trunk"},
		"prBaseBranch":          {"trunk"},
		"qualityGateMode":       {"warn"},
		"branchNameTemplate":    {"feat/{id}-{slug}"},
		// omit deleteBranchOnMerge → false
		"redirect": {"/projects/atlas/backlog"},
	}
	req := httptest.NewRequest(http.MethodPost, "/projects/atlas/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, req)
	if res.Code != http.StatusSeeOther && res.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}
	project, err := app.getProject(context.Background(), "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if project.AutonomyMode != AutonomySupervised {
		t.Fatalf("autonomy = %q", project.AutonomyMode)
	}
	if project.DefaultBranchOverride != "trunk" || project.QualityGateMode != QualityGateWarn {
		t.Fatalf("project = %+v", project)
	}
	if project.DeleteBranchOnMerge {
		t.Fatal("unchecked delete branch should be false")
	}
}

func TestBacklogShowsDeliverySettings(t *testing.T) {
	app := testApp(t)
	seedProjectStories(t, app, "atlas", 1)
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/projects/atlas/backlog", nil))
	body := res.Body.String()
	for _, marker := range []string{
		"Workspace", "Delivery", "Advanced",
		"defaultBranchOverride", "qualityGateMode", "branchNameTemplate", "deleteBranchOnMerge",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("backlog missing %q", marker)
		}
	}
}

func TestAPICreateProjectWithDeliveryFields(t *testing.T) {
	app := testApp(t)
	body := `{"name":"Widgets","prefix":"W","autonomyMode":"autonomous","defaultBranchOverride":"main","qualityGateMode":"warn","deleteBranchOnMerge":false,"branchNameTemplate":"w/{id}-{slug}"}`
	req := httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	app.routes().ServeHTTP(res, req)
	if res.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), `"qualityGateMode":"warn"`) {
		t.Fatalf("response = %s", res.Body.String())
	}
	if !strings.Contains(res.Body.String(), `"deleteBranchOnMerge":false`) {
		t.Fatalf("response = %s", res.Body.String())
	}
}
