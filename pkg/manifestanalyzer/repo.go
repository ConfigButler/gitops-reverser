// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"context"
	"encoding/json"
	"io"

	internalanalyzer "github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
)

// Layout is the structural shape of a candidate folder.
type Layout string

const (
	// LayoutPlain is a directory of raw KRM documents with explicit namespaces and no
	// kustomization. Accepted.
	LayoutPlain Layout = "plain"
	// LayoutKustomizeSingle is a self-contained render root: one kustomization whose
	// resources graph stays within its own subtree. Accepted.
	LayoutKustomizeSingle Layout = "kustomize-single"
	// LayoutKustomizeOverlay is a render root reaching a base outside its own subtree
	// (the classic base/ + overlays/{env} shape). Refused today, with a forward-looking
	// reason: it becomes accepted when render-root scoping ships.
	LayoutKustomizeOverlay Layout = "kustomize-overlay"
	// LayoutRefusedStructural is a render root whose kustomization uses a construct the
	// writer cannot map back to editable source. This is the permanent support boundary,
	// never a "not yet".
	LayoutRefusedStructural Layout = "refused-structural"
)

// Refusal reason codes. The distinction is load-bearing and a consumer must not collapse
// it: one is a "not yet", the other is permanent.
const (
	// ReasonOverlayFanOutNeedsF2 is the forward-looking refusal that flips to accepted
	// when render-root scoping ships.
	ReasonOverlayFanOutNeedsF2 = "overlay-fan-out-needs-f2"
	// ReasonRefusedStructural is the permanent support boundary.
	ReasonRefusedStructural = "refused-structural"
)

// RefusalReason is one machine-readable reason a candidate is not accepted.
type RefusalReason struct {
	Code string `json:"code"`
	// Detail is human-readable and not a stable string.
	Detail string `json:"detail"`
}

// ResourceCounts splits the KRM a candidate covers into what it renders versus what it
// could actually edit. The two are equal for a plain or self-contained candidate and
// diverge for an overlay, which renders documents it cannot own.
type ResourceCounts struct {
	Rendered int `json:"rendered"`
	Editable int `json:"editable"`
	NonKRM   int `json:"nonKrm"`
}

// Candidate is one folder that could become a GitTarget.
type Candidate struct {
	// Path is slash-separated and relative to the repository root.
	Path   string `json:"path"`
	Layout Layout `json:"layout"`
	// AcceptedByOperator reports whether the operator would adopt this folder today.
	AcceptedByOperator bool            `json:"acceptedByOperator"`
	RefusalReasons     []RefusalReason `json:"refusalReasons,omitempty"`
	// RenderRoot reports whether the candidate is a kustomize render root.
	RenderRoot bool `json:"renderRoot"`
	// ReadScope lists base directories outside this candidate's subtree that its
	// kustomization reads. Empty for plain and self-contained candidates.
	ReadScope []string `json:"readScope,omitempty"`
	// InferredNamespace is the namespace the candidate resolves to, when unambiguous.
	InferredNamespace string         `json:"inferredNamespace,omitempty"`
	Resources         ResourceCounts `json:"resources"`
	// OverlapsWith lists candidate paths this one nests with. Two overlapping candidates
	// can never both become GitTargets — a folder has exactly one owner.
	OverlapsWith []string `json:"overlapsWith,omitempty"`
}

// OverlapConflict records that Ancestor strictly contains Descendant.
type OverlapConflict struct {
	Ancestor   string `json:"ancestor"`
	Descendant string `json:"descendant"`
}

// RepoSummary is the repository-level roll-up.
type RepoSummary struct {
	CandidatesByLayout map[Layout]int    `json:"candidatesByLayout"`
	Accepted           int               `json:"accepted"`
	Refused            int               `json:"refused"`
	OverlapConflicts   []OverlapConflict `json:"overlapConflicts,omitempty"`
	// FleetRoot reports that the repository root is a cluster/fleet root. A GitTarget
	// points at an app subtree, never at such a root.
	FleetRoot bool `json:"fleetRoot,omitempty"`
	// UnsupportedConstructs is the sorted, de-duplicated set of unsupported kustomize
	// features seen across refused candidates, so a tool can say "this repository uses
	// Helm inflation, which the operator does not manage".
	UnsupportedConstructs []string `json:"unsupportedConstructs,omitempty"`
}

// RepoReport answers "which folders in this repository could become GitTargets?".
type RepoReport struct {
	SchemaVersion string `json:"schemaVersion"`
	// Root is the scanned repository root as passed to ScanRepo. Informational.
	Root       string      `json:"root,omitempty"`
	Candidates []Candidate `json:"candidates"`
	Summary    RepoSummary `json:"summary"`
}

// ScanRepo walks a whole repository and enumerates the folders that could become
// GitTargets, classifying each one's layout and reporting why a folder is refused. It is
// read-only, needs no cluster, and never follows symlinks.
func ScanRepo(ctx context.Context, root string) (RepoReport, error) {
	rep, err := internalanalyzer.ScanRepo(ctx, root)
	if err != nil {
		return RepoReport{}, err
	}
	return repoReportFrom(rep), nil
}

// WriteJSON writes the report as indented JSON — byte-for-byte what
// `manifest-analyzer --mode scan-repo --format json` prints.
func (r RepoReport) WriteJSON(w io.Writer) error {
	if r.Candidates == nil {
		r.Candidates = []Candidate{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// repoReportFrom projects the internal discovery report onto the public contract.
func repoReportFrom(rep internalanalyzer.RepoReport) RepoReport {
	out := RepoReport{
		SchemaVersion: SchemaVersion,
		Root:          rep.Root,
		Candidates:    make([]Candidate, 0, len(rep.Candidates)),
		Summary: RepoSummary{
			CandidatesByLayout:    make(map[Layout]int, len(rep.Summary.CandidatesByLayout)),
			Accepted:              rep.Summary.Accepted,
			Refused:               rep.Summary.Refused,
			FleetRoot:             rep.Summary.FleetRoot,
			UnsupportedConstructs: rep.Summary.UnsupportedConstructs,
		},
	}
	for layout, n := range rep.Summary.CandidatesByLayout {
		out.Summary.CandidatesByLayout[Layout(layout)] = n
	}
	for _, conflict := range rep.Summary.OverlapConflicts {
		out.Summary.OverlapConflicts = append(out.Summary.OverlapConflicts, OverlapConflict{
			Ancestor: conflict.Ancestor, Descendant: conflict.Descendant,
		})
	}
	for _, cand := range rep.Candidates {
		out.Candidates = append(out.Candidates, candidateFrom(cand))
	}
	return out
}

func candidateFrom(cand internalanalyzer.RepoCandidate) Candidate {
	out := Candidate{
		Path:               cand.Path,
		Layout:             Layout(cand.Layout),
		AcceptedByOperator: cand.AcceptedByOperator,
		RenderRoot:         cand.RenderRoot,
		ReadScope:          cand.ReadScope,
		InferredNamespace:  cand.InferredNamespace,
		Resources: ResourceCounts{
			Rendered: cand.Resources.Rendered,
			Editable: cand.Resources.Editable,
			NonKRM:   cand.Resources.NonKRM,
		},
		OverlapsWith: cand.OverlapsWith,
	}
	for _, reason := range cand.RefusalReasons {
		out.RefusalReasons = append(out.RefusalReasons, RefusalReason{Code: reason.Code, Detail: reason.Detail})
	}
	return out
}
