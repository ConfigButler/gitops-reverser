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
	"testing"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func TestGitTargetEventStream(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "GitTargetEventStream Suite")
}

var _ = Describe("GitTargetEventStream", func() {
	var (
		stream        *GitTargetEventStream
		mockWorker    *mockBranchWorker
		logger        logr.Logger
		gitTargetName = "test-gittarget"
		gitTargetNS   = "test-ns"
	)

	BeforeEach(func() {
		mockWorker = &mockBranchWorker{events: make([]git.Event, 0), batches: make([]*git.ReconcileBatch, 0)}
		logger = logr.Discard()
		stream = NewGitTargetEventStream(gitTargetName, gitTargetNS, mockWorker, logger)
	})

	Describe("Initial State", func() {
		It("should start in RECONCILING state", func() {
			Expect(stream.GetState()).To(Equal(Reconciling))
		})

		It("should have empty buffers initially", func() {
			Expect(stream.GetBufferedEventCount()).To(Equal(0))
			Expect(stream.GetProcessedEventCount()).To(Equal(0))
		})
	})

	Describe("Event Buffering During Reconciliation", func() {
		It("should buffer events during RECONCILING", func() {
			event := createTestEvent("pod", "test-pod", "CREATE")

			stream.OnWatchEvent(event)

			Expect(stream.GetBufferedEventCount()).To(Equal(1))
			Expect(mockWorker.events).To(BeEmpty()) // Not forwarded yet
		})

		It("should deduplicate buffered events", func() {
			event1 := createTestEvent("pod", "test-pod", "CREATE")
			event2 := createTestEvent("pod", "test-pod", "CREATE") // Same content

			stream.OnWatchEvent(event1)
			stream.OnWatchEvent(event2)

			Expect(stream.GetBufferedEventCount()).To(Equal(2)) // Both buffered (no deduplication during buffering)
		})
	})

	Describe("Reconciliation Completion", func() {
		It("should process buffered events when reconciliation completes", func() {
			event1 := createTestEvent("pod", "pod1", "CREATE")
			event2 := createTestEvent("service", "svc1", "CREATE")

			// Buffer events
			stream.OnWatchEvent(event1)
			stream.OnWatchEvent(event2)
			Expect(stream.GetBufferedEventCount()).To(Equal(2))

			// Complete reconciliation
			stream.OnReconciliationComplete()

			// Should transition to LIVE_PROCESSING
			Expect(stream.GetState()).To(Equal(LiveProcessing))
			Expect(stream.GetBufferedEventCount()).To(Equal(0))

			// Should forward events to worker
			Expect(mockWorker.events).To(HaveLen(2))
		})

		It("should transition state correctly", func() {
			Expect(stream.GetState()).To(Equal(Reconciling))

			stream.OnReconciliationComplete()

			Expect(stream.GetState()).To(Equal(LiveProcessing))
		})
	})

	Describe("Live Processing", func() {
		BeforeEach(func() {
			stream.OnReconciliationComplete() // Transition to live processing
		})

		It("should process events immediately in LIVE_PROCESSING", func() {
			event := createTestEvent("pod", "test-pod", "UPDATE")

			stream.OnWatchEvent(event)

			Expect(mockWorker.events).To(HaveLen(1))
			Expect(stream.GetProcessedEventCount()).To(Equal(1))
		})

		It("should deduplicate events in live processing", func() {
			event1 := createTestEvent("pod", "test-pod", "UPDATE")
			event2 := createTestEvent("pod", "test-pod", "UPDATE") // Duplicate

			stream.OnWatchEvent(event1)
			stream.OnWatchEvent(event2)

			Expect(mockWorker.events).To(HaveLen(1)) // Only one forwarded
			Expect(stream.GetProcessedEventCount()).To(Equal(1))
		})
	})

	Describe("BeginReconciliation", func() {
		It("should be a no-op when already in RECONCILING", func() {
			Expect(stream.GetState()).To(Equal(Reconciling))
			stream.BeginReconciliation()
			Expect(stream.GetState()).To(Equal(Reconciling))
		})

		It("should re-enter RECONCILING from LIVE_PROCESSING and buffer new events", func() {
			stream.OnReconciliationComplete() // → LiveProcessing
			Expect(stream.GetState()).To(Equal(LiveProcessing))

			stream.BeginReconciliation() // → Reconciling again
			Expect(stream.GetState()).To(Equal(Reconciling))

			event := createTestEvent("pod", "test-pod", "UPDATE")
			stream.OnWatchEvent(event)

			Expect(stream.GetBufferedEventCount()).To(Equal(1))
			Expect(mockWorker.events).To(BeEmpty()) // buffered, not forwarded
		})
	})

	Describe("EmitReconcileBatch", func() {
		It("should enqueue batch to worker and stamp GitTarget info", func() {
			batch := git.ReconcileBatch{
				Events: []git.Event{
					createTestEvent("pod", "pod1", "CREATE"),
				},
				CommitMessage: "reconcile: sync 1 resources",
			}

			err := stream.EmitReconcileBatch(batch)
			Expect(err).NotTo(HaveOccurred())
			Expect(mockWorker.batches).To(HaveLen(1))
			Expect(mockWorker.batches[0].GitTargetName).To(Equal(gitTargetName))
			Expect(mockWorker.batches[0].GitTargetNamespace).To(Equal(gitTargetNS))
		})
	})

	Describe("Event Hash Deduplication", func() {
		It("should treat different operations as different events", func() {
			event1 := createTestEvent("pod", "test-pod", "CREATE")
			event2 := createTestEvent("pod", "test-pod", "UPDATE")

			stream.OnReconciliationComplete() // Live processing

			stream.OnWatchEvent(event1)
			stream.OnWatchEvent(event2)

			Expect(mockWorker.events).To(HaveLen(2))
		})

		It("should treat different resources as different events", func() {
			event1 := createTestEvent("pod", "pod1", "CREATE")
			event2 := createTestEvent("pod", "pod2", "CREATE")

			stream.OnReconciliationComplete() // Live processing

			stream.OnWatchEvent(event1)
			stream.OnWatchEvent(event2)

			Expect(mockWorker.events).To(HaveLen(2))
		})
	})
})

// mockBranchWorker implements EventEnqueuer interface for testing.
type mockBranchWorker struct {
	events  []git.Event
	batches []*git.ReconcileBatch
}

func (m *mockBranchWorker) Enqueue(event git.Event) {
	m.events = append(m.events, event)
}

func (m *mockBranchWorker) EnqueueRequest(request *git.WriteRequest) {
	if request == nil {
		return
	}
	if request.CommitMode == git.CommitModeAtomic {
		m.batches = append(m.batches, request)
		return
	}
	m.events = append(m.events, request.Events...)
}

func (m *mockBranchWorker) EnqueueBatch(batch *git.ReconcileBatch) {
	m.batches = append(m.batches, batch)
}

// createTestEvent creates a test event with minimal required fields.
func createTestEvent(resourceType, name, operation string) git.Event {
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
		Path:       "test-folder",
	}
}
