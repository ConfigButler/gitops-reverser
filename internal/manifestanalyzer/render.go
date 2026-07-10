// SPDX-License-Identifier: Apache-2.0

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

// RenderScanText writes a human-readable view of a scan: the acceptance decision and
// its refusals, the retained allowlisted documents, and the full plan. It is the
// M5 dry-run output for the CLI and doubles as a GitTarget status summary.
func RenderScanText(w io.Writer, result ScanResult) {
	renderAcceptanceText(w, result.Acceptance)
	fmt.Fprintln(w)
	renderPlanText(w, result.Plan)
}

// renderAcceptanceText writes the acceptance decision, refusals, and retained files.
func renderAcceptanceText(w io.Writer, acc Acceptance) {
	if acc.Accepted {
		fmt.Fprintln(w, "Acceptance: accepted")
	} else {
		fmt.Fprintf(w, "Acceptance: REFUSED (%d issue(s))\n", len(acc.Issues))
	}
	for _, is := range acc.Issues {
		fmt.Fprintf(w, "  %-22s %s#%d  %s\n", is.Kind, is.Path, is.DocumentIndex, is.Message)
	}
	if len(acc.Retained) > 0 {
		fmt.Fprintf(w, "Retained (allowlisted, not materialized): %d\n", len(acc.Retained))
		for _, rd := range acc.Retained {
			fmt.Fprintf(w, "  %s#%d  %s\n", rd.Location.Path, rd.Location.DocumentIndex, identityRef(rd.Identity))
		}
	}
}

// renderPlanText writes the plan's actions and any planning diagnostics.
func renderPlanText(w io.Writer, plan Plan) {
	if len(plan.Actions) == 0 {
		fmt.Fprintln(w, "Plan: no changes")
	} else {
		fmt.Fprintf(w, "Plan: %d action(s)\n", len(plan.Actions))
		for _, a := range plan.Actions {
			fmt.Fprintf(w, "  %-12s %-40s %s\n", a.Kind, planActionTarget(a), a.Reason)
		}
	}
	for _, d := range plan.Diagnostics {
		fmt.Fprintf(w, "  diag %-7s %s#%d  %s\n", d.Level, d.Path, d.DocumentIndex, d.Message)
	}
}

// planActionTarget renders the file an action touches: the placement path for a
// create (which has no existing location yet), the existing document otherwise.
func planActionTarget(a PlanAction) string {
	if a.Kind == PlanCreate {
		return a.Resource.ToGitPath()
	}
	return fmt.Sprintf("%s#%d", a.Ref.FilePath, a.Ref.DocumentIndex)
}
