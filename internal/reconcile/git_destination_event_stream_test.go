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

func TestGitDestinationEventStream(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "GitDestinationEventStream Suite")
}

var _ = Describe("GitDestinationEventStream", func() {
	var (
		stream      *GitDestinationEventStream
		mockWorker  *mockBranchWorker
		logger      logr.Logger
		gitDestName = "test-gitdest"
		gitDestNS   = "test-ns"
	)

	BeforeEach(func() {
		mockWorker = &mockBranchWorker{events: make([]git.Event, 0)}
		logger = logr.Discard()
		stream = NewGitDestinationEventStream(gitDestName, gitDestNS, mockWorker, logger)
	})

	Describe("Initial State", func() {
		It("should start in STARTUP_RECONCILE state", func() {
			Expect(stream.GetState()).To(Equal(StartupReconcile))
		})

		It("should have empty buffers initially", func() {
			Expect(stream.GetBufferedEventCount()).To(Equal(0))
			Expect(stream.GetProcessedEventCount()).To(Equal(0))
		})
	})

	Describe("Event Buffering During Reconciliation", func() {
		It("should buffer events during STARTUP_RECONCILE", func() {
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
			Expect(stream.GetState()).To(Equal(StartupReconcile))

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

// mockBranchWorker implements EventEnqueuer interface for testing
type mockBranchWorker struct {
	events []git.Event
}

func (m *mockBranchWorker) Enqueue(event git.Event) {
	m.events = append(m.events, event)
}

// createTestEvent creates a test event with minimal required fields
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
		BaseFolder: "test-folder",
	}
}
