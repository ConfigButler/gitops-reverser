// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"context"
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
	// (the classic base/ + overlays/{env} shape). Accepted: render-root scoping shipped,
	// so the base is read as read-only context and writes stay in the overlay. The
	// candidate's editable count shows how much of what it renders it can own. An overlay
	// refused for a real fault carries that fault's own code, never a forward-looking one.
	LayoutKustomizeOverlay Layout = "kustomize-overlay"
	// LayoutRefusedStructural is a render root whose kustomization uses a construct the
	// writer cannot map back to editable source. This is the support boundary; each
	// refusal it carries says whether anyone can solve it.
	LayoutRefusedStructural Layout = "refused-structural"
)

// Refusal reason codes that are NOT [IssueKind] values. This block is not the
// enumeration of what [RefusalReason.Code] can hold — it is the two codes that come from
// somewhere other than the acceptance gate. Every other code a candidate carries is an
// [IssueKind], because that is where it came from: the gate raised an issue, and
// RefusalReason is that issue projected one level up.
const (
	// ReasonOverlayFanOutUnsupported was the forward-looking refusal for an external-base
	// overlay. Render-root scoping shipped, so the scanner now ADOPTS such an overlay and no
	// longer emits this code; it is retained only so a consumer pinning the string still
	// compiles. A kustomize-overlay candidate is accepted (its editable count shows how much
	// it owns); an overlay refused for a real fault carries that fault's own code.
	//
	// Deprecated: no longer emitted; kept for source compatibility.
	ReasonOverlayFanOutUnsupported IssueKind = "overlay-fan-out-unsupported"
	// ReasonRefusedStructural is the permanent support boundary: a render root whose
	// kustomization uses a construct the writer cannot map back to editable source.
	ReasonRefusedStructural IssueKind = "refused-structural"
)

// RefusalReason is one machine-readable reason a candidate is not accepted.
type RefusalReason struct {
	// Code is an [IssueKind] value, or [ReasonRefusedStructural]. The type makes that
	// relationship compile-checked rather than merely stated: a candidate's refusal is
	// the acceptance gate's own issue, projected up.
	Code IssueKind `json:"code"`
	// Detail is human-readable and not a stable string.
	Detail string `json:"detail"`
	// Solvable says whether anyone can make this candidate acceptable with this release.
	// A report produced before this field shipped carries no `solvable` key at all; read
	// that as "nobody said", not as false.
	Solvable bool `json:"solvable"`
	// Actor names who can solve it. Empty unless Solvable.
	Actor Actor `json:"actor,omitempty"`
}

// ResourceCounts splits the KRM a candidate covers into what it renders versus what it
// could actually edit. The two are equal for a plain or self-contained candidate and
// diverge for an overlay, which renders documents it cannot own.
type ResourceCounts struct {
	Rendered int `json:"rendered"`
	Editable int `json:"editable"`
	NonKRM   int `json:"nonKrm"`
}

// RenderedTypes says which type a folder renders into which namespace. It is expressed as
// a map rather than two lists because the PAIRING is the answer: a set of types beside a
// set of namespaces reads as every combination of the two, and a folder rendering a
// Deployment into frontend and a Service into backend would then describe two pairs that
// exist in no repository — enough to authorize a watch that matches nothing.
//
// Every type is a canonical GVK string: "group/version/kind", or "version/kind" for the
// core group. Split on "/" and count the segments; a group never contains one.
//
//	apps/v1/Deployment
//	v1/ConfigMap
//	rbac.authorization.k8s.io/v1/ClusterRole
type RenderedTypes struct {
	// ByNamespace lists the types that land in each namespace, sorted, keyed by namespace.
	// These are the exact (type, namespace) pairs, and the only ones.
	ByNamespace map[string][]string `json:"byNamespace,omitempty"`

	// NamespaceUndeclared lists the types that render WITHOUT a namespace, sorted.
	//
	// It is NOT a list of cluster-scoped types, and must not be read as one. It holds two
	// facts this scan cannot tell apart: a genuinely cluster-scoped type, and a namespaced
	// type relying on whatever namespace the applier defaults to. Separating them needs API
	// discovery, and a structure-only scan has none — so the honest reading is "we do not
	// know where these land". A scan that does have discovery can split them, in a field
	// added then rather than a name reserved now.
	//
	// A type can appear here AND under ByNamespace. Two ConfigMaps, one carrying a
	// namespace and one not, is an ordinary folder rather than a contradiction.
	NamespaceUndeclared []string `json:"namespaceUndeclared,omitempty"`
}

// Candidate is one folder that could become a GitTarget.// Candidate is one folder that could become a GitTarget.
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
	// RenderedTypes says which type this folder renders into which namespace. For a render
	// root it is read off a real kustomize build, so it covers what the folder pulls from a
	// base outside its own subtree and has the namespace transformer already applied; for a
	// plain folder the documents are the render.
	//
	// It answers the two questions that gate provisioning from a folder scan, and both gate
	// a step that is not cheap to undo. The schemas have to be served wherever this is
	// applied — a type the destination does not serve does not degrade, the apply fails
	// with "no matches for kind" and waits for the next resync. And naming a GitTarget's
	// allowed source namespaces, or one watch rule per (type, namespace), needs the exact
	// pairs: [Candidate.InferredNamespace] is one name, and a folder can render into
	// several.
	//
	// Everything is empty for a render root kustomize could not build. What it renders is
	// not knowable, and saying nothing is the honest answer.
	RenderedTypes RenderedTypes `json:"renderedTypes"`
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

// RepoReport answers "which folders in this repository could become GitTargets?". It is a
// KRM document: the scan REQUEST is the spec, what was FOUND is the status. See
// [TypeMeta] for why the envelope, and why the document is never served or applyable.
type RepoReport struct {
	TypeMeta `json:",inline"`

	Spec   RepoReportSpec   `json:"spec"`
	Status RepoReportStatus `json:"status"`
}

// RepoReportSpec is the scan that was asked for.
type RepoReportSpec struct {
	// Root is the scanned repository root as passed to ScanRepo.
	Root string `json:"root,omitempty"`
	// Mode is always [ModeScanRepo].
	Mode string `json:"mode"`
}

// RepoReportStatus is what the scan found.
type RepoReportStatus struct {
	// Generator names the build that produced this report. Never empty.
	Generator  Generator   `json:"generator"`
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
	return writeJSON(w, r.withNonNilCandidates())
}

// WriteYAML writes the report as YAML — byte-for-byte what
// `manifest-analyzer --mode scan-repo --format yaml` prints. The report is a KRM document,
// so this is the serialization it reads best in, and the one a human can commit beside the
// manifests it describes.
func (r RepoReport) WriteYAML(w io.Writer) error {
	return writeYAML(w, r.withNonNilCandidates())
}

// withNonNilCandidates returns a copy whose Candidates marshals as [] rather than null, so
// a consumer can iterate it unconditionally.
func (r RepoReport) withNonNilCandidates() RepoReport {
	if r.Status.Candidates == nil {
		r.Status.Candidates = []Candidate{}
	}
	return r
}

// repoReportFrom projects the internal discovery report onto the public contract.
func repoReportFrom(rep internalanalyzer.RepoReport) RepoReport {
	out := RepoReport{
		TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: KindRepoReport},
		Spec:     RepoReportSpec{Root: rep.Root, Mode: ModeScanRepo},
		Status: RepoReportStatus{
			Generator:  generator(),
			Candidates: make([]Candidate, 0, len(rep.Candidates)),
			Summary: RepoSummary{
				CandidatesByLayout:    make(map[Layout]int, len(rep.Summary.CandidatesByLayout)),
				Accepted:              rep.Summary.Accepted,
				Refused:               rep.Summary.Refused,
				FleetRoot:             rep.Summary.FleetRoot,
				UnsupportedConstructs: rep.Summary.UnsupportedConstructs,
			},
		},
	}
	for layout, n := range rep.Summary.CandidatesByLayout {
		out.Status.Summary.CandidatesByLayout[Layout(layout)] = n
	}
	for _, conflict := range rep.Summary.OverlapConflicts {
		out.Status.Summary.OverlapConflicts = append(out.Status.Summary.OverlapConflicts, OverlapConflict{
			Ancestor: conflict.Ancestor, Descendant: conflict.Descendant,
		})
	}
	for _, cand := range rep.Candidates {
		out.Status.Candidates = append(out.Status.Candidates, candidateFrom(cand))
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
		RenderedTypes: RenderedTypes{
			ByNamespace:         cand.RenderedTypes.ByNamespace,
			NamespaceUndeclared: cand.RenderedTypes.NamespaceUndeclared,
		},
	}
	for _, reason := range cand.RefusalReasons {
		out.RefusalReasons = append(out.RefusalReasons, RefusalReason{
			Code:     IssueKind(reason.Code),
			Detail:   reason.Detail,
			Solvable: reason.Solvable,
			Actor:    Actor(reason.Actor),
		})
	}
	return out
}
