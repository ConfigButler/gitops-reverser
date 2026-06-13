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
// apiservice-audit-proxy (see its docs/examples). deletecollection is name-less:
// the per-type tail and the splice skip it, and the checkpoint sweep reconciles
// its removals (DEC-5), so its body is never consumed downstream — even a
// successful proxy-body merge emits a bodyless, objectRef-only event. A hollow
// kube-apiserver (Lane A) deletecollection therefore carries enough identity
// (resource + scope) to act on by itself, so it emits AS-IS whether or not the
// proxy (Lane B) body arrives. This removes the body-join race that previously
// shallow-dropped teardown deletecollections under namespace-deletion bursts (the
// proxy body raced the 0.5s officialBodyWait and sometimes lost).
//
// Two real proxy-body flavours still park harmlessly when they happen to arrive:
//
//   - "raw"      — `kubectl delete flunder --all`; a real FlunderList responseObject.
//   - "teardown" — namespace deletion; only a DeleteOptions requestObject.
//
// A bodyless ADDITIONAL (proxy) contribution remains useless and is discarded as
// Malformed; only an OFFICIAL hollow deletecollection is actionable on its objectRef.

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
			// Lane A, namespace teardown: hollow (objectRef, no body). It is emittable
			// as-is on its objectRef identity — a deletecollection's body is never
			// consumed — so it is Collection, not identity-shallow, and never waits for
			// or depends on a parked proxy body.
			name:    "official teardown (hollow) is collection on objectRef identity",
			fixture: deletecollectionOfficialTeardownFixture,
			source:  AuditSourceOfficial,
			want:    AuditEventQualityCollection,
		},
		{
			name:    "official raw (hollow) is collection on objectRef identity",
			fixture: deletecollectionOfficialRawFixture,
			source:  AuditSourceOfficial,
			want:    AuditEventQualityCollection,
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
// core namespace-deletion check: even when the proxy's DeleteOptions-only body has
// parked, the hollow official deletecollection emits AS-IS on its objectRef — the
// parked body is harmless and unused (a deletecollection's body is never merged or
// consumed). The official no longer depends on that body, so the join no longer races.
func TestRedisAuditEventJoiner_DeleteCollectionTeardownNotDroppedAsShallow(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoiner(t, mr)
	ctx := context.Background()

	proxy := loadAuditEventListItem(t, deletecollectionProxyTeardownFixture)
	parked, err := decide(ctx, t, joiner, AuditSourceAdditional, &proxy)
	require.NoError(t, err)
	require.Equal(t, AuditJoinActionParked, parked.Action, "proxy teardown body parks harmlessly")

	official := loadAuditEventListItem(t, deletecollectionOfficialTeardownFixture)
	require.Equal(t, proxy.AuditID, official.AuditID, "fixtures share an auditID")

	decision, err := decide(ctx, t, joiner, AuditSourceOfficial, &official)
	require.NoError(t, err)

	require.Equal(t, AuditJoinActionEmit, decision.Action, "the hollow deletecollection emits, not dropped as shallow")
	assert.Equal(t, AuditJoinResultAsIs, decision.Result, "emitted as-is; the parked DeleteOptions body is not merged")
	// The objectRef identifies the collection to remove (resource + namespace, no
	// name); the consumer acts on it and the checkpoint sweep does the removal.
	require.NotNil(t, decision.Event.ObjectRef)
	assert.Equal(t, "flunders", decision.Event.ObjectRef.Resource)
	assert.Equal(t, "example-teardown", decision.Event.ObjectRef.Namespace)
	assert.Empty(t, decision.Event.ObjectRef.Name)
	assert.Nil(t, decision.Event.RequestObject, "a deletecollection emits bodyless — no body is merged onto it")
	assert.Nil(t, decision.Event.ResponseObject)
}

// TestRedisAuditEventJoiner_DeleteCollectionRawNotDroppedAsShallow covers the
// `kubectl delete flunder --all` flavour, where the proxy supplies a real
// FlunderList responseObject. As with teardown, the parked body is unused: the
// hollow official emits as-is on its objectRef.
func TestRedisAuditEventJoiner_DeleteCollectionRawNotDroppedAsShallow(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoiner(t, mr)
	ctx := context.Background()

	proxy := loadAuditEventListItem(t, deletecollectionProxyRawFixture)
	parked, err := decide(ctx, t, joiner, AuditSourceAdditional, &proxy)
	require.NoError(t, err)
	require.Equal(t, AuditJoinActionParked, parked.Action, "proxy raw body parks harmlessly")

	official := loadAuditEventListItem(t, deletecollectionOfficialRawFixture)
	require.Equal(t, proxy.AuditID, official.AuditID, "fixtures share an auditID")

	decision, err := decide(ctx, t, joiner, AuditSourceOfficial, &official)
	require.NoError(t, err)

	require.Equal(t, AuditJoinActionEmit, decision.Action, "the hollow deletecollection emits, not dropped as shallow")
	assert.Equal(t, AuditJoinResultAsIs, decision.Result, "emitted as-is; the parked FlunderList body is not merged")
	require.NotNil(t, decision.Event.ObjectRef)
	assert.Equal(t, "flunders", decision.Event.ObjectRef.Resource)
	assert.Equal(t, "example-raw", decision.Event.ObjectRef.Namespace)
	assert.Empty(t, decision.Event.ObjectRef.Name)
}

// TestRedisAuditEventJoiner_HollowDeleteCollectionEmitsAsIsWithoutProxyBody is the core race fix:
// with NO proxy (Lane B) body parked, a hollow official deletecollection still emits as-is on its
// objectRef identity — it is NOT dropped as shallow and never waits for a body it does not consume
// (the checkpoint sweep removes the collection). This is exactly the case that raced and flaked the
// aggregated-api e2e under namespace-teardown bursts (the proxy body lost the 0.5s officialBodyWait).
func TestRedisAuditEventJoiner_HollowDeleteCollectionEmitsAsIsWithoutProxyBody(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoiner(t, mr)

	official := loadAuditEventListItem(t, deletecollectionOfficialTeardownFixture)
	decision, err := decide(context.Background(), t, joiner, AuditSourceOfficial, &official)
	require.NoError(t, err)

	assert.Equal(t, AuditJoinActionEmit, decision.Action,
		"a hollow deletecollection emits as-is on its objectRef identity, not dropped as shallow")
	assert.Equal(t, AuditJoinResultAsIs, decision.Result, "emitted as-is — no body merged or required")
	require.NotNil(t, decision.Event.ObjectRef)
	assert.Equal(t, "flunders", decision.Event.ObjectRef.Resource)
	assert.Empty(t, decision.Event.ObjectRef.Name, "name-less; the consumer acts on the objectRef")
	assert.Nil(t, decision.Event.RequestObject)
	assert.Nil(t, decision.Event.ResponseObject)
	assert.False(t, mr.Exists(bodyKey(string(official.AuditID))), "emit-as-is parks nothing")
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
