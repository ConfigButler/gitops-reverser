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

package reconcile

import (
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// TestGitTargetEventStream_MultipleStreamsWithSharedWorker tests that multiple
// GitTargetEventStream instances can share a single BranchWorker without interference:
// each stamps its own identity and all events converge at the shared worker.
func TestGitTargetEventStream_MultipleStreamsWithSharedWorker(t *testing.T) {
	mockWorker := &mockEventEnqueuer{events: make([]git.Event, 0)}
	logger := logr.Discard()

	stream1 := NewGitTargetEventStream("target1", "default", mockWorker, logger)
	stream2 := NewGitTargetEventStream("target2", "default", mockWorker, logger)

	require.NoError(t, stream1.OnWatchEvent(createTestEventWithPath("pod", "app-pod", "CREATE", "apps")))
	require.NoError(t, stream2.OnWatchEvent(createTestEventWithPath("deployment", "nginx", "CREATE", "infra")))

	assert.Len(t, mockWorker.events, 2, "Both events should reach shared worker")

	foundApps := false
	foundInfra := false
	for _, evt := range mockWorker.events {
		if evt.Path == "apps" && evt.Identifier.Name == "app-pod" {
			foundApps = true
		}
		if evt.Path == "infra" && evt.Identifier.Name == "nginx" {
			foundInfra = true
		}
	}
	assert.True(t, foundApps, "Event with 'apps' path should be present")
	assert.True(t, foundInfra, "Event with 'infra' path should be present")
}

// TestGitTargetEventStream_DuplicateEventsAcrossStreams tests that the same cluster
// resource observed by multiple streams produces separate events for each stream
// (intentional for multi-target scenarios).
func TestGitTargetEventStream_DuplicateEventsAcrossStreams(t *testing.T) {
	mockWorker := &mockEventEnqueuer{events: make([]git.Event, 0)}
	logger := logr.Discard()

	streamClusterA := NewGitTargetEventStream("cluster-a-target", "default", mockWorker, logger)
	streamClusterB := NewGitTargetEventStream("cluster-b-target", "default", mockWorker, logger)

	require.NoError(
		t,
		streamClusterA.OnWatchEvent(createTestEventWithPath("configmap", "shared-config", "UPDATE", "cluster-a")),
	)
	require.NoError(
		t,
		streamClusterB.OnWatchEvent(createTestEventWithPath("configmap", "shared-config", "UPDATE", "cluster-b")),
	)

	assert.Len(t, mockWorker.events, 2, "Both duplicate events should be enqueued")

	foundClusterA := false
	foundClusterB := false
	for _, evt := range mockWorker.events {
		if evt.Path == "cluster-a" && evt.Identifier.Name == "shared-config" {
			foundClusterA = true
		}
		if evt.Path == "cluster-b" && evt.Identifier.Name == "shared-config" {
			foundClusterB = true
		}
	}
	assert.True(t, foundClusterA, "Event for 'cluster-a' should be present")
	assert.True(t, foundClusterB, "Event for 'cluster-b' should be present")
}

// TestGitTargetEventStream_StreamDeletion tests that a stream that stops sending events
// does not affect the others sharing the worker.
func TestGitTargetEventStream_StreamDeletion(t *testing.T) {
	mockWorker := &mockEventEnqueuer{events: make([]git.Event, 0)}
	logger := logr.Discard()

	stream1 := NewGitTargetEventStream("target1", "default", mockWorker, logger)
	stream2 := NewGitTargetEventStream("target2", "default", mockWorker, logger)

	require.NoError(t, stream1.OnWatchEvent(createTestEventWithPath("pod", "app-pod", "CREATE", "apps")))
	require.NoError(t, stream2.OnWatchEvent(createTestEventWithPath("deployment", "infra-deploy", "CREATE", "infra")))

	initialEventCount := len(mockWorker.events)
	assert.Equal(t, 2, initialEventCount, "Both initial events should be enqueued")

	// stream1 is "deleted" (no longer used); stream2 keeps sending.
	require.NoError(t, stream2.OnWatchEvent(createTestEventWithPath("service", "infra-svc", "CREATE", "infra")))

	assert.Greater(t, len(mockWorker.events), initialEventCount, "Worker should continue processing events")

	foundInfraService := false
	for i := initialEventCount; i < len(mockWorker.events); i++ {
		evt := mockWorker.events[i]
		if evt.Path == "infra" && evt.Identifier.Name == "infra-svc" {
			foundInfraService = true
		}
	}
	assert.True(t, foundInfraService, "Event for remaining stream should be processed")
}

// TestGitTargetEventStream_EventConvergence tests that events from multiple streams
// converge at a shared worker for batched commit processing.
func TestGitTargetEventStream_EventConvergence(t *testing.T) {
	mockWorker := &mockEventEnqueuer{events: make([]git.Event, 0)}
	logger := logr.Discard()

	streamTeamA := NewGitTargetEventStream("team-a-target", "default", mockWorker, logger)
	streamTeamB := NewGitTargetEventStream("team-b-target", "default", mockWorker, logger)
	streamTeamC := NewGitTargetEventStream("team-c-target", "default", mockWorker, logger)

	var wg sync.WaitGroup
	streams := []*GitTargetEventStream{streamTeamA, streamTeamB, streamTeamC}
	paths := []string{"team-a", "team-b", "team-c"}

	for i, stream := range streams {
		wg.Add(1)
		go func(idx int, s *GitTargetEventStream, path string) {
			defer wg.Done()
			// assert (not require) inside a goroutine: require's FailNow must run on the
			// test goroutine only (testifylint go-require).
			assert.NoError(
				t,
				s.OnWatchEvent(createTestEventWithPath("pod", "pod-"+string(rune('a'+idx)), "CREATE", path)),
			)
		}(i, stream, paths[i])
	}

	wg.Wait()

	assert.GreaterOrEqual(t, len(mockWorker.events), 3, "All events should converge at shared worker")

	pathsFound := make(map[string]bool)
	for _, evt := range mockWorker.events {
		pathsFound[evt.Path] = true
	}
	assert.True(t, pathsFound["team-a"], "Events from team-a should converge")
	assert.True(t, pathsFound["team-b"], "Events from team-b should converge")
	assert.True(t, pathsFound["team-c"], "Events from team-c should converge")
}

// TestGitTargetEventStream_ForwardsWithoutDeduplication confirms the R3 pivot dropped
// content-hash dedup: the stream forwards every call, leaving "did it change?" to the
// writer's no-op detection at the commit boundary.
func TestGitTargetEventStream_ForwardsWithoutDeduplication(t *testing.T) {
	mockWorker := &mockEventEnqueuer{events: make([]git.Event, 0)}
	stream := NewGitTargetEventStream("target1", "default", mockWorker, logr.Discard())

	require.NoError(t, stream.OnWatchEvent(createTestEventWithPath("pod", "test-pod", "UPDATE", "apps")))
	require.NoError(t, stream.OnWatchEvent(createTestEventWithPath("pod", "test-pod", "UPDATE", "apps"))) // identical

	assert.Len(t, mockWorker.events, 2, "every call is forwarded; no content-hash dedup remains")
}

// mockEventEnqueuer implements EventEnqueuer interface for testing.
type mockEventEnqueuer struct {
	mu     sync.Mutex
	events []git.Event
}

func (m *mockEventEnqueuer) Enqueue(event git.Event) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, event)
	return true
}

// createTestEventWithPath creates a test event with a specific path.
func createTestEventWithPath(resourceType, name, operation, path string) git.Event {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("v1")
	obj.SetKind(resourceType)
	obj.SetName(name)
	obj.SetNamespace("default")

	identifier := types.ResourceIdentifier{
		Group:     "",
		Version:   "v1",
		Resource:  resourceType + "s", // Plural form
		Name:      name,
		Namespace: "default",
	}

	return git.Event{
		Object:     obj,
		Identifier: identifier,
		Operation:  operation,
		UserInfo:   git.UserInfo{Username: "test-user", UID: "test-uid"},
		Path:       path,
	}
}
