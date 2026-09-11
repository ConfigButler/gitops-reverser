// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"fmt"
	"slices"
	"strings"
)

// AcceptanceRefusedError is the writer-facing error for a GitTarget folder the acceptance
// gate refused. It carries every issue so the surface (GitTarget status / a blocked stream)
// can name the offending file and reason. errors.As recovers it from a wrapped flush or
// resync error, so the watch layer can translate a refusal into a Blocked stream while a
// plain write fault keeps its existing handling.
type AcceptanceRefusedError struct {
	Issues []AcceptanceIssue
}

// Error returns a bounded one-liner: the first offending file and reason, plus a count of
// any others. Stable ordering comes from Accept's sortIssues, so the "first" issue is
// deterministic.
func (e *AcceptanceRefusedError) Error() string {
	if len(e.Issues) == 0 {
		return "Git path refused: unspecified unsupported content"
	}
	first := e.Issues[0]
	if len(e.Issues) == 1 {
		return fmt.Sprintf("Git path refused at %s: %s", first.Path, first.Message)
	}
	return fmt.Sprintf("Git path refused at %s: %s (and %d more issue(s))",
		first.Path, first.Message, len(e.Issues)-1)
}

const acceptanceBlockMessageMax = 512
const truncationSuffixLen = 3

// BlockMessage returns a bounded, human-readable one-liner suitable for a GitTarget status
// condition / stream-block message. It keeps Error stable for callers while adding the
// machine-stable issue kind and, when known, the actor who can fix the folder.
func (e *AcceptanceRefusedError) BlockMessage() string {
	if len(e.Issues) == 0 {
		return e.Error()
	}

	first := e.Issues[0]
	parts := []string{"Git path refused"}
	if first.Kind != "" {
		parts = append(parts, "kind="+string(first.Kind))
	}
	if first.Path != "" {
		parts = append(parts, "path="+first.Path)
	}
	if first.DocumentIndex > 0 {
		parts = append(parts, fmt.Sprintf("document=%d", first.DocumentIndex))
	}
	if first.Solvable && first.Actor != ActorUnknown {
		parts = append(parts, "actor="+string(first.Actor))
	}

	msg := strings.Join(parts, " ")
	if first.Message != "" {
		msg += ": " + first.Message
	}
	if hint := issueHint(first); hint != "" {
		msg += "; " + hint
	}
	if len(e.Issues) > 1 {
		msg += fmt.Sprintf(" (and %d more issue(s))", len(e.Issues)-1)
	}
	return capMessage(msg, acceptanceBlockMessageMax)
}

func issueHint(issue AcceptanceIssue) string {
	switch issue.Kind {
	case IssueAmbiguousLayout:
		return "point spec.path at one render root"
	case IssueForeignFile, IssueForeignSymlink, IssueForeignSubmodule:
		return "remove or move unmanaged content"
	case IssueIgnoreShadowsManaged:
		return "narrow .gittargetignore or move the managed resource"
	case IssueMultipleSourceNamespaces:
		return "split targets or serialize namespaces"
	case IssueUnrenderedPlacement:
		return "adjust placement so the file renders from the target root"
	case IssueUnsupportedKustomize:
		if issue.Solvable {
			return "edit the unsupported kustomization feature"
		}
		return "use a supported source layout for this target"
	case IssueWriteEscapesScope:
		return "keep writes under spec.path"
	case IssueDuplicate,
		IssueImpureManagedFile,
		IssueInvalidYAML,
		IssueMixedFile,
		IssueNonKRM,
		IssueOutOfScope,
		IssueRenderDoesNotMatchLive,
		IssueRenderRefused,
		IssueUnplaceableEdit,
		IssueUnresolvedKRM,
		IssueWriteFanIn:
		return ""
	default:
		return ""
	}
}

func capMessage(msg string, limit int) string {
	if len(msg) <= limit {
		return msg
	}
	if limit <= truncationSuffixLen {
		return msg[:limit]
	}
	return msg[:limit-truncationSuffixLen] + "..."
}

// AllIssuesOfKinds reports whether every issue in the refusal is one of the given kinds. The
// surface uses it to pick a precise status reason: a refusal made up purely of
// IssueIgnoreShadowsManaged is the unrecoverable .gittargetignore-shadows-a-write case (§4.3),
// and one made up purely of the write-boundary kinds (IssueWriteEscapesScope, IssueWriteFanIn)
// is a refused write-boundary violation — each deserves its own reason, whereas any mix falls
// back to the umbrella UnsupportedContent. An empty issue set returns false.
func (e *AcceptanceRefusedError) AllIssuesOfKinds(kinds ...IssueKind) bool {
	if len(e.Issues) == 0 {
		return false
	}
	for _, iss := range e.Issues {
		if !slices.Contains(kinds, iss.Kind) {
			return false
		}
	}
	return true
}

// GitPathRefusalReason maps a refusal onto the GitPathAccepted condition reason it is published
// under. A refusal made up PURELY of one recognised shape gets that shape's own reason; any mix
// falls back to the umbrella UnsupportedContent, because a mixed refusal has no single answer and
// naming one of its halves would send the reader to the wrong fix.
//
// It lives here, beside the IssueKind constants it reads, rather than in the watch package that
// publishes it. Both the projection and the corpus that pins the fixtures need the same answer,
// and the corpus cannot import watch (watch imports git). Before this the mapping was in watch
// with three comments elsewhere asking callers to keep their strings in sync with it by hand.
//
// The strings mirror the controller's GitTargetReason* constants — neither package can import
// controller without a cycle — and every one of them is a member of the controller's
// stalled-reason set, so a refusal surfaces as Stalled=True / kstatus Failed whichever it is.
func GitPathRefusalReason(refused *AcceptanceRefusedError) string {
	switch {
	case refused.AllIssuesOfKinds(IssueIgnoreShadowsManaged):
		return "IgnoreShadowsManagedPath"
	case refused.AllIssuesOfKinds(IssueAmbiguousLayout):
		return "AmbiguousLayout"
	case refused.AllIssuesOfKinds(IssueMultipleSourceNamespaces):
		return "MultipleSourceNamespaces"
	case refused.AllIssuesOfKinds(IssueUnrenderedPlacement):
		return "UnrenderedPlacement"
	case refused.AllIssuesOfKinds(
		IssueWriteEscapesScope,
		IssueWriteFanIn,
		IssueRenderRefused,
		IssueUnplaceableEdit,
	):
		return "WriteBoundaryRefused"
	default:
		return "UnsupportedContent"
	}
}

// RefusalError returns an *AcceptanceRefusedError when the acceptance was not accepted, or
// nil when the folder is clean. The writer calls this immediately after running the gate, so
// a refusal aborts the commit before any file is touched.
func RefusalError(acc Acceptance) error {
	if acc.Accepted {
		return nil
	}
	return &AcceptanceRefusedError{Issues: acc.Issues}
}
