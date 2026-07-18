// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authnv1 "k8s.io/api/authentication/v1"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
)

// TestEffectiveAuditUsername_PrefersImpersonatedUser pins whose name reaches Git. When a request is
// impersonated the ACTOR is the impersonated identity — that is who changed the cluster — while the
// authenticating account is only the one permitted to impersonate. Crediting the authenticating
// user would attribute every `kubectl --as` change to the operator or a CI robot.
func TestEffectiveAuditUsername_PrefersImpersonatedUser(t *testing.T) {
	tests := []struct {
		name  string
		event auditv1.Event
		want  string
	}{
		{
			name:  "plain request uses the authenticated user",
			event: auditv1.Event{User: authnv1.UserInfo{Username: "alice"}},
			want:  "alice",
		},
		{
			name: "impersonated request uses the impersonated user",
			event: auditv1.Event{
				User:             authnv1.UserInfo{Username: "admin"},
				ImpersonatedUser: &authnv1.UserInfo{Username: "alice"},
			},
			want: "alice",
		},
		{
			name: "an empty impersonated username falls back to the authenticated user",
			event: auditv1.Event{
				User:             authnv1.UserInfo{Username: "admin"},
				ImpersonatedUser: &authnv1.UserInfo{},
			},
			want: "admin",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, effectiveAuditUsername(tt.event))
		})
	}
}

// TestGVRParts_BoundedLabels keeps the metric label set small: an objectRef with no usable identity
// must collapse to fixed strings rather than emit unbounded cardinality.
func TestGVRParts_BoundedLabels(t *testing.T) {
	tests := []struct {
		name                     string
		event                    *auditv1.Event
		group, version, resource string
	}{
		{
			name:  "no objectRef",
			event: &auditv1.Event{},
			group: "unknown", version: "unknown", resource: "unknown",
		},
		{
			name:  "core group",
			event: &auditv1.Event{ObjectRef: &auditv1.ObjectReference{APIVersion: "v1", Resource: "configmaps"}},
			group: "", version: "v1", resource: "configmaps",
		},
		{
			name:  "grouped resource",
			event: &auditv1.Event{ObjectRef: &auditv1.ObjectReference{APIVersion: "apps/v1", Resource: "deployments"}},
			group: "apps", version: "v1", resource: "deployments",
		},
		{
			name:  "missing resource collapses to unknown",
			event: &auditv1.Event{ObjectRef: &auditv1.ObjectReference{APIVersion: "v1"}},
			group: "", version: "v1", resource: "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, v, r := gvrParts(tt.event)
			assert.Equal(t, tt.group, g)
			assert.Equal(t, tt.version, v)
			assert.Equal(t, tt.resource, r)
		})
	}
}

// TestExtractGVR_CoreGroupRendersLeadingSlash covers the log-field rendering for both group shapes.
func TestExtractGVR_CoreGroupRendersLeadingSlash(t *testing.T) {
	core := &auditv1.Event{ObjectRef: &auditv1.ObjectReference{APIVersion: "v1", Resource: "configmaps"}}
	assert.Equal(t, "/v1/configmaps", extractGVR(core))

	grouped := &auditv1.Event{ObjectRef: &auditv1.ObjectReference{APIVersion: "apps/v1", Resource: "deployments"}}
	assert.Equal(t, "apps/v1/deployments", extractGVR(grouped))
}

// TestAuditHandler_ImpersonatedEventIsRecordedUnderTheImpersonatedUser drives the whole ingress path
// for an impersonated mutation, so the identity rule above is proven end-to-end rather than only at
// the helper.
func TestAuditHandler_ImpersonatedEventIsRecordedUnderTheImpersonatedUser(t *testing.T) {
	recorder := &fakeFactRecorder{}
	handler, err := NewAuditHandler(routedConfig(AuditHandlerConfig{FactRecorder: recorder}))
	require.NoError(t, err)

	const impersonated = `{"kind":"Event","level":"RequestResponse","auditID":"imp-1",` +
		`"stage":"ResponseComplete","verb":"create","user":{"username":"admin"},` +
		`"impersonatedUser":{"username":"alice"},` +
		`"requestURI":"/api/v1/namespaces/default/configmaps",` +
		`"objectRef":{"resource":"configmaps","namespace":"default","name":"cm","apiVersion":"v1"},` +
		`"responseStatus":{"code":200},` +
		`"responseObject":{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm"}}}`

	w := serveBody(t, handler, http.MethodPost, defaultRoute, eventListBody(impersonated))
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 1, recorder.len())
	assert.Equal(t, "alice", effectiveAuditUsername(recorder.events[0]),
		"the impersonated actor is the author, not the account permitted to impersonate")
}
