/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package manifestanalyzer

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

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
