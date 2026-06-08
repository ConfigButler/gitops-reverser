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

package queue

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
)

// scaleEvent builds a deployments/scale audit event with the given response and request
// bodies. The parent GVR (apps/deployments) is supplied by the caller to
// translateScaleToAssignments; the objectRef here only needs to carry the subresource.
func scaleEvent(responseBody, requestBody string) auditv1.Event {
	ev := auditv1.Event{
		ObjectRef: &auditv1.ObjectReference{Resource: "deployments", Subresource: "scale"},
	}
	if responseBody != "" {
		ev.ResponseObject = &runtime.Unknown{Raw: []byte(responseBody)}
	}
	if requestBody != "" {
		ev.RequestObject = &runtime.Unknown{Raw: []byte(requestBody)}
	}
	return ev
}

// TestTranslateScale_RealScaleRecording feeds the actual recorded deployments/scale
// responseObject (the design's reference capture) through the translator and asserts it
// becomes exactly spec.replicas: 3.
func TestTranslateScale_RealScaleRecording(t *testing.T) {
	raw, err := os.ReadFile("../webhook/testdata/audit-events/deployment-scale-subresource.json")
	require.NoError(t, err)

	var recording map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &recording))
	require.Contains(t, recording, "responseObject")

	ev := auditv1.Event{
		ObjectRef:      &auditv1.ObjectReference{Resource: "deployments", Subresource: "scale"},
		ResponseObject: &runtime.Unknown{Raw: recording["responseObject"]},
	}

	assignments, dropOutcome, ok := translateScaleToAssignments(ev, "apps", "deployments")
	require.True(t, ok)
	assert.Empty(t, dropOutcome)
	require.Len(t, assignments, 1)
	assert.Equal(t, []string{"spec", "replicas"}, assignments[0].Path)
	assert.Equal(t, int64(3), assignments[0].Value)
}

func TestTranslateScale_UsesResponseIgnoresRequest(t *testing.T) {
	ev := scaleEvent(`{"spec":{"replicas":5}}`, `{"spec":{"replicas":3}}`)

	assignments, _, ok := translateScaleToAssignments(ev, "apps", "deployments")
	require.True(t, ok)
	require.Len(t, assignments, 1)
	assert.Equal(t, int64(5), assignments[0].Value, "only the post-mutation responseObject is read")
}

// A scale event carrying only a requestObject is dropped: a request body is
// pre-admission intent, not confirmed accepted state, so there is no fallback to it.
func TestTranslateScale_RequestOnlyIsDropped(t *testing.T) {
	ev := scaleEvent("", `{"spec":{"replicas":3}}`)

	_, dropOutcome, ok := translateScaleToAssignments(ev, "apps", "deployments")
	assert.False(t, ok, "a request-only scale event must not be translated")
	assert.Equal(t, pipelineOutcomeDroppedScaleMissingReplicas, dropOutcome)
}

// Only spec.replicas is read; status (e.g. a Scale's status.replicas) never enters the
// patch.
func TestTranslateScale_NeverReadsStatus(t *testing.T) {
	ev := scaleEvent(`{"spec":{"replicas":3},"status":{"replicas":0,"selector":"app=web"}}`, "")

	assignments, _, ok := translateScaleToAssignments(ev, "apps", "deployments")
	require.True(t, ok)
	require.Len(t, assignments, 1, "status is never translated")
	assert.Equal(t, []string{"spec", "replicas"}, assignments[0].Path)
}

// A scale response with no spec.replicas is dropped with the missing-replicas outcome.
func TestTranslateScale_NoReplicasIsDropped(t *testing.T) {
	ev := scaleEvent(`{"status":{"replicas":0}}`, "")

	_, dropOutcome, ok := translateScaleToAssignments(ev, "apps", "deployments")
	assert.False(t, ok, "a body with no spec.replicas is not translatable")
	assert.Equal(t, pipelineOutcomeDroppedScaleMissingReplicas, dropOutcome)
}

// A non-scale subresource is dropped before any replica lookup: scale is the only
// supported subresource.
func TestTranslateScale_NonScaleSubresourceIsDropped(t *testing.T) {
	ev := auditv1.Event{
		ObjectRef:      &auditv1.ObjectReference{Resource: "deployments", Subresource: "status"},
		ResponseObject: &runtime.Unknown{Raw: []byte(`{"spec":{"replicas":3}}`)},
	}

	_, dropOutcome, ok := translateScaleToAssignments(ev, "apps", "deployments")
	assert.False(t, ok, "only the scale subresource is supported")
	assert.Equal(t, pipelineOutcomeDroppedNonScale, dropOutcome)
}

// A scale on a CRD — whose parent has no known replica path — is dropped as
// path-unresolved rather than defaulting to .spec.replicas, even when the body carries
// spec.replicas.
func TestTranslateScale_CRDParentPathUnresolved(t *testing.T) {
	ev := auditv1.Event{
		ObjectRef:      &auditv1.ObjectReference{Resource: "widgets", Subresource: "scale"},
		ResponseObject: &runtime.Unknown{Raw: []byte(`{"spec":{"replicas":3}}`)},
	}

	_, dropOutcome, ok := translateScaleToAssignments(ev, "example.com", "widgets")
	assert.False(t, ok, "a CRD scale has no known parent replica path")
	assert.Equal(t, pipelineOutcomeDroppedScalePathUnresolved, dropOutcome)
}

// An aggregated API scale falls through the same unresolved-path drop as any other
// non-built-in resource.
func TestTranslateScale_AggregatedAPIPathUnresolved(t *testing.T) {
	ev := auditv1.Event{
		ObjectRef:      &auditv1.ObjectReference{Resource: "things", Subresource: "scale"},
		ResponseObject: &runtime.Unknown{Raw: []byte(`{"spec":{"replicas":2}}`)},
	}

	_, dropOutcome, ok := translateScaleToAssignments(ev, "metrics.k8s.io", "things")
	assert.False(t, ok, "an aggregated API scale has no known parent replica path")
	assert.Equal(t, pipelineOutcomeDroppedScalePathUnresolved, dropOutcome)
}

// A StatefulSet scale routes to spec.replicas too — every built-in scalable parent
// shares the path.
func TestTranslateScale_StatefulSetRoutes(t *testing.T) {
	ev := auditv1.Event{
		ObjectRef:      &auditv1.ObjectReference{Resource: "statefulsets", Subresource: "scale"},
		ResponseObject: &runtime.Unknown{Raw: []byte(`{"spec":{"replicas":7}}`)},
	}

	assignments, _, ok := translateScaleToAssignments(ev, "apps", "statefulsets")
	require.True(t, ok)
	require.Len(t, assignments, 1)
	assert.Equal(t, []string{"spec", "replicas"}, assignments[0].Path)
	assert.Equal(t, int64(7), assignments[0].Value)
}
