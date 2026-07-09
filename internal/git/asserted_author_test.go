// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthorUserInfo_AssertedAuthorWinsOverEventAuthor(t *testing.T) {
	t.Parallel()

	asserted := UserInfo{Username: "Ada Lovelace", DisplayName: "Ada Lovelace", Email: "ada@example.com"}
	pendingWrite := PendingWrite{
		Kind:           PendingWriteCommit,
		Events:         []Event{{UserInfo: UserInfo{Username: "alice"}}},
		AssertedAuthor: &asserted,
	}

	// The assertion is the more specific, more recent statement, made by a caller who had
	// to hold an RBAC verb to make it.
	assert.Equal(t, asserted, pendingWrite.AuthorUserInfo())
	assert.Equal(t, "Ada Lovelace", pendingWrite.Author())
}

// An atomic write has no per-event author at all. An assertion still names one.
func TestAuthorUserInfo_AssertedAuthorAppliesToAtomicWrite(t *testing.T) {
	t.Parallel()

	asserted := UserInfo{Username: "Ada Lovelace"}
	pendingWrite := PendingWrite{Kind: PendingWriteAtomic, AssertedAuthor: &asserted}
	assert.Equal(t, asserted, pendingWrite.AuthorUserInfo())
}

func TestAuthorUserInfo_NoAssertionKeepsEventAuthor(t *testing.T) {
	t.Parallel()

	pendingWrite := PendingWrite{
		Kind:   PendingWriteCommit,
		Events: []Event{{UserInfo: UserInfo{Username: "alice"}}},
	}
	assert.Equal(t, "alice", pendingWrite.AuthorUserInfo().Username)
}

func TestCommitOptionsFor_AssertedAuthorSignsAsAuthorNotCommitter(t *testing.T) {
	t.Parallel()

	config := ResolveCommitConfig(nil)
	asserted := UserInfo{Username: "Ada Lovelace", DisplayName: "Ada Lovelace", Email: "ada@example.com"}
	pendingWrite := PendingWrite{
		Kind:           PendingWriteCommit,
		Events:         []Event{{UserInfo: UserInfo{Username: "alice"}}},
		AssertedAuthor: &asserted,
	}

	options := commitOptionsFor(pendingWrite, config, nil, time.Now())

	require.NotNil(t, options.Author)
	assert.Equal(t, "Ada Lovelace", options.Author.Name)
	assert.Equal(t, "ada@example.com", options.Author.Email)

	// A reader can always tell the reverser made the commit on someone's behalf.
	require.NotNil(t, options.Committer)
	assert.Equal(t, DefaultCommitterName, options.Committer.Name)
	assert.Equal(t, DefaultCommitterEmail, options.Committer.Email)
}

// An omitted email is derived from the name, exactly as it is for an audit-attributed
// author whose token carried no email claim.
func TestCommitOptionsFor_AssertedAuthorWithoutEmailGetsSafeAddress(t *testing.T) {
	t.Parallel()

	asserted := UserInfo{Username: "Ada Lovelace", DisplayName: "Ada Lovelace"}
	pendingWrite := PendingWrite{Kind: PendingWriteCommit, AssertedAuthor: &asserted}

	options := commitOptionsFor(pendingWrite, ResolveCommitConfig(nil), nil, time.Now())

	require.NotNil(t, options.Author)
	assert.Equal(t, "Ada Lovelace", options.Author.Name)
	assert.NotEmpty(t, options.Author.Email)
	assert.NotEqual(t, DefaultCommitterEmail, options.Author.Email)
}

func window(author, target, namespace string) *openWindow {
	return &openWindow{Author: author, GitTarget: target, GitTargetNamespace: namespace}
}

// Without an assertion the request finalizes "the requesting author's open window",
// never someone else's.
func TestPendingCommitRequest_MatchesWindow_AuditAuthorBound(t *testing.T) {
	t.Parallel()

	pcr := &pendingCommitRequest{author: "alice", gitTargetName: "tenants", gitTargetNamespace: "team-a"}

	assert.True(t, pcr.matchesWindow(window("alice", "tenants", "team-a")))
	assert.False(t, pcr.matchesWindow(window("bob", "tenants", "team-a")), "a foreign window stays open")
	assert.False(t, pcr.matchesWindow(window("alice", "other", "team-a")))
	assert.False(t, pcr.matchesWindow(window("alice", "tenants", "team-b")))
	assert.False(t, pcr.matchesWindow(nil))
}

// An assertion binds to any open window for the GitTarget: it is a statement about the
// commit being made, not a claim to be whichever actor the audit stream recorded.
func TestPendingCommitRequest_MatchesWindow_AssertedAuthorBindsAnyWindow(t *testing.T) {
	t.Parallel()

	asserted := UserInfo{Username: "Ada Lovelace"}
	pcr := &pendingCommitRequest{
		author:             "gitops-api",
		assertedAuthor:     &asserted,
		gitTargetName:      "tenants",
		gitTargetNamespace: "team-a",
	}

	assert.True(t, pcr.matchesWindow(window("someone-else", "tenants", "team-a")))
	assert.True(t, pcr.matchesWindow(window("", "tenants", "team-a")), "an unattributed window matches too")

	// The GitTarget scope still holds: assert-author is granted per GitTarget, so a
	// request can never reach another target's window.
	assert.False(t, pcr.matchesWindow(window("someone-else", "other", "team-a")))
	assert.False(t, pcr.matchesWindow(window("someone-else", "tenants", "team-b")))
}
