// SPDX-License-Identifier: Apache-2.0

package manifestanalyzer

import (
	"fmt"
	"slices"
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

// BlockMessage returns a bounded, human-readable one-liner suitable for a GitTarget status
// condition / stream-block message. It is the same text as Error today, named separately so
// the surface intent is explicit at the call site.
func (e *AcceptanceRefusedError) BlockMessage() string { return e.Error() }

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

// RefusalError returns an *AcceptanceRefusedError when the acceptance was not accepted, or
// nil when the folder is clean. The writer calls this immediately after running the gate, so
// a refusal aborts the commit before any file is touched.
func RefusalError(acc Acceptance) error {
	if acc.Accepted {
		return nil
	}
	return &AcceptanceRefusedError{Issues: acc.Issues}
}
