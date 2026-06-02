/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package webhook

// These tests pin the shallow-detector and joiner behaviour for aggregated-API
// `deletecollection`, using exact copies of real audit events captured by
// apiservice-audit-proxy (see its docs/examples). deletecollection is the
// awkward verb: the kube-apiserver's own (Lane A) event for an aggregated
// resource is hollow — it has the objectRef but no request/response body — and
// the only body comes from the proxy (Lane B). Two flavours matter:
//
//   - "raw"      — `kubectl delete flunder --all`; the proxy captures a real
//                  FlunderList responseObject.
//   - "teardown" — namespace deletion; the namespace-controller issues the
//                  deletecollection and the proxy only ever sees a DeleteOptions
//                  requestObject (no list body at all).
//
// The teardown flavour is the trap: its only proxy body is DeleteOptions. These
// tests assert that such an event is still NOT dropped as shallow (the proxy
// body is enough to take the merge/emit path), while a deletecollection with no
// proxy body at all IS correctly dropped.

import (
	"context"
	"os"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	"sigs.k8s.io/yaml"
)

const (
	deletecollectionOfficialTeardownFixture = "testdata/audit-events/audit-deletecollection-official-teardown-hollow.json"
	deletecollectionOfficialRawFixture      = "testdata/audit-events/audit-deletecollection-official-raw-hollow.json"
	deletecollectionProxyTeardownFixture    = "testdata/audit-events/audit-deletecollection-proxy-teardown-deleteoptions.json"
	deletecollectionProxyRawFixture         = "testdata/audit-events/audit-deletecollection-proxy-raw-listbody.json"
)

// loadAuditEventListItem reads an audit.k8s.io/v1 EventList fixture and returns
// its single item. yaml.Unmarshal handles JSON input and populates the
// runtime.Unknown Raw bodies the same way the live decoder does.
func loadAuditEventListItem(t *testing.T, path string) auditv1.Event {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	var list auditv1.EventList
	require.NoError(t, yaml.Unmarshal(raw, &list))
	require.Len(t, list.Items, 1, "fixture %s must contain exactly one audit event", path)
	return list.Items[0]
}

// TestClassifyAuditEventQuality_DeleteCollectionFixtures documents how the
// shallow detector classifies each real deletecollection event in isolation.
func TestClassifyAuditEventQuality_DeleteCollectionFixtures(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		source  AuditSource
		want    AuditEventQuality
	}{
		{
			// Lane A, namespace teardown: hollow. Alone it has no body, so it is
			// correctly identity-shallow — it must rely on a parked proxy body.
			name:    "official teardown (hollow) is identity-shallow",
			fixture: deletecollectionOfficialTeardownFixture,
			source:  AuditSourceOfficial,
			want:    AuditEventQualityIdentityShallow,
		},
		{
			name:    "official raw (hollow) is identity-shallow",
			fixture: deletecollectionOfficialRawFixture,
			source:  AuditSourceOfficial,
			want:    AuditEventQualityIdentityShallow,
		},
		{
			// Lane B, namespace teardown: only a DeleteOptions requestObject. It
			// must still register as a real body (Collection), not Malformed —
			// otherwise the proxy contribution is discarded and the official
			// event has nothing to merge and gets dropped.
			name:    "proxy teardown (DeleteOptions body) is collection, not malformed",
			fixture: deletecollectionProxyTeardownFixture,
			source:  AuditSourceAdditional,
			want:    AuditEventQualityCollection,
		},
		{
			// Lane B, --all: a real FlunderList responseObject.
			name:    "proxy raw (FlunderList body) is collection",
			fixture: deletecollectionProxyRawFixture,
			source:  AuditSourceAdditional,
			want:    AuditEventQualityCollection,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := loadAuditEventListItem(t, tt.fixture)
			assert.Equal(t, tt.want, classifyAuditEventQuality(tt.source, &event))
		})
	}
}

// TestRedisAuditEventJoiner_DeleteCollectionTeardownNotDroppedAsShallow is the
// core namespace-deletion check: the hollow official deletecollection plus the
// proxy's DeleteOptions-only body must be emitted, not dropped as shallow.
func TestRedisAuditEventJoiner_DeleteCollectionTeardownNotDroppedAsShallow(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoiner(t, mr)
	ctx := context.Background()

	proxy := loadAuditEventListItem(t, deletecollectionProxyTeardownFixture)
	parked, err := decide(ctx, t, joiner, AuditSourceAdditional, &proxy)
	require.NoError(t, err)
	require.Equal(t, AuditJoinActionParked, parked.Action, "proxy teardown body must park")

	official := loadAuditEventListItem(t, deletecollectionOfficialTeardownFixture)
	require.Equal(t, proxy.AuditID, official.AuditID, "fixtures must share an auditID to join")

	decision, err := decide(ctx, t, joiner, AuditSourceOfficial, &official)
	require.NoError(t, err)

	require.Equal(t, AuditJoinActionEmit, decision.Action,
		"a deletecollection with a parked proxy body must be emitted, not dropped as shallow")
	// The objectRef identifies the collection to remove (resource + namespace,
	// no name). The DeleteOptions body is intentionally not merged onto a
	// deletecollection, so the emitted event stays bodyless and the consumer
	// acts on the objectRef.
	require.NotNil(t, decision.Event.ObjectRef)
	assert.Equal(t, "flunders", decision.Event.ObjectRef.Resource)
	assert.Equal(t, "example-teardown", decision.Event.ObjectRef.Namespace)
	assert.Empty(t, decision.Event.ObjectRef.Name)
	assert.Nil(t, decision.Event.RequestObject, "DeleteOptions must not be merged onto a deletecollection")
	assert.Nil(t, decision.Event.ResponseObject)
}

// TestRedisAuditEventJoiner_DeleteCollectionRawNotDroppedAsShallow covers the
// `kubectl delete flunder --all` flavour, where the proxy supplies a real
// FlunderList responseObject.
func TestRedisAuditEventJoiner_DeleteCollectionRawNotDroppedAsShallow(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoiner(t, mr)
	ctx := context.Background()

	proxy := loadAuditEventListItem(t, deletecollectionProxyRawFixture)
	parked, err := decide(ctx, t, joiner, AuditSourceAdditional, &proxy)
	require.NoError(t, err)
	require.Equal(t, AuditJoinActionParked, parked.Action, "proxy raw body must park")

	official := loadAuditEventListItem(t, deletecollectionOfficialRawFixture)
	require.Equal(t, proxy.AuditID, official.AuditID, "fixtures must share an auditID to join")

	decision, err := decide(ctx, t, joiner, AuditSourceOfficial, &official)
	require.NoError(t, err)

	require.Equal(t, AuditJoinActionEmit, decision.Action,
		"a deletecollection with a parked proxy body must be emitted, not dropped as shallow")
	require.NotNil(t, decision.Event.ObjectRef)
	assert.Equal(t, "flunders", decision.Event.ObjectRef.Resource)
	assert.Equal(t, "example-raw", decision.Event.ObjectRef.Namespace)
	assert.Empty(t, decision.Event.ObjectRef.Name)
}

// TestRedisAuditEventJoiner_DeleteCollectionWithoutProxyBodyIsShallow is the
// negative control: with no proxy (Lane B) body parked, the hollow official
// deletecollection has nothing to act on and is correctly dropped as shallow.
// This is the pre-0.6.0 / proxy-missing behaviour and the boundary the proxy
// fix moves the teardown case across.
func TestRedisAuditEventJoiner_DeleteCollectionWithoutProxyBodyIsShallow(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoiner(t, mr)

	official := loadAuditEventListItem(t, deletecollectionOfficialTeardownFixture)
	decision, err := decide(context.Background(), t, joiner, AuditSourceOfficial, &official)
	require.NoError(t, err)

	assert.Equal(t, AuditJoinActionDrop, decision.Action,
		"a hollow deletecollection with no proxy body must be dropped as shallow")
	assert.False(t, mr.Exists(bodyKey(string(official.AuditID))), "shallow official must not park")
}

// TestClassifyAuditEventQuality_DeleteCollectionBodyShapeIndependent guards the
// fact that the responseObject a backend returns for a deletecollection is
// aggregated-API-specific: wardle returns a FlunderList, but another apiserver
// may return a metav1.Status, an empty object, or only echo a DeleteOptions
// requestObject. The detector keys off "is any body present" (hasAuditV1ObjectBody)
// and the objectRef, never the body shape — so every non-empty variant must
// classify as Collection, and only a truly bodyless event is shallow. The base
// event is the real hollow official teardown fixture with its body swapped, so
// these stay synthetic-shape variants of a real event rather than invented data.
func TestClassifyAuditEventQuality_DeleteCollectionBodyShapeIndependent(t *testing.T) {
	statusBody := `{"kind":"Status","apiVersion":"v1","status":"Success","code":200}`
	listBody := `{"kind":"FlunderList","apiVersion":"wardle.example.com/v1alpha1","items":[]}`
	deleteOptionsBody := `{"kind":"DeleteOptions","apiVersion":"v1","propagationPolicy":"Background"}`

	tests := []struct {
		name        string
		requestRaw  string
		responseRaw string
		want        AuditEventQuality
	}{
		{name: "metav1.Status response body", responseRaw: statusBody, want: AuditEventQualityCollection},
		{name: "List response body", responseRaw: listBody, want: AuditEventQualityCollection},
		{name: "empty-object response body", responseRaw: `{}`, want: AuditEventQualityCollection},
		{name: "DeleteOptions request body only", requestRaw: deleteOptionsBody, want: AuditEventQualityCollection},
		// A bodyless proxy (additional-source) contribution is useless and is
		// discarded as Malformed before it can park, so the hollow official has
		// nothing to merge and is later dropped as shallow. Either way: not
		// actionable. (For the official source the same empty event is
		// IdentityShallow; the label differs, the "dropped" outcome does not.)
		{name: "no body at all is not actionable", want: AuditEventQualityMalformed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Start from a real hollow official deletecollection (objectRef set,
			// no body) and attach only the body shape under test.
			event := loadAuditEventListItem(t, deletecollectionOfficialTeardownFixture)
			require.Nil(t, event.RequestObject)
			require.Nil(t, event.ResponseObject)
			if tt.requestRaw != "" {
				event.RequestObject = &runtime.Unknown{Raw: []byte(tt.requestRaw)}
			}
			if tt.responseRaw != "" {
				event.ResponseObject = &runtime.Unknown{Raw: []byte(tt.responseRaw)}
			}

			assert.Equal(t, tt.want, classifyAuditEventQuality(AuditSourceAdditional, &event))
		})
	}
}
