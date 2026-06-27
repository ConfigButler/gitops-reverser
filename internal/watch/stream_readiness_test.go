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

package watch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestStreamSummaryForTypes_AggregatesByGVR(t *testing.T) {
	configmaps := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	secrets := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	expected := []targetWatchKey{
		{GVR: configmaps, Namespace: "a"},
		{GVR: configmaps, Namespace: "b"},
		{GVR: secrets, Namespace: "a"},
	}
	states := map[targetWatchKey]targetStreamStatus{
		{GVR: configmaps, Namespace: "a"}: {state: StreamStateStreaming},
		{GVR: configmaps, Namespace: "b"}: {state: StreamStateReplaying, reason: StreamReasonInitialReplay},
		{GVR: secrets, Namespace: "a"}:    {state: StreamStateStreaming},
	}

	summary := streamSummaryForTypes(expected, states, nil)

	assert.Equal(t, 2, summary.Total)
	assert.Equal(t, 1, summary.Ready)
	assert.Equal(t, 1, summary.Replaying)
	assert.Equal(t, StreamReasonReplaying, summary.Reason)
	assert.Equal(t, []string{"configmaps"}, summary.PendingSample)
}

func TestStreamSummaryForTypes_BlockedOutranksReplaying(t *testing.T) {
	configmaps := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	secrets := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	expected := []targetWatchKey{{GVR: configmaps}, {GVR: secrets}}
	states := map[targetWatchKey]targetStreamStatus{
		{GVR: configmaps}: {state: StreamStateReplaying, reason: StreamReasonInitialReplay},
		{GVR: secrets}:    {state: StreamStateBlocked, reason: StreamReasonWatchError},
	}

	summary := streamSummaryForTypes(expected, states, nil)

	assert.Equal(t, 2, summary.Total)
	assert.Equal(t, 1, summary.Blocked)
	assert.Equal(t, 1, summary.Replaying)
	assert.Equal(t, StreamReasonWatchError, summary.Reason)
	assert.False(t, summary.StreamsRunning())
}
