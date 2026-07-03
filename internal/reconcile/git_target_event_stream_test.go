// SPDX-License-Identifier: Apache-2.0

package reconcile

import (
	"testing"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/git/manifestedit"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func TestGitTargetEventStream(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "GitTargetEventStream Suite")
}

// With the R3 pivot the stream is a thin pass-through to the branch worker: no buffering, no
// state machine, no content-hash dedup. "Newer?" is the audit RV order and "changed?" is the
// writer's no-op detection at the commit boundary, so the only behaviour left to assert is that
// an event is forwarded with the GitTarget identity stamped on it (and that an empty-payload
// control event is dropped).
var _ = Describe("GitTargetEventStream", func() {
	var (
		stream        *GitTargetEventStream
		mockWorker    *mockBranchWorker
		gitTargetName = "test-gittarget"
		gitTargetNS   = "test-ns"
	)

	BeforeEach(func() {
		mockWorker = &mockBranchWorker{events: make([]git.Event, 0)}
		stream = NewGitTargetEventStream(gitTargetName, gitTargetNS, mockWorker, logr.Discard())
	})

	It("forwards an object event immediately, stamped with the GitTarget identity", func() {
		Expect(stream.OnWatchEvent(createTestEvent("pod", "test-pod", "UPDATE"))).To(Succeed())

		Expect(mockWorker.events).To(HaveLen(1))
		Expect(mockWorker.events[0].GitTargetName).To(Equal(gitTargetName))
		Expect(mockWorker.events[0].GitTargetNamespace).To(Equal(gitTargetNS))
	})

	It("forwards a field-patch event that carries no object", func() {
		Expect(stream.OnWatchEvent(createTestFieldPatchEvent(3))).To(Succeed())

		Expect(mockWorker.events).To(HaveLen(1))
		Expect(mockWorker.events[0].IsFieldPatch()).To(BeTrue())
		Expect(mockWorker.events[0].Object).To(BeNil())
	})

	It("forwards a DELETE event even when it carries no object payload", func() {
		ev := createTestEvent("pod", "gone", "DELETE")
		ev.Object = nil
		Expect(stream.OnWatchEvent(ev)).To(Succeed())

		Expect(mockWorker.events).To(HaveLen(1))
	})

	It("drops a non-delete, non-field-patch event with no object payload", func() {
		ev := createTestEvent("pod", "empty", "UPDATE")
		ev.Object = nil
		Expect(stream.OnWatchEvent(ev)).To(Succeed())

		Expect(mockWorker.events).To(BeEmpty())
	})

	It("forwards each call without deduplication (RV order + writer no-op detection own that now)", func() {
		Expect(stream.OnWatchEvent(createTestFieldPatchEvent(3))).To(Succeed())
		// identical redelivery is forwarded too
		Expect(stream.OnWatchEvent(createTestFieldPatchEvent(3))).To(Succeed())

		Expect(mockWorker.events).To(HaveLen(2))
	})

	It("returns an error and forwards nothing when the worker queue is full", func() {
		// Models the cursor-safety contract: a full FIFO must surface as an error so the
		// watch loop leaves its durable cursor un-advanced and redelivers on reconnect,
		// rather than silently dropping the event and skipping it forever.
		mockWorker.full = true

		Expect(stream.OnWatchEvent(createTestEvent("configmap", "full-queue", "UPDATE"))).NotTo(Succeed())
		Expect(mockWorker.events).To(BeEmpty())
	})
})

// mockBranchWorker implements the EventEnqueuer interface for testing. When full is set
// it models a full FIFO: Enqueue records nothing and reports the drop.
type mockBranchWorker struct {
	events []git.Event
	full   bool
}

func (m *mockBranchWorker) Enqueue(event git.Event) bool {
	if m.full {
		return false
	}
	m.events = append(m.events, event)
	return true
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

// createTestFieldPatchEvent builds a deployments/scale-shaped field-patch event:
// no Object, just a spec.replicas assignment against a parent Deployment identity.
func createTestFieldPatchEvent(replicas int64) git.Event {
	identifier := types.ResourceIdentifier{
		Group:     "apps",
		Version:   "v1",
		Resource:  "deployments",
		Name:      "web",
		Namespace: "default",
	}

	return git.Event{
		FieldPatch: &git.FieldPatch{
			Assignments: []manifestedit.FieldAssignment{
				{Path: []string{"spec", "replicas"}, Value: replicas},
			},
			Source: "deployments/scale",
		},
		Identifier: identifier,
		Operation:  "UPDATE",
		UserInfo:   git.UserInfo{Username: "test-user", UID: "test-uid"},
		Path:       "test-folder",
	}
}
