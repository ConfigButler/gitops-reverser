// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authnv1 "k8s.io/api/authentication/v1"
)

// fakeAuthorizer stands in for the SubjectAccessReview client and records what it was asked.
type fakeAuthorizer struct {
	allowed bool
	reason  string
	err     error

	calls []struct {
		user          authnv1.UserInfo
		namespace     string
		gitTargetName string
	}
}

func (f *fakeAuthorizer) CanAssertAuthor(
	_ context.Context, user authnv1.UserInfo, namespace, gitTargetName string,
) (bool, string, error) {
	f.calls = append(f.calls, struct {
		user          authnv1.UserInfo
		namespace     string
		gitTargetName string
	}{user, namespace, gitTargetName})
	return f.allowed, f.reason, f.err
}

const commitRequestWithAuthor = `{
  "metadata": {"uid": "cr-uid"},
  "spec": {"targetRef": {"name": "tenants"}, "author": {"name": "Ada Lovelace", "email": "ada@example.com"}}
}`

const commitRequestWithoutAuthor = `{"metadata":{"uid":"cr-uid"},"spec":{"targetRef":{"name":"tenants"}}}`

func TestParseAssertedAuthor(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		raw      string
		wantOK   bool
		wantName string
		wantRef  string
	}{
		"asserts an author": {
			raw:      commitRequestWithAuthor,
			wantOK:   true,
			wantName: "Ada Lovelace",
			wantRef:  "tenants",
		},
		"no author":          {raw: commitRequestWithoutAuthor, wantOK: false},
		"empty author name":  {raw: `{"spec":{"author":{"email":"a@b.co"}}}`, wantOK: false},
		"null author":        {raw: `{"spec":{"author":null}}`, wantOK: false},
		"empty body":         {raw: "", wantOK: false},
		"malformed json":     {raw: "{not json", wantOK: false},
		"author but no name": {raw: `{"spec":{"author":{"name":""}}}`, wantOK: false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseAssertedAuthor([]byte(tc.raw))
			require.Equal(t, tc.wantOK, ok)
			if !tc.wantOK {
				return
			}
			assert.Equal(t, tc.wantName, got.Name)
			assert.Equal(t, tc.wantRef, got.GitTargetName)
		})
	}
}

func TestValidateOperatorTypesHandler_AllowsAuthorizedAssertion(t *testing.T) {
	rec := &fakeCommandAuthorRecorder{}
	authz := &fakeAuthorizer{allowed: true}
	h := &ValidateOperatorTypesHandler{Store: rec, Authorizer: authz}

	user := authnv1.UserInfo{Username: "gitops-api", Groups: []string{"platform"}}
	resp := h.Handle(context.Background(), commandReview(commitRequestResource(), commitRequestWithAuthor, user, nil))

	assert.True(t, resp.Allowed)
	require.Len(t, authz.calls, 1)
	assert.Equal(t, "gitops-api", authz.calls[0].user.Username)
	assert.Equal(t, "team-a", authz.calls[0].namespace, "the review is scoped to the CommitRequest's namespace")
	assert.Equal(t, "tenants", authz.calls[0].gitTargetName, "and to the GitTarget it names")

	require.Len(t, rec.calls, 1)
	assert.True(t, rec.calls[0].author.AssertAuthorAllowed,
		"the verdict must be recorded: the controller honors spec.author only against it")
}

func TestValidateOperatorTypesHandler_DeniesUnauthorizedAssertion(t *testing.T) {
	rec := &fakeCommandAuthorRecorder{}
	h := &ValidateOperatorTypesHandler{Store: rec, Authorizer: &fakeAuthorizer{allowed: false, reason: "no RBAC rule"}}

	resp := h.Handle(context.Background(),
		commandReview(commitRequestResource(), commitRequestWithAuthor, authnv1.UserInfo{Username: "mallory"}, nil))

	assert.False(t, resp.Allowed)
	require.NotNil(t, resp.Result)
	assert.Contains(t, resp.Result.Message, "assert-author")
	assert.Contains(t, resp.Result.Message, "tenants", "the denial names the GitTarget")
	assert.Contains(t, resp.Result.Message, "no RBAC rule", "and the authorizer's own reason")
	assert.Empty(t, rec.calls, "an unauthorized assertion records nothing")
}

// An unverifiable privilege is not a granted one: without an authorizer the handler cannot
// know whether the requester holds the verb, so it must not let the assertion through.
func TestValidateOperatorTypesHandler_DeniesAssertionWithoutAuthorizer(t *testing.T) {
	rec := &fakeCommandAuthorRecorder{}
	h := &ValidateOperatorTypesHandler{Store: rec}

	resp := h.Handle(context.Background(),
		commandReview(commitRequestResource(), commitRequestWithAuthor, authnv1.UserInfo{Username: "alice"}, nil))

	assert.False(t, resp.Allowed)
	assert.Contains(t, resp.Result.Message, "SubjectAccessReview")
	assert.Empty(t, rec.calls)
}

// An authorizer we could not reach has not said yes.
func TestValidateOperatorTypesHandler_DeniesAssertionWhenReviewErrors(t *testing.T) {
	rec := &fakeCommandAuthorRecorder{}
	h := &ValidateOperatorTypesHandler{Store: rec, Authorizer: &fakeAuthorizer{err: errors.New("apiserver down")}}

	resp := h.Handle(context.Background(),
		commandReview(commitRequestResource(), commitRequestWithAuthor, authnv1.UserInfo{Username: "alice"}, nil))

	assert.False(t, resp.Allowed)
	assert.Contains(t, resp.Result.Message, "apiserver down")
	assert.Empty(t, rec.calls)
}

// A dry-run apply must report the same forbidden error a real one would, so the guard runs
// before the dry-run short-circuit.
func TestValidateOperatorTypesHandler_DeniesUnauthorizedAssertionOnDryRun(t *testing.T) {
	rec := &fakeCommandAuthorRecorder{}
	h := &ValidateOperatorTypesHandler{Store: rec, Authorizer: &fakeAuthorizer{allowed: false}}
	dryRun := true

	resp := h.Handle(context.Background(),
		commandReview(commitRequestResource(), commitRequestWithAuthor, authnv1.UserInfo{Username: "mallory"}, &dryRun))

	assert.False(t, resp.Allowed)
	assert.Empty(t, rec.calls, "dry-run still records nothing")
}

func TestValidateOperatorTypesHandler_AuthorizedDryRunRecordsNothing(t *testing.T) {
	rec := &fakeCommandAuthorRecorder{}
	h := &ValidateOperatorTypesHandler{Store: rec, Authorizer: &fakeAuthorizer{allowed: true}}
	dryRun := true

	resp := h.Handle(context.Background(),
		commandReview(commitRequestResource(), commitRequestWithAuthor, authnv1.UserInfo{Username: "alice"}, &dryRun))

	assert.True(t, resp.Allowed)
	assert.Empty(t, rec.calls, "sideEffects: NoneOnDryRun")
}

// Without Redis the assertion is authorized but never recorded, so the controller ignores
// it. The create is still allowed — a missing store never blocks a command.
func TestValidateOperatorTypesHandler_AuthorizedAssertionWithoutStore(t *testing.T) {
	h := &ValidateOperatorTypesHandler{Authorizer: &fakeAuthorizer{allowed: true}}

	resp := h.Handle(context.Background(),
		commandReview(commitRequestResource(), commitRequestWithAuthor, authnv1.UserInfo{Username: "alice"}, nil))

	assert.True(t, resp.Allowed)
}

// The overwhelmingly common case: no spec.author, so no review is issued at all.
func TestValidateOperatorTypesHandler_NoAssertionSkipsAuthorization(t *testing.T) {
	rec := &fakeCommandAuthorRecorder{}
	authz := &fakeAuthorizer{allowed: false}
	h := &ValidateOperatorTypesHandler{Store: rec, Authorizer: authz}

	resp := h.Handle(context.Background(),
		commandReview(commitRequestResource(), commitRequestWithoutAuthor, authnv1.UserInfo{Username: "alice"}, nil))

	assert.True(t, resp.Allowed)
	assert.Empty(t, authz.calls, "a CommitRequest without spec.author needs no privilege")
	require.Len(t, rec.calls, 1)
	assert.False(t, rec.calls[0].author.AssertAuthorAllowed)
}

func TestExtraToSubjectAccessReviewExtra(t *testing.T) {
	t.Parallel()

	assert.Nil(t, extraToSubjectAccessReviewExtra(nil))
	got := extraToSubjectAccessReviewExtra(map[string]authnv1.ExtraValue{"claim": {"a", "b"}})
	require.Len(t, got, 1)
	assert.Equal(t, []string{"a", "b"}, []string(got["claim"]))
}
