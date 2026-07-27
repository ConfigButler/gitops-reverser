// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"

	internalanalyzer "github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
)

// IssueKind classifies why a folder was not accepted. The values are the operator's own
// refusal codes. They are the part of an [Issue] worth matching on, and they change less
// often than the surrounding shape — but pre-1.0 they can still change.
//
// The set below is complete: every kind the operator can raise is declared here, including
// the three that only a live write path emits (they reach a consumer through GitTarget
// status rather than through [ScanFolder] or [ScanRepo], which are structure-only). A
// consumer matching on these constants can recognise every code it will ever be handed.
type IssueKind string

const (
	// IssueDuplicate marks a document that duplicates an earlier manifest identity.
	IssueDuplicate IssueKind = "duplicate-identity"
	// IssueNonKRM marks YAML that does not parse as a Kubernetes manifest.
	IssueNonKRM IssueKind = "non-krm-yaml"
	// IssueInvalidYAML marks a document that does not parse as YAML.
	IssueInvalidYAML IssueKind = "invalid-yaml"
	// IssueImpureManagedFile marks a file holding managed resources that also holds a
	// non-managed document. A managed file may contain only valid KRM documents.
	IssueImpureManagedFile IssueKind = "impure-managed-file"
	// IssueMixedFile marks a managed file that also holds an allowlisted non-API KRM
	// document (a kustomization). Allowlisted KRM must be retained in its own file.
	IssueMixedFile IssueKind = "mixed-managed-allowlisted"
	// IssueUnresolvedKRM marks recognized KRM that cannot be tied to a single served,
	// followable resource. Only a cluster-aware scan reports this.
	IssueUnresolvedKRM IssueKind = "unresolved-krm"
	// IssueOutOfScope marks a watched kind whose resource falls outside the GitTarget's
	// scope (right kind, wrong namespace). Only a cluster-aware scan reports this.
	IssueOutOfScope IssueKind = "out-of-scope"
	// IssueUnsupportedKustomize marks a kustomization.yaml using a feature the writer
	// cannot map back to editable source documents (generators, patches, components,
	// Helm inflation, replacements, transformers, name prefixes, remote bases).
	IssueUnsupportedKustomize IssueKind = "unsupported-kustomize"
	// IssueForeignFile marks a non-YAML regular file the operator cannot manage.
	IssueForeignFile IssueKind = "foreign-file"
	// IssueForeignSymlink marks a symlink, which a writer could follow out of the subtree.
	IssueForeignSymlink IssueKind = "foreign-symlink"
	// IssueForeignSubmodule marks a nested Git submodule.
	IssueForeignSubmodule IssueKind = "foreign-submodule"
	// IssueIgnoreShadowsManaged marks a .gittargetignore pattern that matches a path the
	// operator writes, which would blind it to its own file.
	IssueIgnoreShadowsManaged IssueKind = "ignore-shadows-managed"
	// IssueWriteEscapesScope marks a planned write that would leave the GitTarget's path.
	IssueWriteEscapesScope IssueKind = "write-escapes-scope"
	// IssueWriteFanIn marks an in-place edit of a source file that more than one kustomize
	// render root reaches.
	IssueWriteFanIn IssueKind = "write-fan-in"
	// IssueRenderRefused marks a planned write kustomize itself will not vouch for: the
	// flush was re-rendered with the write applied, and the result was not the live
	// object. Only a live write raises it; a structure-only scan never can.
	IssueRenderRefused IssueKind = "kustomize-render-refused"
	// IssueRenderDoesNotMatchLive marks a rendered ${...} value whose corresponding live
	// field is absent or different. Only a live write raises it.
	IssueRenderDoesNotMatchLive IssueKind = "render-does-not-match-live"
	// IssueUnplaceableEdit marks a live change the writer could not place in any source
	// document. Only a live write raises it.
	IssueUnplaceableEdit IssueKind = "unplaceable-edit"
)

// Permanence says whether a refusal can ever stop being one. It is set by the check that
// raised the refusal, because only that check knows — several codes classify differently
// depending on which branch emitted them, which is exactly why this is a field on the
// emitted value and not a table beside the constants.
//
// Consumers MUST treat an unrecognised or absent value as [PermanenceUnknown] and say
// nothing about the future. "Not supported yet" is a wait, "cannot be synced" is a
// redesign, and "fix your YAML" is neither; a code alone cannot tell them apart.
type Permanence string

const (
	// PermanenceUnknown is the zero value: not classified. Say nothing.
	PermanenceUnknown Permanence = ""
	// PermanenceFixable means changing the repository or the GitTarget clears it today.
	// [Actor] says who can make that change.
	PermanenceFixable Permanence = "fixable"
	// PermanencePending means a future release may accept it. Nobody can clear it today.
	PermanencePending Permanence = "pending-upstream"
	// PermanencePermanent is the support boundary: never a "not yet".
	PermanencePermanent Permanence = "permanent"
)

// Actor names who can act on a fixable refusal. It is empty when the refusal is not
// fixable, or when nobody can act.
//
// It matters because some refusals are fixable ONLY by the person who owns the GitTarget,
// who is often not the person reading the message: rendering an out-of-scope refusal as
// "fix your repository" to a repository author who cannot is worse than saying nothing.
type Actor string

const (
	// ActorUnknown is the zero value: unclassified, or nobody can act.
	ActorUnknown Actor = ""
	// ActorAuthor is the person who owns the files in the repository.
	ActorAuthor Actor = "repository-author"
	// ActorPlatform is the person who owns the GitTarget — its scope, its path, and the
	// CRDs installed in the cluster it mirrors.
	ActorPlatform Actor = "platform-operator"
)

// Issue is one reason a folder is not accepted, or one fact a stricter policy may treat
// as blocking.
type Issue struct {
	Kind IssueKind `json:"kind"`
	// Path is the offending file, slash-separated and relative to the scan root. Empty
	// when the issue is about the folder as a whole.
	Path string `json:"path"`
	// DocumentIndex is the zero-based index of the offending document within Path.
	DocumentIndex int `json:"documentIndex"`
	// Message is a human-readable explanation. It is not a stable string.
	Message string `json:"message"`
	// Permanence is empty when the check did not classify itself.
	Permanence Permanence `json:"permanence,omitempty"`
	// Actor is empty unless Permanence is [PermanenceFixable].
	Actor Actor `json:"actor,omitempty"`
}

// Identity names one Kubernetes document as it appears in the file.
type Identity struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
}

// RetainedDocument is content the operator keeps but never writes: build directives such
// as a kustomization.yaml. They are read as context, never edited as resources.
type RetainedDocument struct {
	Path          string `json:"path"`
	DocumentIndex int    `json:"documentIndex"`
	// Identity is set only when a named Kubernetes resource is hiding inside an
	// allowlisted build-directive file — the mixed-file case, which is refused. It is nil
	// for the ordinary whole-file retention of a kustomization.yaml, which names no
	// resource.
	Identity *Identity `json:"identity,omitempty"`
	// Unsupported reports that this retained file uses a construct the writer cannot map
	// back to editable source. It is the cause of an IssueUnsupportedKustomize.
	Unsupported bool `json:"unsupported,omitempty"`
}

// FolderReport answers "may this folder become a GitTarget?". It is a KRM document: the
// scan REQUEST is the spec, what was FOUND is the status. See [TypeMeta] for why the
// envelope, and why the document is never served or applyable.
type FolderReport struct {
	TypeMeta `json:",inline"`

	Spec   FolderReportSpec   `json:"spec"`
	Status FolderReportStatus `json:"status"`
}

// FolderReportSpec is the scan that was asked for.
type FolderReportSpec struct {
	// Root is the scanned folder as passed to ScanFolder. Empty for ScanFolderFS.
	Root string `json:"root,omitempty"`
	// Mode is always [ModeScanFolder].
	Mode string `json:"mode"`
}

// FolderReportStatus is what the scan found.
type FolderReportStatus struct {
	// Generator names the build that produced this report. Never empty.
	Generator Generator `json:"generator"`
	// Accepted is the gate decision. When false, Issues says why.
	Accepted bool `json:"accepted"`
	// Issues is empty when Accepted, and never nil in the marshaled JSON.
	Issues []Issue `json:"issues"`
	// Retained lists the build directives read as context.
	Retained []RetainedDocument `json:"retained,omitempty"`
}

// ScanFolder runs the adoption gate over a folder on disk. It is read-only, needs no
// cluster, and never follows symlinks.
//
// The returned error covers only I/O: a folder that cannot be adopted is a successful
// scan with Accepted=false, not an error.
func ScanFolder(ctx context.Context, root string) (FolderReport, error) {
	result, err := internalanalyzer.ScanDir(ctx, root, nil, nil, folderScanPolicy())
	if err != nil {
		return FolderReport{}, err
	}
	report := folderReportFrom(result.Acceptance)
	report.Spec.Root = root
	return report, nil
}

// ScanFolderFS is ScanFolder over an arbitrary [io/fs.FS] — an in-memory tree, a tarball,
// or a Git tree exposed as a filesystem. It cannot fail: an unreadable file becomes an
// issue, not an error.
func ScanFolderFS(ctx context.Context, fsys fs.FS) FolderReport {
	return folderReportFrom(internalanalyzer.Scan(ctx, fsys, nil, nil, folderScanPolicy()).Acceptance)
}

// WriteJSON writes the report as indented JSON — byte-for-byte what
// `manifest-analyzer --mode scan-folder --format json` prints.
func (r FolderReport) WriteJSON(w io.Writer) error {
	if r.Status.Issues == nil {
		r.Status.Issues = []Issue{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// folderScanPolicy is the structure-only adoption gate: the default allowlist, no
// followability lookup, and no desired state. It is the same policy the CLI's scan-folder mode
// applies, kept in one place so the two can never disagree.
func folderScanPolicy() internalanalyzer.ScanPolicy {
	return internalanalyzer.ScanPolicy{
		Acceptance: internalanalyzer.AcceptancePolicy{Allowlist: internalanalyzer.DefaultAllowlist()},
		Plan:       internalanalyzer.FolderScanPlanPolicy(),
	}
}

// folderReportFrom projects the internal acceptance decision onto the public contract.
// It is deliberately a copy rather than a type alias: the public shape must be free to
// stay still while the internal one moves.
func folderReportFrom(acc internalanalyzer.Acceptance) FolderReport {
	report := FolderReport{
		TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: KindFolderReport},
		Spec:     FolderReportSpec{Mode: ModeScanFolder},
		Status: FolderReportStatus{
			Generator: generator(),
			Accepted:  acc.Accepted,
			Issues:    make([]Issue, 0, len(acc.Issues)),
		},
	}
	for _, issue := range acc.Issues {
		report.Status.Issues = append(report.Status.Issues, Issue{
			Kind:          IssueKind(issue.Kind),
			Path:          issue.Path,
			DocumentIndex: issue.DocumentIndex,
			Message:       issue.Message,
			Permanence:    Permanence(issue.Permanence),
			Actor:         Actor(issue.Actor),
		})
	}
	for _, rd := range acc.Retained {
		doc := RetainedDocument{
			Path:          rd.Location.Path,
			DocumentIndex: rd.Location.DocumentIndex,
			Unsupported:   rd.Unsupported,
		}
		if rd.Identity.Kind != "" {
			doc.Identity = &Identity{
				APIVersion: rd.Identity.APIVersion,
				Kind:       rd.Identity.Kind,
				Namespace:  rd.Identity.Namespace,
				Name:       rd.Identity.Name,
			}
		}
		report.Status.Retained = append(report.Status.Retained, doc)
	}
	return report
}
