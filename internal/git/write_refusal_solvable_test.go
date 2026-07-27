// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
)

// The write path raises four of the operator's refusal kinds, and each one has to say
// whether it can ever stop being a refusal — the same contract the acceptance gate now
// carries. Classifying only the folder-level kinds would leave a consumer reading a
// GitTarget's status with the same "not supported yet" guess the ask exists to kill.
//
// See docs/design/analyzer-consumer-contract-asks.md (Ask 1).

// refusalIssues unwraps an AcceptanceRefusedError and returns the issues it carries.
func refusalIssues(t *testing.T, err error) []manifestanalyzer.AcceptanceIssue {
	t.Helper()
	require.Error(t, err)
	var refused *manifestanalyzer.AcceptanceRefusedError
	require.ErrorAs(t, err, &refused, "error should be an AcceptanceRefusedError: %v", err)
	require.NotEmpty(t, refused.Issues)
	return refused.Issues
}

// issueKinds projects the kinds out of a refusal's issues.
func issueKinds(issues []manifestanalyzer.AcceptanceIssue) []manifestanalyzer.IssueKind {
	kinds := make([]manifestanalyzer.IssueKind, 0, len(issues))
	for _, issue := range issues {
		kinds = append(kinds, issue.Kind)
	}
	return kinds
}

// assertClassified holds the invariant every emitted refusal must satisfy: it names an
// actor exactly when someone can act.
func assertClassified(t *testing.T, issues []manifestanalyzer.AcceptanceIssue) {
	t.Helper()
	for _, issue := range issues {
		if issue.Solvable {
			assert.NotEqual(t, manifestanalyzer.ActorUnknown, issue.Actor,
				"%s is solvable but does not say by whom", issue.Kind)
		} else {
			assert.Equal(t, manifestanalyzer.ActorUnknown, issue.Actor,
				"%s names an actor for a refusal nobody can act on", issue.Kind)
		}
	}
}

// TestUnplaceableEditRefusalIsNotSolvable pins the classification of the refusal nobody
// can act on: the alternative to refusing is aligning two lists by position, which is
// measurably wrong rather than merely risky.
func TestUnplaceableEditRefusalIsNotSolvable(t *testing.T) {
	err := sourceFormRefusal("apps/web/deploy.yaml",
		manifestedit.Identity{Kind: "Deployment", Name: "web"}, assert.AnError)

	issues := refusalIssues(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, manifestanalyzer.IssueUnplaceableEdit, issues[0].Kind)
	assert.False(t, issues[0].Solvable)
	assertClassified(t, issues)
}

// TestRenderFidelityRefusalIsSolvableByThePlatform pins the other half: a live value that
// diverges from what the folder renders is out-of-band substitution, not a render
// artifact, so whoever owns the deployment pipeline can reconcile the two — and the
// repository author, who is the one usually on the screen, cannot.
func TestRenderFidelityRefusalIsSolvableByThePlatform(t *testing.T) {
	err := renderFidelityRefusal("apps/web/deploy.yaml",
		manifestedit.Identity{Kind: "Deployment", Name: "web"},
		&renderFidelityRefusedError{Divergences: []manifestanalyzer.RenderDivergence{
			{Field: "spec.replicas", Token: "${REPLICAS}"},
		}})

	issues := refusalIssues(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, manifestanalyzer.IssueRenderDoesNotMatchLive, issues[0].Kind)
	assert.True(t, issues[0].Solvable)
	assert.Equal(t, manifestanalyzer.ActorPlatformOperator, issues[0].Actor)
	assertClassified(t, issues)
}

// TestIgnoreShadowRefusalIsSolvableByTheAuthor drives the DYNAMIC half of the
// .gittargetignore guard. The parse-time denylist catches only a catastrophic pattern; a
// narrow one passes the initial scan and can still match a write planned later, and that
// refusal reaches the same reader with the same question to answer.
func TestIgnoreShadowRefusalIsSolvableByTheAuthor(t *testing.T) {
	matcher, parseIssues := manifestanalyzer.LoadGitTargetIgnore([]byte("secrets/\n"))
	require.Empty(t, parseIssues, "a narrow pattern must pass the parse-time denylist")

	wb := &writeBatch{
		store: &manifestanalyzer.ManifestStore{Ignore: matcher},
		buffers: map[string]*fileBuffer{
			"secrets/db.yaml": {rel: "secrets/db.yaml", current: []byte("a")},
		},
	}

	issues := refusalIssues(t, wb.ignoreShadowPrecondition())
	require.Len(t, issues, 1)
	assert.Equal(t, manifestanalyzer.IssueIgnoreShadowsManaged, issues[0].Kind)
	assert.True(t, issues[0].Solvable)
	assert.Equal(t, manifestanalyzer.ActorRepositoryAuthor, issues[0].Actor)
	assertClassified(t, issues)
}

// TestPathScopeRefusalIsSolvableByThePlatform drives the real L1 precondition: widening
// spec.path, or re-placing the write, is the GitTarget owner's call and nobody else's.
func TestPathScopeRefusalIsSolvableByThePlatform(t *testing.T) {
	wb := &writeBatch{buffers: map[string]*fileBuffer{
		"../escape.yaml": {rel: "../escape.yaml", current: []byte("b")},
	}}

	issues := refusalIssues(t, wb.pathScopePrecondition())
	require.Len(t, issues, 1)
	assert.Equal(t, manifestanalyzer.IssueWriteEscapesScope, issues[0].Kind)
	assert.True(t, issues[0].Solvable)
	assert.Equal(t, manifestanalyzer.ActorPlatformOperator, issues[0].Actor)
	assertClassified(t, issues)
}
