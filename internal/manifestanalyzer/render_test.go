// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

func TestRenderText(t *testing.T) {
	rep := Analyze(sampleFS())
	rep.Root = "/tmp/repo"

	var buf bytes.Buffer
	RenderText(&buf, rep)
	out := buf.String()

	for _, want := range []string{
		"Manifest analysis: /tmp/repo",
		"files: 8 (yaml 7, other 1)",
		"gvks: apps/v1/Deployment 2",
		"docs/notes.txt",
		"(non-yaml, ignored)",
		"krm",
		"apps/v1/Deployment/default/web",
		"(encrypted)",
		"Acceptance:",
		// Duplicate identity is reported as an acceptance issue, not an inline tag.
		"duplicate-identity",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderText_NonEditableAndNoIssues(t *testing.T) {
	rep := Report{
		Root: "",
		Files: []FileReport{{
			Path:   "cm.yaml",
			IsYAML: true,
			Documents: []DocumentReport{{
				Index:    0,
				Class:    ClassKRM,
				GVK:      GVK{Version: "v1", Kind: "ConfigMap"},
				Identity: manifestedit.Identity{APIVersion: "v1", Kind: "ConfigMap", Namespace: "default", Name: "x"},
				Editable: false,
				Cause:    &DocumentCause{Kind: CauseNonEditable, Detail: "anchor"},
			}},
		}},
		Summary: buildSummary([]FileReport{}, nil, 0),
	}
	var buf bytes.Buffer
	RenderText(&buf, rep)
	out := buf.String()

	if !strings.Contains(out, "(fs)") {
		t.Errorf("empty root should render as (fs): %s", out)
	}
	if !strings.Contains(out, "non-editable: anchor") {
		t.Errorf("expected non-editable tag: %s", out)
	}
	if !strings.Contains(out, "Acceptance: no issues") {
		t.Errorf("expected no-issues line: %s", out)
	}
}

func TestRenderJSON_RoundTrip(t *testing.T) {
	rep := Analyze(sampleFS())

	var buf bytes.Buffer
	if err := RenderJSON(&buf, rep); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}

	var decoded Report
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Summary.Documents != rep.Summary.Documents {
		t.Errorf("round-trip documents = %d, want %d", decoded.Summary.Documents, rep.Summary.Documents)
	}
	if len(decoded.Files) != len(rep.Files) {
		t.Errorf("round-trip files = %d, want %d", len(decoded.Files), len(rep.Files))
	}
	if decoded.Summary.ByClass[ClassKRM] != rep.Summary.ByClass[ClassKRM] {
		t.Errorf("round-trip class counts differ")
	}
}

// scanWithRefusalAndPlan builds a scan with one create, one patch, a retained file,
// and a denied-Secret refusal, so the renderers exercise every branch.
func scanWithRefusalAndPlan(t *testing.T) ScanResult {
	t.Helper()
	fsys := fstest.MapFS{
		"deploy.yaml":        {Data: []byte(deployYAML)},
		"secret.yaml":        {Data: []byte(plainSecretYAML)},
		"kustomization.yaml": {Data: []byte(kustomizationY)},
	}
	desired := []DesiredResource{desiredDeployWeb(3), desiredConfigMap("new")}
	policy := ScanPolicy{Acceptance: AcceptancePolicy{Allowlist: DefaultAllowlist()}}
	return Scan(context.Background(), fsys, snapMapper(), desired, policy)
}

func TestRenderScanText(t *testing.T) {
	var buf bytes.Buffer
	RenderScanText(&buf, scanWithRefusalAndPlan(t))
	out := buf.String()

	for _, want := range []string{
		"Acceptance: REFUSED",
		"unresolved-krm",
		"Retained (allowlisted, not materialized): 1",
		"kustomization.yaml",
		"Plan: 2 action(s)",
		"create",
		"default/configmaps/new.yaml",
		"patch",
		"deploy.yaml#0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scan text output missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderScanText_Accepted(t *testing.T) {
	fsys := fstest.MapFS{"deploy.yaml": {Data: []byte(deployYAML)}}
	desired := []DesiredResource{desiredDeployWeb(1)}
	result := Scan(context.Background(), fsys, snapMapper(), desired, ScanPolicy{})

	var buf bytes.Buffer
	RenderScanText(&buf, result)
	out := buf.String()
	if !strings.Contains(out, "Acceptance: accepted") {
		t.Errorf("expected accepted line: %s", out)
	}
	if !strings.Contains(out, "Plan: no changes") {
		t.Errorf("expected no-changes plan line: %s", out)
	}
}

func TestRenderScanJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderScanJSON(&buf, scanWithRefusalAndPlan(t)); err != nil {
		t.Fatalf("RenderScanJSON: %v", err)
	}

	var parsed struct {
		Accepted bool `json:"accepted"`
		Issues   []struct {
			Kind string `json:"kind"`
		} `json:"issues"`
		Retained []struct {
			Path string `json:"path"`
		} `json:"retained"`
		Plan struct {
			Counts  map[string]int `json:"counts"`
			Actions []struct {
				Kind     string `json:"kind"`
				Path     string `json:"path"`
				Resource string `json:"resource"`
			} `json:"actions"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("scan JSON is invalid: %v\n%s", err, buf.String())
	}
	if parsed.Accepted {
		t.Errorf("scan JSON should report refusal")
	}
	if len(parsed.Issues) == 0 || len(parsed.Retained) != 1 {
		t.Errorf("scan JSON issues=%d retained=%d", len(parsed.Issues), len(parsed.Retained))
	}
	if parsed.Plan.Counts["create"] != 1 || parsed.Plan.Counts["patch"] != 1 {
		t.Errorf("scan JSON plan counts = %+v", parsed.Plan.Counts)
	}
	if len(parsed.Plan.Actions) != 2 {
		t.Errorf("scan JSON should carry both actions, got %+v", parsed.Plan.Actions)
	}
}

// TestRenderScanJSON_PreservesDocumentIndexZero guards the machine-readable contract:
// a patch on a file's first document must serialize documentIndex 0 (not omit it),
// while a create — which has no existing document location — omits it.
func TestRenderScanJSON_PreservesDocumentIndexZero(t *testing.T) {
	fsys := fstest.MapFS{"deploy.yaml": {Data: []byte(deployYAML)}}
	desired := []DesiredResource{desiredDeployWeb(3), desiredConfigMap("new")} // patch deploy#0 + create
	result := Scan(context.Background(), fsys, snapMapper(), desired, ScanPolicy{})

	var buf bytes.Buffer
	if err := RenderScanJSON(&buf, result); err != nil {
		t.Fatalf("RenderScanJSON: %v", err)
	}
	if !strings.Contains(buf.String(), `"documentIndex": 0`) {
		t.Errorf("a patch on the first document must keep documentIndex 0:\n%s", buf.String())
	}

	var parsed struct {
		Plan struct {
			Actions []struct {
				Kind          string `json:"kind"`
				DocumentIndex *int   `json:"documentIndex"`
			} `json:"actions"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, a := range parsed.Plan.Actions {
		switch a.Kind {
		case "patch":
			if a.DocumentIndex == nil || *a.DocumentIndex != 0 {
				t.Errorf("patch action should carry documentIndex 0, got %+v", a.DocumentIndex)
			}
		case "create":
			if a.DocumentIndex != nil {
				t.Errorf("create action should omit documentIndex, got %v", *a.DocumentIndex)
			}
		default:
			t.Errorf("unexpected action kind %q", a.Kind)
		}
	}
}
