// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/queue"
)

func assertingCommitRequest(name string) *configv1alpha3.CommitRequest {
	cr := newCommitRequest(name)
	cr.Spec.Author = &configv1alpha3.CommitAuthor{Name: "Ada Lovelace", Email: "ada@example.com"}
	return cr
}

// authorizedSubmitter is the admission record the webhook writes after a SubjectAccessReview
// said the submitter holds assert-author on the GitTarget.
func authorizedSubmitter() *fakeAuthorLookup {
	return &fakeAuthorLookup{
		author: queue.CommandAuthor{Author: "gitops-api", AssertAuthorAllowed: true},
		found:  true,
	}
}

func TestCommitRequestReconcile_AssertedAuthorSignsTheCommit(t *testing.T) {
	cr := assertingCommitRequest("save-asserted")
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result:   git.FinalizeResult{Outcome: git.FinalizeCommitted, SHA: "abc123", Branch: "main"},
		resolved: true,
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: authorizedSubmitter()}

	reconcileCommitRequest(t, r, "save-asserted")

	require.Len(t, f.calls, 1)
	require.NotNil(t, f.calls[0].AssertedAuthor, "the asserted identity must reach the worker")
	assert.Equal(t, "Ada Lovelace", f.calls[0].AssertedAuthor.Username)
	assert.Equal(t, "Ada Lovelace", f.calls[0].AssertedAuthor.DisplayName)
	assert.Equal(t, "ada@example.com", f.calls[0].AssertedAuthor.Email)
	assert.Equal(t, "gitops-api", f.calls[0].Author, "the submitter is still carried, for logging")

	got := fetchCommitRequest(t, c, "save-asserted")
	requireCondition(t, got, ConditionTypeAuthorAttributed, metav1.ConditionTrue, crReasonAuthorAsserted)
}

// The webhook is failurePolicy: Ignore, so a guard living only there would be bypassable
// by taking the webhook down. The controller is the real gate.
func TestCommitRequestReconcile_UnauthorizedAssertionIsIgnored(t *testing.T) {
	tests := map[string]*fakeAuthorLookup{
		"no admission record at all": {found: false},
		"record without the verdict": {
			author: queue.CommandAuthor{Author: "mallory", AssertAuthorAllowed: false},
			found:  true,
		},
	}

	for name, lookup := range tests {
		t.Run(name, func(t *testing.T) {
			cr := assertingCommitRequest("save-unverified")
			c := newCommitRequestClient(t, nil, cr)
			f := &fakeFinalizer{
				result:   git.FinalizeResult{Outcome: git.FinalizeCommitted, SHA: "abc123", Branch: "main"},
				resolved: true,
			}
			r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: lookup}

			reconcileCommitRequest(t, r, "save-unverified")

			require.Len(t, f.calls, 1)
			assert.Nil(t, f.calls[0].AssertedAuthor, "an unverified assertion never reaches the worker")
			assert.Empty(t, f.calls[0].Author, "and it does not silently fall back to the submitter either")

			got := fetchCommitRequest(t, c, "save-unverified")
			requireCondition(t, got, ConditionTypeAuthorAttributed, metav1.ConditionFalse,
				crReasonAuthorAssertionUnverified)
			// Not a failure: the commit is still made, authored by the committer.
			requireCondition(t, got, ConditionTypeReady, metav1.ConditionTrue, crReasonCommitted)
		})
	}
}

// Without the webhook there is no AuthorLookup at all, so an assertion cannot be backed.
func TestCommitRequestReconcile_AssertionWithoutAuthorLookupIsIgnored(t *testing.T) {
	cr := assertingCommitRequest("save-no-lookup")
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result:   git.FinalizeResult{Outcome: git.FinalizeCommitted, SHA: "abc123", Branch: "main"},
		resolved: true,
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f}

	reconcileCommitRequest(t, r, "save-no-lookup")

	require.Len(t, f.calls, 1)
	assert.Nil(t, f.calls[0].AssertedAuthor)

	got := fetchCommitRequest(t, c, "save-no-lookup")
	requireCondition(t, got, ConditionTypeAuthorAttributed, metav1.ConditionFalse,
		crReasonAuthorAssertionUnverified)
}

// A CommitRequest with no spec.author keeps the pre-existing behavior exactly.
func TestCommitRequestReconcile_NoAssertionKeepsAdmissionAttribution(t *testing.T) {
	cr := newCommitRequest("save-plain")
	c := newCommitRequestClient(t, nil, cr)
	f := &fakeFinalizer{
		result:   git.FinalizeResult{Outcome: git.FinalizeCommitted, SHA: "abc123", Branch: "main"},
		resolved: true,
	}
	r := &CommitRequestReconciler{Client: c, APIReader: c, Finalizer: f, AuthorLookup: attributedAlice()}

	reconcileCommitRequest(t, r, "save-plain")

	require.Len(t, f.calls, 1)
	assert.Nil(t, f.calls[0].AssertedAuthor)
	assert.Equal(t, "alice", f.calls[0].Author)

	got := fetchCommitRequest(t, c, "save-plain")
	requireCondition(t, got, ConditionTypeAuthorAttributed, metav1.ConditionTrue, crReasonAttributedFromAdmission)
}

func TestAssertedUserInfo(t *testing.T) {
	t.Parallel()

	full := assertedUserInfo(&configv1alpha3.CommitAuthor{Name: "Ada Lovelace", Email: "ada@example.com"})
	assert.Equal(t, git.UserInfo{Username: "Ada Lovelace", DisplayName: "Ada Lovelace", Email: "ada@example.com"}, full)

	// An omitted email is derived downstream, exactly as for an audit-attributed author
	// whose token carried no email claim.
	noEmail := assertedUserInfo(&configv1alpha3.CommitAuthor{Name: "Ada Lovelace"})
	assert.Empty(t, noEmail.Email)
	assert.Equal(t, "Ada Lovelace", noEmail.Username)
}
