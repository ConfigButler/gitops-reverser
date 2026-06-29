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
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/ConfigButler/gitops-reverser/internal/queue"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// fakeLookup returns fact/ok after `hitAfter` calls; calls counts invocations and
// lastExactCapable records the event-kind flag of the most recent lookup.
type fakeLookup struct {
	resolution       queue.AuthorResolution
	hitAfter         int
	calls            int
	lastExactCapable bool
}

func (f *fakeLookup) LookupAuthorResolution(
	_ context.Context, _ schema.GroupVersionResource, _ k8stypes.UID, _ string, exactCapable bool,
) queue.AuthorResolution {
	f.calls++
	f.lastExactCapable = exactCapable
	if f.calls >= f.hitAfter {
		return f.resolution
	}
	return queue.AuthorResolution{Result: queue.AttributionAbsent}
}

var resolverGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

func TestAuthorResolver_HumanHit(t *testing.T) {
	lookup := &fakeLookup{
		resolution: queue.AuthorResolution{
			Fact:   queue.AuthorFact{Author: "alice", Email: "a@x.io"},
			Result: queue.AttributionExactUser,
		},
		hitAfter: 1,
	}
	r := NewAuthorResolver(lookup, DefaultAttributionGraceWindow, logr.Discard())

	ui, ok := r.ResolveAuthor(context.Background(), resolverGVR, "uid-1", "101", true)
	require.True(t, ok)
	assert.Equal(t, "alice", ui.Username)
	assert.Equal(t, "a@x.io", ui.Email)
	assert.Equal(t, 1, lookup.calls)
	assert.True(t, lookup.lastExactCapable, "an ADDED/MODIFIED event is exact-capable")
}

func TestAuthorResolver_ServiceAccountIsNamed(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	// A matched service account is always named by its own username — never collapsed
	// to the committer — and the resolution is recorded as exact_serviceaccount.
	sa := "system:serviceaccount:flux-system:kustomize-controller"
	lookup := &fakeLookup{
		resolution: queue.AuthorResolution{
			Fact:   queue.AuthorFact{Author: sa, IsServiceAccount: true},
			Result: queue.AttributionExactServiceAccount,
		},
		hitAfter: 1,
	}
	r := NewAuthorResolver(lookup, DefaultAttributionGraceWindow, logr.Discard())

	ui, ok := r.ResolveAuthor(context.Background(), resolverGVR, "uid-1", "101", true)
	require.True(t, ok, "a matched service account is named, not collapsed to the committer")
	assert.Equal(t, sa, ui.Username)

	count, ok := telemetry.CollectInt64Sum(reader, "gitopsreverser_attribution_resolutions_total",
		map[string]string{"result": string(queue.AttributionExactServiceAccount)})
	require.True(t, ok)
	assert.Equal(t, int64(1), count)

	waitCount, ok := telemetry.CollectHistogramCount(reader, "gitopsreverser_attribution_resolution_wait_seconds",
		map[string]string{"result": string(queue.AttributionExactServiceAccount)})
	require.True(t, ok)
	assert.Equal(t, uint64(1), waitCount)
}

func TestAuthorResolver_MissExpiresToCommitter(t *testing.T) {
	lookup := &fakeLookup{resolution: queue.AuthorResolution{Result: queue.AttributionAbsent}, hitAfter: 1000}
	r := NewAuthorResolver(lookup, 0, logr.Discard())

	// A zero grace does a single lookup and, on a miss, ships as committer (ok=false).
	// There is no longer a miss-marker write-back.
	_, ok := r.ResolveAuthor(context.Background(), resolverGVR, "uid-1", "101", true)
	assert.False(t, ok)
	assert.Equal(t, 1, lookup.calls)
}

func TestAuthorResolver_DeleteEventIsNotExactCapable(t *testing.T) {
	lookup := &fakeLookup{
		resolution: queue.AuthorResolution{
			Fact:   queue.AuthorFact{Author: "alice"},
			Result: queue.AttributionWeak,
		},
		hitAfter: 1,
	}
	r := NewAuthorResolver(lookup, DefaultAttributionGraceWindow, logr.Discard())

	_, ok := r.ResolveAuthor(context.Background(), resolverGVR, "uid-1", "999", false)
	require.True(t, ok)
	assert.False(t, lookup.lastExactCapable, "a removal event may consult the /last pointer")
}

func TestAuthorResolver_WaitsThroughGraceWindowForLateFact(t *testing.T) {
	lookup := &fakeLookup{
		resolution: queue.AuthorResolution{
			Fact:   queue.AuthorFact{Author: "bob"},
			Result: queue.AttributionExactUser,
		},
		hitAfter: 3,
	}
	r := NewAuthorResolver(lookup, 2*time.Second, logr.Discard())

	ui, ok := r.ResolveAuthor(context.Background(), resolverGVR, "uid-1", "101", true)
	require.True(t, ok)
	assert.Equal(t, "bob", ui.Username)
	assert.GreaterOrEqual(t, lookup.calls, 3)
}

func TestAuthorResolver_NilLookupIsCommitter(t *testing.T) {
	r := NewAuthorResolver(nil, DefaultAttributionGraceWindow, logr.Discard())
	_, ok := r.ResolveAuthor(context.Background(), resolverGVR, "uid-1", "101", true)
	assert.False(t, ok)
}
