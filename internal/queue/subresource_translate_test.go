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

// TestTranslateSubresource_RealScaleRecording feeds the actual recorded
// deployments/scale responseObject (the design's reference capture) through the
// translator and asserts it becomes exactly spec.replicas: 3.
func TestTranslateSubresource_RealScaleRecording(t *testing.T) {
	raw, err := os.ReadFile("../webhook/testdata/audit-events/deployment-scale-subresource.json")
	require.NoError(t, err)

	var recording map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &recording))
	require.Contains(t, recording, "responseObject")

	ev := auditv1.Event{
		ObjectRef:      &auditv1.ObjectReference{Resource: "deployments", Subresource: "scale"},
		ResponseObject: &runtime.Unknown{Raw: recording["responseObject"]},
	}

	assignments, ok := translateSubresourceToAssignments(ev)
	require.True(t, ok)
	require.Len(t, assignments, 1)
	assert.Equal(t, []string{"spec", "replicas"}, assignments[0].Path)
	assert.Equal(t, int64(3), assignments[0].Value)
}

func TestTranslateSubresource_PrefersResponseOverRequest(t *testing.T) {
	ev := scaleEvent(`{"spec":{"replicas":5}}`, `{"spec":{"replicas":3}}`)

	assignments, ok := translateSubresourceToAssignments(ev)
	require.True(t, ok)
	require.Len(t, assignments, 1)
	assert.Equal(t, int64(5), assignments[0].Value, "the post-mutation responseObject wins")
}

func TestTranslateSubresource_FallsBackToRequest(t *testing.T) {
	ev := scaleEvent("", `{"spec":{"replicas":3}}`)

	assignments, ok := translateSubresourceToAssignments(ev)
	require.True(t, ok)
	require.Len(t, assignments, 1)
	assert.Equal(t, int64(3), assignments[0].Value)
}

func TestTranslateSubresource_NeverReadsStatus(t *testing.T) {
	ev := scaleEvent(`{"spec":{"replicas":3},"status":{"replicas":0,"selector":"app=web"}}`, "")

	assignments, ok := translateSubresourceToAssignments(ev)
	require.True(t, ok)
	require.Len(t, assignments, 1, "status is never translated")
	assert.Equal(t, []string{"spec", "replicas"}, assignments[0].Path)
}

func TestTranslateSubresource_NoSpecIsDropped(t *testing.T) {
	ev := scaleEvent(`{"status":{"replicas":0}}`, "")

	_, ok := translateSubresourceToAssignments(ev)
	assert.False(t, ok, "a body with no spec is not translatable")
}

// A nested spec yields one assignment per leaf, in stable path order, so a CRD-shaped
// subresource with several fields is supported without per-subresource code.
func TestTranslateSubresource_NestedSpecYieldsLeafAssignments(t *testing.T) {
	ev := scaleEvent(`{"spec":{"size":3,"config":{"mode":"fast"}}}`, "")

	assignments, ok := translateSubresourceToAssignments(ev)
	require.True(t, ok)
	require.Len(t, assignments, 2)
	assert.Equal(t, []string{"spec", "config", "mode"}, assignments[0].Path)
	assert.Equal(t, "fast", assignments[0].Value)
	assert.Equal(t, []string{"spec", "size"}, assignments[1].Path)
	assert.Equal(t, int64(3), assignments[1].Value)
}
