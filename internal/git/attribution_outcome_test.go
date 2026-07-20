// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// attributedEvent builds a commit-shaped event carrying an explicit attribution outcome.
func attributedEvent(username string, outcome AttributionOutcome) Event {
	return Event{
		Identifier:         types.NewResourceIdentifier("", "v1", "configmaps", "team-a", "app"),
		Operation:          "CREATE",
		UserInfo:           UserInfo{Username: username},
		Attribution:        outcome,
		GitTargetName:      "target",
		GitTargetNamespace: "default",
	}
}

func commitWrite(events ...Event) PendingWrite {
	return PendingWrite{Kind: PendingWriteCommit, Events: events}
}

// An attribution that RAN and did not resolve must NOT be authored by the committer. Authoring
// it as the committer is what made a lost actor byte-identical to a configured-author commit,
// so the loss was invisible in git history.
func TestCommitOptionsFor_UnresolvedAttributionUsesSentinelAuthor(t *testing.T) {
	config := ResolveCommitConfig(nil)
	when := time.Now()

	options := commitOptionsFor(commitWrite(attributedEvent("", AttributionUnresolved)), config, nil, when)

	require.NotNil(t, options.Author)
	require.NotNil(t, options.Committer)
	assert.Equal(t, UnresolvedAuthorDisplayName, options.Author.Name)
	assert.Equal(t, UnresolvedAuthorEmail, options.Author.Email)
	assert.Equal(t, DefaultCommitterName, options.Committer.Name,
		"the operator really did write the commit, so it stays the committer")
	assert.NotEqual(t, options.Committer.Name, options.Author.Name,
		"an unresolved attribution must be distinguishable from a configured-author commit")
}

// Configured-author mode is untouched: nothing was attempted, so the committer legitimately
// authors the commit. This is the case that MUST stay identical to the sentinel case's
// opposite — the whole point is that the two are now told apart.
func TestCommitOptionsFor_NotAttemptedStillAuthorsAsCommitter(t *testing.T) {
	config := ResolveCommitConfig(nil)
	when := time.Now()

	options := commitOptionsFor(commitWrite(attributedEvent("", AttributionNotAttempted)), config, nil, when)

	require.NotNil(t, options.Author)
	assert.Equal(t, DefaultCommitterName, options.Author.Name)
	assert.Equal(t, DefaultCommitterEmail, options.Author.Email)
	assert.Equal(t, options.Committer.Name, options.Author.Name)
}

func TestCommitOptionsFor_ResolvedAttributionNamesTheActor(t *testing.T) {
	config := ResolveCommitConfig(nil)

	options := commitOptionsFor(
		commitWrite(attributedEvent("jane@acme.com", AttributionResolved)), config, nil, time.Now())

	require.NotNil(t, options.Author)
	assert.Equal(t, "jane@acme.com", options.Author.Name)
	assert.NotEqual(t, UnresolvedAuthorDisplayName, options.Author.Name)
}

// The sentinel is written into a git signature header, so it must survive the same safety
// check every other author value does — spaces and parentheses included.
func TestUnresolvedAuthor_IsSafeInASignatureHeader(t *testing.T) {
	user := UnresolvedAuthor()
	assert.True(t, isSafeSignatureField(user.DisplayName),
		"the display form must be placeable verbatim in a git signature")
	assert.Equal(t, UnresolvedAuthorDisplayName, authorName(user))
	assert.Equal(t, UnresolvedAuthorEmail, authorEmail(user),
		"the reserved .invalid address must pass email validation and be used verbatim")
}

// author_kind drives dashboards. An unresolved attribution must never be counted as a named
// user: that would make a DEGRADING attribution path read as an improving one.
func TestAuthorKind_ClassifiesAllFourKinds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write PendingWrite
		want  string
	}{
		{
			name:  "resolved human is a user",
			write: commitWrite(attributedEvent("jane@acme.com", AttributionResolved)),
			want:  authorKindUser,
		},
		{
			name:  "resolved service account is a serviceaccount",
			write: commitWrite(attributedEvent("system:serviceaccount:flux-system:kustomize", AttributionResolved)),
			want:  authorKindServiceAccount,
		},
		{
			name:  "attribution never attempted is the committer",
			write: commitWrite(attributedEvent("", AttributionNotAttempted)),
			want:  authorKindCommitter,
		},
		{
			name:  "attribution attempted and unresolved is its own kind",
			write: commitWrite(attributedEvent("", AttributionUnresolved)),
			want:  authorKindUnresolved,
		},
		{
			name:  "an atomic write never attempts attribution",
			write: PendingWrite{Kind: PendingWriteAtomic},
			want:  authorKindCommitter,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.write.authorKind())
		})
	}
}

// Even if a sentinel username ever reached authorKind through a path that lost the outcome,
// it must not be silently counted as a real user. This pins the intent rather than the
// mechanism, so a future refactor that drops the outcome check fails here.
func TestAuthorKind_SentinelUsernameIsNeverCountedAsAUser(t *testing.T) {
	write := commitWrite(attributedEvent(UnresolvedAuthorUsername, AttributionUnresolved))
	assert.Equal(t, authorKindUnresolved, write.authorKind())
	assert.NotEqual(t, authorKindUser, write.authorKind())
}

// Window coalescing is keyed on (outcome, author): two unresolved events belong together
// exactly as they did when both carried an empty username, and an unresolved event must never
// join a resolved one.
func TestOpenWindow_CanAppendMatchesOnOutcomeAndAuthor(t *testing.T) {
	unresolved := attributedEvent("", AttributionUnresolved)
	window := newOpenWindow(unresolved, nil)

	assert.True(t, window.canAppend(attributedEvent("", AttributionUnresolved)),
		"two unresolved events share one window")
	assert.False(t, window.canAppend(attributedEvent("jane@acme.com", AttributionResolved)),
		"a named actor must not be folded into an unresolved window")
	assert.False(t, window.canAppend(attributedEvent("", AttributionNotAttempted)),
		"configured-author writes must not be folded into an unresolved window")
}

func TestOpenWindow_ResolvedWindowsStillSplitByAuthor(t *testing.T) {
	window := newOpenWindow(attributedEvent("jane@acme.com", AttributionResolved), nil)

	assert.True(t, window.canAppend(attributedEvent("jane@acme.com", AttributionResolved)))
	assert.False(t, window.canAppend(attributedEvent("bob@acme.com", AttributionResolved)))
}

// THE REGRESSION THIS DESIGN EXISTS TO AVOID.
//
// A CommitRequest that could not be attributed carries an empty author, and so does a window
// whose attribution ran and named nobody — the sentinel is a git-signature rendering only and
// never reaches the window's Author. The two therefore still match on the author, and the
// outcomes must not be allowed to push them apart: the request's outcome comes from command
// authorship and the window's from mirrored-resource attribution, which are configured
// independently, so they routinely differ. See the full cross-subsystem matrix in
// TestMatchesWindow_AcrossIndependentlyConfiguredSubsystems.
func TestPendingCommitRequest_UnresolvedRequestAttachesToUnresolvedWindow(t *testing.T) {
	window := newOpenWindow(attributedEvent("", AttributionUnresolved), nil)
	require.Empty(t, window.Author,
		"an unresolved window carries no author; the sentinel is applied at the write path")

	request := &pendingCommitRequest{
		author:             "", // a fallback CommitRequest has no author
		attribution:        AttributionUnresolved,
		gitTargetName:      "target",
		gitTargetNamespace: "default",
	}
	assert.True(t, request.matchesWindow(window),
		"a fallback CommitRequest must attach to the unresolved window it was meant for")

	// The default deployment: command-author capture is off, so the request says "not
	// attempted" while the window says "unresolved". Neither names an actor, so they match.
	notAttempted := &pendingCommitRequest{
		author:             "",
		attribution:        AttributionNotAttempted,
		gitTargetName:      "target",
		gitTargetNamespace: "default",
	}
	assert.True(t, notAttempted.matchesWindow(window),
		"a request from a deployment with command-author capture off must still attach; "+
			"requiring the two subsystems' outcomes to be equal drops the user's commit message")
}

func TestPendingCommitRequest_DoesNotCrossAttributionOutcomes(t *testing.T) {
	resolvedWindow := newOpenWindow(attributedEvent("jane@acme.com", AttributionResolved), nil)

	unresolvedRequest := &pendingCommitRequest{
		author:             "",
		attribution:        AttributionUnresolved,
		gitTargetName:      "target",
		gitTargetNamespace: "default",
	}
	assert.False(t, unresolvedRequest.matchesWindow(resolvedWindow),
		"an unattributed request must not claim a named actor's window")

	resolvedRequest := &pendingCommitRequest{
		author:             "jane@acme.com",
		attribution:        AttributionResolved,
		gitTargetName:      "target",
		gitTargetNamespace: "default",
	}
	assert.True(t, resolvedRequest.matchesWindow(resolvedWindow))

	otherActor := &pendingCommitRequest{
		author:             "bob@acme.com",
		attribution:        AttributionResolved,
		gitTargetName:      "target",
		gitTargetNamespace: "default",
	}
	assert.False(t, otherActor.matchesWindow(resolvedWindow),
		"a resolved request still matches on the actor")
}

func TestPendingCommitRequest_StillScopedToOneGitTarget(t *testing.T) {
	window := newOpenWindow(attributedEvent("", AttributionUnresolved), nil)
	request := &pendingCommitRequest{
		author:             "",
		attribution:        AttributionUnresolved,
		gitTargetName:      "other-target",
		gitTargetNamespace: "default",
	}
	assert.False(t, request.matchesWindow(window),
		"the outcome match must not widen attachment across GitTargets")
}

// The sentinel is scoped to the git author header and deliberately does NOT reach message
// bodies or user-authored {{.Username}} templates — it is derived at the write path from the
// outcome, never stamped onto the event (see UnresolvedAuthor). An unresolved event therefore
// renders {{.Username}} exactly as configured-author mode always has: empty. Pushing a magic
// token in here would change the commit text of every deployment that has attribution misses
// and force user templates to special-case a value they never had to handle.
func TestRenderEventCommitMessage_UnresolvedUsernameStaysEmptyInTemplates(t *testing.T) {
	config := ResolveCommitConfig(nil)
	config.Message.EventTemplate = "{{.Operation}} by {{.Username}}"

	// The production shape: attachAuthor sets UserInfo only when the outcome is resolved.
	event := attributedEvent("", AttributionUnresolved)
	message, err := renderEventCommitMessage(event, config)

	require.NoError(t, err)
	assert.Equal(t, "CREATE by ", message,
		"the sentinel must not leak into user-authored templates")

	// The identity a human actually sees for this commit lives in the git author header, which
	// TestCommitOptionsFor_UnresolvedAttributionUsesSentinelAuthor pins.
	options := commitOptionsFor(commitWrite(event), ResolveCommitConfig(nil), nil, time.Now())
	require.NotNil(t, options.Author)
	assert.Equal(t, UnresolvedAuthorDisplayName, options.Author.Name)
}
