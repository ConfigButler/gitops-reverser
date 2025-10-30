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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// TestGitDestinationEventStream_MultipleStreamsWithSharedWorker tests that multiple
// GitDestinationEventStream instances can share a single BranchWorker without interference.
// Expected behavior:
// - Each stream maintains its own baseFolder
// - Events are properly isolated by baseFolder
// - All events converge at the shared worker.
func TestGitDestinationEventStream_MultipleStreamsWithSharedWorker(t *testing.T) {
	// Create shared mock worker
	mockWorker := &mockEventEnqueuer{events: make([]git.Event, 0)}
	logger := logr.Discard()

	// Create first GitDestinationEventStream for "apps" folder
	stream1 := NewGitDestinationEventStream("dest1", "default", mockWorker, logger)

	// Create second GitDestinationEventStream for "infra" folder
	stream2 := NewGitDestinationEventStream("dest2", "default", mockWorker, logger)

	// Complete reconciliation for both streams
	stream1.OnReconciliationComplete()
	stream2.OnReconciliationComplete()

	// Send events to first stream
	event1 := createTestEventWithBaseFolder("pod", "app-pod", "CREATE", "apps")
	stream1.OnWatchEvent(event1)

	// Send events to second stream
	event2 := createTestEventWithBaseFolder("deployment", "nginx", "CREATE", "infra")
	stream2.OnWatchEvent(event2)

	// Verify both events reached the shared worker
	assert.Len(t, mockWorker.events, 2, "Both events should reach shared worker")

	// Verify baseFolder isolation
	foundApps := false
	foundInfra := false
	for _, evt := range mockWorker.events {
		if evt.BaseFolder == "apps" && evt.Identifier.Name == "app-pod" {
			foundApps = true
		}
		if evt.BaseFolder == "infra" && evt.Identifier.Name == "nginx" {
			foundInfra = true
		}
	}
	assert.True(t, foundApps, "Event with 'apps' baseFolder should be present")
	assert.True(t, foundInfra, "Event with 'infra' baseFolder should be present")
}

// TestGitDestinationEventStream_DuplicateEventsAcrossStreams tests that the same cluster
// resource observed by multiple streams produces separate events for each stream.
// Expected behavior:
// - Same resource → multiple Git paths (one per stream's baseFolder)
// - Event duplication is intentional for multi-cluster scenarios.
func TestGitDestinationEventStream_DuplicateEventsAcrossStreams(t *testing.T) {
	// Create shared mock worker
	mockWorker := &mockEventEnqueuer{events: make([]git.Event, 0)}
	logger := logr.Discard()

	// Create two streams watching the same resources but writing to different folders
	streamClusterA := NewGitDestinationEventStream("cluster-a-dest", "default", mockWorker, logger)
	streamClusterB := NewGitDestinationEventStream("cluster-b-dest", "default", mockWorker, logger)

	// Complete reconciliation
	streamClusterA.OnReconciliationComplete()
	streamClusterB.OnReconciliationComplete()

	// Simulate same resource change observed by both streams
	// This represents a scenario where both GitDestinations watch the same cluster
	eventForClusterA := createTestEventWithBaseFolder("configmap", "shared-config", "UPDATE", "cluster-a")
	eventForClusterB := createTestEventWithBaseFolder("configmap", "shared-config", "UPDATE", "cluster-b")

	streamClusterA.OnWatchEvent(eventForClusterA)
	streamClusterB.OnWatchEvent(eventForClusterB)

	// Verify both events were enqueued (duplication is intentional)
	assert.Len(t, mockWorker.events, 2, "Both duplicate events should be enqueued")

	// Verify both baseFolders are represented
	foundClusterA := false
	foundClusterB := false
	for _, evt := range mockWorker.events {
		if evt.BaseFolder == "cluster-a" && evt.Identifier.Name == "shared-config" {
			foundClusterA = true
		}
		if evt.BaseFolder == "cluster-b" && evt.Identifier.Name == "shared-config" {
			foundClusterB = true
		}
	}
	assert.True(t, foundClusterA, "Event for 'cluster-a' should be present")
	assert.True(t, foundClusterB, "Event for 'cluster-b' should be present")
}

// TestGitDestinationEventStream_StreamDeletion tests what happens when a
// GitDestinationEventStream is deleted (no longer sending events).
// Expected behavior:
// - Other streams continue to operate normally
// - Shared worker continues processing events from remaining streams
// - Files from deleted stream remain in Git (no automatic cleanup).
func TestGitDestinationEventStream_StreamDeletion(t *testing.T) {
	// Create shared mock worker
	mockWorker := &mockEventEnqueuer{events: make([]git.Event, 0)}
	logger := logr.Discard()

	// Create two streams
	stream1 := NewGitDestinationEventStream("dest1", "default", mockWorker, logger)
	stream2 := NewGitDestinationEventStream("dest2", "default", mockWorker, logger)

	// Complete reconciliation
	stream1.OnReconciliationComplete()
	stream2.OnReconciliationComplete()

	// Send events to both streams
	event1 := createTestEventWithBaseFolder("pod", "app-pod", "CREATE", "apps")
	event2 := createTestEventWithBaseFolder("deployment", "infra-deploy", "CREATE", "infra")

	stream1.OnWatchEvent(event1)
	stream2.OnWatchEvent(event2)

	initialEventCount := len(mockWorker.events)
	assert.Equal(t, 2, initialEventCount, "Both initial events should be enqueued")

	// Simulate deletion of stream1 by stopping sending events to it
	// In reality, the stream object would be garbage collected and no longer receive events
	// We test this by only sending events to stream2 from this point forward

	// Stream2 continues to send events (stream1 is "deleted" - no longer used)
	event3 := createTestEventWithBaseFolder("service", "infra-svc", "CREATE", "infra")
	stream2.OnWatchEvent(event3)

	// Verify worker continues processing events from remaining stream
	assert.Greater(t, len(mockWorker.events), initialEventCount, "Worker should continue processing events")

	// Verify the new event has correct baseFolder from remaining stream
	foundInfraService := false
	for i := initialEventCount; i < len(mockWorker.events); i++ {
		evt := mockWorker.events[i]
		if evt.BaseFolder == "infra" && evt.Identifier.Name == "infra-svc" {
			foundInfraService = true
		}
	}
	assert.True(t, foundInfraService, "Event for remaining stream should be processed")

	// Note: This test does not verify Git file cleanup because:
	// 1. GitDestinationEventStream deletion does not trigger cleanup
	// 2. Files remain in Git history even after stream deletion
	// 3. WorkerManager handles the actual worker lifecycle, not the stream
}

// TestGitDestinationEventStream_EventConvergence tests that events from multiple
// streams converge at a shared worker for batched commit processing.
// Expected behavior:
// - Multiple streams can send events concurrently
// - All events converge at the shared worker
// - Events from different baseFolders are batched together.
func TestGitDestinationEventStream_EventConvergence(t *testing.T) {
	// Create shared mock worker
	mockWorker := &mockEventEnqueuer{events: make([]git.Event, 0)}
	logger := logr.Discard()

	// Create multiple streams
	streamTeamA := NewGitDestinationEventStream("team-a-dest", "default", mockWorker, logger)
	streamTeamB := NewGitDestinationEventStream("team-b-dest", "default", mockWorker, logger)
	streamTeamC := NewGitDestinationEventStream("team-c-dest", "default", mockWorker, logger)

	// Complete reconciliation for all streams
	streamTeamA.OnReconciliationComplete()
	streamTeamB.OnReconciliationComplete()
	streamTeamC.OnReconciliationComplete()

	// Send events from different streams concurrently
	var wg sync.WaitGroup
	streams := []*GitDestinationEventStream{streamTeamA, streamTeamB, streamTeamC}
	baseFolders := []string{"team-a", "team-b", "team-c"}

	for i, stream := range streams {
		wg.Add(1)
		go func(idx int, s *GitDestinationEventStream, baseFolder string) {
			defer wg.Done()
			event := createTestEventWithBaseFolder("pod", "pod-"+string(rune('a'+idx)), "CREATE", baseFolder)
			s.OnWatchEvent(event)
		}(i, stream, baseFolders[i])
	}

	wg.Wait()

	// Verify all events converged at the worker
	assert.GreaterOrEqual(t, len(mockWorker.events), 3, "All events should converge at shared worker")

	// Verify events from all streams are present
	baseFoldersFound := make(map[string]bool)
	for _, evt := range mockWorker.events {
		baseFoldersFound[evt.BaseFolder] = true
	}

	assert.True(t, baseFoldersFound["team-a"], "Events from team-a should converge")
	assert.True(t, baseFoldersFound["team-b"], "Events from team-b should converge")
	assert.True(t, baseFoldersFound["team-c"], "Events from team-c should converge")
}

// TestGitDestinationEventStream_DeduplicationPerStream tests that each stream
// performs its own deduplication independently.
// Expected behavior:
// - Duplicate events within a stream are deduplicated
// - Same event sent to different streams is NOT deduplicated (intentional).
func TestGitDestinationEventStream_DeduplicationPerStream(t *testing.T) {
	// Create shared mock worker
	mockWorker := &mockEventEnqueuer{events: make([]git.Event, 0)}
	logger := logr.Discard()

	// Create two streams
	stream1 := NewGitDestinationEventStream("dest1", "default", mockWorker, logger)
	stream2 := NewGitDestinationEventStream("dest2", "default", mockWorker, logger)

	// Complete reconciliation
	stream1.OnReconciliationComplete()
	stream2.OnReconciliationComplete()

	// Send same event twice to stream1 (should be deduplicated)
	event1a := createTestEventWithBaseFolder("pod", "test-pod", "UPDATE", "apps")
	event1b := createTestEventWithBaseFolder("pod", "test-pod", "UPDATE", "apps")

	stream1.OnWatchEvent(event1a)
	stream1.OnWatchEvent(event1b) // Duplicate - should be ignored

	// Send same resource to stream2 (should NOT be deduplicated across streams)
	event2 := createTestEventWithBaseFolder("pod", "test-pod", "UPDATE", "infra")
	stream2.OnWatchEvent(event2)

	// Verify: 1 from stream1 (deduplicated) + 1 from stream2 = 2 total
	assert.Len(t, mockWorker.events, 2, "Should have 2 events (1 per stream, stream1 deduplicated)")

	// Verify both baseFolders are present (not deduplicated across streams)
	baseFoldersFound := make(map[string]bool)
	for _, evt := range mockWorker.events {
		baseFoldersFound[evt.BaseFolder] = true
	}

	assert.True(t, baseFoldersFound["apps"], "Event from stream1 should be present")
	assert.True(t, baseFoldersFound["infra"], "Event from stream2 should be present")
}

// mockEventEnqueuer implements EventEnqueuer interface for testing.
type mockEventEnqueuer struct {
	mu     sync.Mutex
	events []git.Event
}

func (m *mockEventEnqueuer) Enqueue(event git.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, event)
}

// createTestEventWithBaseFolder creates a test event with a specific baseFolder.
func createTestEventWithBaseFolder(resourceType, name, operation, baseFolder string) git.Event {
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
		BaseFolder: baseFolder,
	}
}
