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
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
)

// RenderJSON writes the report as indented JSON. It is the machine-readable form
// shared by the controller status path and the CLI.
func RenderJSON(w io.Writer, rep Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

// RenderText writes a human-readable summary of the report.
func RenderText(w io.Writer, rep Report) {
	s := rep.Summary
	fmt.Fprintf(w, "Manifest analysis: %s\n", rootOrFS(rep.Root))
	fmt.Fprintf(w, "  files: %d (yaml %d, other %d)   documents: %d\n",
		s.FilesTotal, s.YAMLFiles, s.NonYAMLFiles, s.Documents)
	if len(s.ByClass) > 0 {
		fmt.Fprintf(w, "  classes: %s\n", joinCounts(classCounts(s.ByClass)))
	}
	if len(s.ByGVK) > 0 {
		fmt.Fprintf(w, "  gvks: %s\n", joinCounts(strCounts(s.ByGVK)))
	}
	fmt.Fprintf(w, "  duplicates: %d   encrypted: %d\n", s.Duplicates, s.Encrypted)
	if len(s.Diagnostics) > 0 {
		fmt.Fprintf(w, "  diagnostics: %s\n", joinCounts(diagCounts(s.Diagnostics)))
	}

	fmt.Fprintln(w, "\nFiles:")
	for _, f := range rep.Files {
		renderFile(w, f)
	}

	fmt.Fprintln(w)
	if len(rep.Issues) == 0 {
		fmt.Fprintln(w, "Acceptance: no issues")
		return
	}
	fmt.Fprintf(w, "Acceptance: %d issue(s)\n", len(rep.Issues))
	for _, is := range rep.Issues {
		fmt.Fprintf(w, "  %-18s %s#%d  %s\n", is.Kind, is.Path, is.DocumentIndex, is.Message)
	}
}

// renderFile writes one file's line(s) for the text report.
func renderFile(w io.Writer, f FileReport) {
	if !f.IsYAML {
		fmt.Fprintf(w, "  %-44s (non-yaml, ignored)\n", f.Path)
		return
	}
	fmt.Fprintf(w, "  %s\n", f.Path)
	for _, d := range f.Documents {
		line := fmt.Sprintf("    [%d] %-14s", d.Index, d.Class)
		if !d.GVK.Empty() {
			line += " " + identityRef(d.Identity)
		}
		if tags := docTags(d); tags != "" {
			line += "  (" + tags + ")"
		}
		fmt.Fprintln(w, line)
	}
}

// docTags renders the small set of flags shown after a document line. Duplicate
// identity is no longer a per-document tag — it surfaces in the Acceptance section
// as an issue.
func docTags(d DocumentReport) string {
	if d.Cause == nil {
		return ""
	}
	switch d.Cause.Kind {
	case CauseEncrypted:
		return "encrypted"
	case CauseNonEditable:
		if d.Cause.Detail != "" {
			return "non-editable: " + d.Cause.Detail
		}
		return "non-editable"
	case CauseNone:
		return ""
	default:
		return ""
	}
}

// rootOrFS describes the scanned root, falling back to "(fs)" for an in-memory FS.
func rootOrFS(root string) string {
	if root == "" {
		return "(fs)"
	}
	return root
}

// kv is a sortable label/count pair for deterministic count rendering.
type kv struct {
	label string
	count int
}

// joinCounts formats sorted label/count pairs as "a 2, b 1".
func joinCounts(items []kv) string {
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = fmt.Sprintf("%s %d", it.label, it.count)
	}
	return strings.Join(parts, ", ")
}

func classCounts(m map[Class]int) []kv {
	out := make([]kv, 0, len(m))
	for k, v := range m {
		out = append(out, kv{string(k), v})
	}
	return sortKV(out)
}

func strCounts(m map[string]int) []kv {
	out := make([]kv, 0, len(m))
	for k, v := range m {
		out = append(out, kv{k, v})
	}
	return sortKV(out)
}

func diagCounts(m map[manifestedit.DiagnosticLevel]int) []kv {
	out := make([]kv, 0, len(m))
	for k, v := range m {
		out = append(out, kv{string(k), v})
	}
	return sortKV(out)
}

func sortKV(items []kv) []kv {
	sort.Slice(items, func(i, j int) bool { return items[i].label < items[j].label })
	return items
}
