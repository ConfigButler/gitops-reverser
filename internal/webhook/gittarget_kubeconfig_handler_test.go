// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// fakeSecretAuthorizer records the request it was asked to authorize and returns a canned
// verdict, standing in for the SubjectAccessReview client without an API server.
type fakeSecretAuthorizer struct {
	allowed       bool
	reason        string
	err           error
	gotUser       string
	gotNamespace  string
	gotSecretName string
	called        bool
}

func (f *fakeSecretAuthorizer) CanGetSecret(
	_ context.Context, user authnv1.UserInfo, namespace, name string,
) (bool, string, error) {
	f.called = true
	f.gotUser = user.Username
	f.gotNamespace = namespace
	f.gotSecretName = name
	return f.allowed, f.reason, f.err
}

func gitTargetReview(raw string, user authnv1.UserInfo) ctrladmission.Request {
	return ctrladmission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID: "review-uid",
			Resource: metav1.GroupVersionResource{
				Group:    "configbutler.ai",
				Version:  "v1alpha3",
				Resource: "gittargets",
			},
			Name:      "gt",
			Namespace: "team-a",
			Operation: admissionv1.Create,
			UserInfo:  user,
			Object:    runtime.RawExtension{Raw: []byte(raw)},
		},
	}
}

const gitTargetWithSecretRef = `{
  "apiVersion": "configbutler.ai/v1alpha3",
  "kind": "GitTarget",
  "metadata": {"name": "gt", "namespace": "team-a"},
  "spec": {"providerRef": {"name": "p"}, "branch": "main", "path": "clusters/x",
           "kubeConfig": {"secretRef": {"name": "acme-kubeconfig"}}}
}`

const gitTargetNoKubeConfig = `{
  "apiVersion": "configbutler.ai/v1alpha3",
  "kind": "GitTarget",
  "metadata": {"name": "gt", "namespace": "team-a"},
  "spec": {"providerRef": {"name": "p"}, "branch": "main", "path": "clusters/x"}
}`

var jane = authnv1.UserInfo{Username: "jane", Groups: []string{"tenants"}}

func TestGitTargetKubeConfig_AllowsWhenAuthorized(t *testing.T) {
	auth := &fakeSecretAuthorizer{allowed: true}
	h := &ValidateGitTargetKubeConfigHandler{Authorizer: auth}

	resp := h.Handle(context.Background(), gitTargetReview(gitTargetWithSecretRef, jane))
	assert.True(t, resp.Allowed)
	require.True(t, auth.called, "the authorizer must be consulted")
	assert.Equal(t, "jane", auth.gotUser)
	assert.Equal(t, "team-a", auth.gotNamespace)
	assert.Equal(t, "acme-kubeconfig", auth.gotSecretName)
}

func TestGitTargetKubeConfig_DeniesWhenUnauthorized(t *testing.T) {
	h := &ValidateGitTargetKubeConfigHandler{Authorizer: &fakeSecretAuthorizer{allowed: false, reason: "no such role"}}
	resp := h.Handle(context.Background(), gitTargetReview(gitTargetWithSecretRef, jane))
	assert.False(t, resp.Allowed)
	require.NotNil(t, resp.Result)
	assert.Contains(t, resp.Result.Message, "acme-kubeconfig")
	assert.Contains(t, resp.Result.Message, "jane")
}

func TestGitTargetKubeConfig_FailsClosedOnAuthorizerError(t *testing.T) {
	h := &ValidateGitTargetKubeConfigHandler{Authorizer: &fakeSecretAuthorizer{err: errors.New("apiserver down")}}
	resp := h.Handle(context.Background(), gitTargetReview(gitTargetWithSecretRef, jane))
	assert.False(t, resp.Allowed, "an authorizer error must fail closed")
	assert.Contains(t, resp.Result.Message, "fail-closed")
}

func TestGitTargetKubeConfig_FailsClosedWithoutAuthorizer(t *testing.T) {
	h := &ValidateGitTargetKubeConfigHandler{Authorizer: nil}
	resp := h.Handle(context.Background(), gitTargetReview(gitTargetWithSecretRef, jane))
	assert.False(t, resp.Allowed, "a nil authorizer must fail closed on any kubeConfig.secretRef")
}

func TestGitTargetKubeConfig_AllowsWhenNoKubeConfig(t *testing.T) {
	auth := &fakeSecretAuthorizer{}
	h := &ValidateGitTargetKubeConfigHandler{Authorizer: auth}
	resp := h.Handle(context.Background(), gitTargetReview(gitTargetNoKubeConfig, jane))
	assert.True(t, resp.Allowed)
	assert.False(t, auth.called, "no secretRef -> nothing to authorize")
}
