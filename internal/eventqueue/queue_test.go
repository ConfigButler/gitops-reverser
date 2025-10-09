package eventqueue

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func TestNewQueue(t *testing.T) {
	queue := NewQueue()
	assert.NotNil(t, queue)
	assert.NotNil(t, queue.events)
	assert.Empty(t, queue.events)
	assert.Equal(t, 0, queue.Size())
}

func TestEnqueue_SingleEvent(t *testing.T) {
	queue := NewQueue()

	obj := &unstructured.Unstructured{}
	obj.SetName("test-pod")
	obj.SetNamespace("default")
	obj.SetKind("Pod")

	event := Event{
		Object: obj,
		Identifier: types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "pods",
			Namespace: "default",
			Name:      "test-pod",
		},
		Operation: "CREATE",
		UserInfo: UserInfo{
			Username: "test-user",
			UID:      "test-uid",
		},
		GitRepoConfigRef: "test-repo-config",
	}

	queue.Enqueue(event)

	assert.Equal(t, 1, queue.Size())
}

func TestEnqueue_MultipleEvents(t *testing.T) {
	queue := NewQueue()

	// Create multiple events
	for i := range 5 {
		obj := &unstructured.Unstructured{}
		obj.SetName("test-pod-" + string(rune(i)))
		obj.SetNamespace("default")
		obj.SetKind("Pod")

		event := Event{
			Object: obj,
			Identifier: types.ResourceIdentifier{
				Group:     "",
				Version:   "v1",
				Resource:  "pods",
				Namespace: "default",
				Name:      "test-pod-" + string(rune(i)),
			},
			Operation: "CREATE",
			UserInfo: UserInfo{
				Username: "test-user",
				UID:      "test-uid-" + string(rune(i)),
			},
			GitRepoConfigRef: "test-repo-config",
		}

		queue.Enqueue(event)
	}

	assert.Equal(t, 5, queue.Size())
}

func TestDequeueAll_EmptyQueue(t *testing.T) {
	queue := NewQueue()

	events := queue.DequeueAll()
	assert.Nil(t, events)
	assert.Equal(t, 0, queue.Size())
}

func TestDequeueAll_SingleEvent(t *testing.T) {
	queue := NewQueue()

	obj := &unstructured.Unstructured{}
	obj.SetName("test-pod")
	obj.SetNamespace("default")
	obj.SetKind("Pod")

	originalEvent := Event{
		Object: obj,
		Identifier: types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "pods",
			Namespace: "default",
			Name:      "test-pod",
		},
		Operation: "CREATE",
		UserInfo: UserInfo{
			Username: "test-user",
			UID:      "test-uid",
		},
		GitRepoConfigRef: "test-repo-config",
	}

	queue.Enqueue(originalEvent)
	assert.Equal(t, 1, queue.Size())

	events := queue.DequeueAll()
	assert.NotNil(t, events)
	assert.Len(t, events, 1)
	assert.Equal(t, 0, queue.Size()) // Queue should be empty after dequeue

	// Verify the dequeued event
	dequeuedEvent := events[0]
	assert.Equal(t, "test-pod", dequeuedEvent.Object.GetName())
	assert.Equal(t, "default", dequeuedEvent.Object.GetNamespace())
	assert.Equal(t, "Pod", dequeuedEvent.Object.GetKind())
	assert.Equal(t, "test-uid", dequeuedEvent.UserInfo.UID)
	assert.Equal(t, "CREATE", dequeuedEvent.Operation)
	assert.Equal(t, "test-user", dequeuedEvent.UserInfo.Username)
	assert.Equal(t, "test-repo-config", dequeuedEvent.GitRepoConfigRef)
}

func TestDequeueAll_MultipleEvents(t *testing.T) {
	queue := NewQueue()

	// Enqueue multiple events
	expectedEvents := make([]Event, 3)
	for i := range 3 {
		obj := &unstructured.Unstructured{}
		obj.SetName("test-pod-" + string(rune('0'+i)))
		obj.SetNamespace("default")
		obj.SetKind("Pod")

		event := Event{
			Object: obj,
			Identifier: types.ResourceIdentifier{
				Group:     "",
				Version:   "v1",
				Resource:  "pods",
				Namespace: "default",
				Name:      "test-pod-" + string(rune('0'+i)),
			},
			Operation: "CREATE",
			UserInfo: UserInfo{
				Username: "test-user",
				UID:      "test-uid-" + string(rune('0'+i)),
			},
			GitRepoConfigRef: "test-repo-config-" + string(rune('0'+i)),
		}

		expectedEvents[i] = event
		queue.Enqueue(event)
	}

	assert.Equal(t, 3, queue.Size())

	events := queue.DequeueAll()
	assert.NotNil(t, events)
	assert.Len(t, events, 3)
	assert.Equal(t, 0, queue.Size()) // Queue should be empty after dequeue

	// Verify all events are returned in order
	for i, event := range events {
		assert.Equal(t, "test-pod-"+string(rune('0'+i)), event.Object.GetName())
		assert.Equal(t, "test-uid-"+string(rune('0'+i)), event.UserInfo.UID)
		assert.Equal(t, "test-repo-config-"+string(rune('0'+i)), event.GitRepoConfigRef)
	}
}

func TestDequeueAll_ConsecutiveCalls(t *testing.T) {
	queue := NewQueue()

	// First batch
	for i := range 2 {
		obj := &unstructured.Unstructured{}
		obj.SetName("batch1-pod-" + string(rune('0'+i)))

		event := Event{
			Object: obj,
			Identifier: types.ResourceIdentifier{
				Group:     "",
				Version:   "v1",
				Resource:  "pods",
				Namespace: "default",
				Name:      "batch1-pod-" + string(rune('0'+i)),
			},
			Operation: "CREATE",
			UserInfo: UserInfo{
				Username: "test-user",
				UID:      "batch1-uid-" + string(rune('0'+i)),
			},
			GitRepoConfigRef: "batch1-repo",
		}
		queue.Enqueue(event)
	}

	// Dequeue first batch
	events1 := queue.DequeueAll()
	assert.Len(t, events1, 2)
	assert.Equal(t, 0, queue.Size())

	// Second dequeue should return nil
	events2 := queue.DequeueAll()
	assert.Nil(t, events2)

	// Add second batch
	for i := range 3 {
		obj := &unstructured.Unstructured{}
		obj.SetName("batch2-pod-" + string(rune('0'+i)))

		event := Event{
			Object: obj,
			Identifier: types.ResourceIdentifier{
				Group:     "",
				Version:   "v1",
				Resource:  "pods",
				Namespace: "default",
				Name:      "batch2-pod-" + string(rune('0'+i)),
			},
			Operation: "CREATE",
			UserInfo: UserInfo{
				Username: "test-user",
				UID:      "batch2-uid-" + string(rune('0'+i)),
			},
			GitRepoConfigRef: "batch2-repo",
		}
		queue.Enqueue(event)
	}

	// Dequeue second batch
	events3 := queue.DequeueAll()
	assert.Len(t, events3, 3)
	assert.Equal(t, 0, queue.Size())
}

func TestSize_Accuracy(t *testing.T) {
	queue := NewQueue()

	// Initially empty
	assert.Equal(t, 0, queue.Size())

	// Add events one by one
	for i := 1; i <= 5; i++ {
		obj := &unstructured.Unstructured{}
		obj.SetName("test-pod-" + string(rune('0'+i-1)))

		event := Event{
			Object: obj,
			Identifier: types.ResourceIdentifier{
				Group:     "",
				Version:   "v1",
				Resource:  "pods",
				Namespace: "default",
				Name:      "test-pod-" + string(rune('0'+i-1)),
			},
			Operation: "CREATE",
			UserInfo: UserInfo{
				Username: "test-user",
				UID:      "test-uid-" + string(rune('0'+i-1)),
			},
			GitRepoConfigRef: "test-repo",
		}

		queue.Enqueue(event)
		assert.Equal(t, i, queue.Size())
	}

	// Dequeue all
	events := queue.DequeueAll()
	assert.Len(t, events, 5)
	assert.Equal(t, 0, queue.Size())
}

func TestConcurrentAccess(t *testing.T) {
	queue := NewQueue()

	const numGoroutines = 10
	const eventsPerGoroutine = 100

	var wg sync.WaitGroup

	// Start multiple producer goroutines
	for g := range numGoroutines {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()

			for i := range eventsPerGoroutine {
				obj := &unstructured.Unstructured{}
				obj.SetName("pod-g" + string(rune('0'+goroutineID)) + "-e" + string(rune('0'+i)))

				event := Event{
					Object: obj,
					Identifier: types.ResourceIdentifier{
						Group:     "",
						Version:   "v1",
						Resource:  "pods",
						Namespace: "default",
						Name:      "pod-g" + string(rune('0'+goroutineID)) + "-e" + string(rune('0'+i)),
					},
					Operation: "CREATE",
					UserInfo: UserInfo{
						Username: "test-user",
						UID:      "uid-g" + string(rune('0'+goroutineID)) + "-e" + string(rune('0'+i)),
					},
					GitRepoConfigRef: "repo-g" + string(rune('0'+goroutineID)),
				}

				queue.Enqueue(event)
			}
		}(g)
	}

	// Start a consumer goroutine
	var totalDequeued int
	wg.Add(1)
	go func() {
		defer wg.Done()

		for {
			events := queue.DequeueAll()
			if events != nil {
				totalDequeued += len(events)
			}

			// Stop when we've dequeued all expected events
			if totalDequeued >= numGoroutines*eventsPerGoroutine {
				break
			}
		}
	}()

	wg.Wait()

	// Verify all events were processed
	assert.Equal(t, numGoroutines*eventsPerGoroutine, totalDequeued)

	// Final dequeue should be empty
	finalEvents := queue.DequeueAll()
	if finalEvents != nil {
		totalDequeued += len(finalEvents)
	}

	assert.Equal(t, numGoroutines*eventsPerGoroutine, totalDequeued)
	assert.Equal(t, 0, queue.Size())
}

func TestConcurrentEnqueueDequeue(t *testing.T) {
	queue := NewQueue()

	const numOperations = 1000
	var enqueuedCount, dequeuedCount int
	var mu sync.Mutex

	done := make(chan bool, 2)

	// Enqueue goroutine
	go func() {
		for i := range numOperations {
			obj := &unstructured.Unstructured{}
			obj.SetName("test-pod-" + string(rune('0'+i%10)))

			event := Event{
				Object: obj,
				Identifier: types.ResourceIdentifier{
					Group:     "",
					Version:   "v1",
					Resource:  "pods",
					Namespace: "default",
					Name:      "test-pod-" + string(rune('0'+i%10)),
				},
				Operation: "CREATE",
				UserInfo: UserInfo{
					Username: "test-user",
					UID:      "test-uid-" + string(rune('0'+i%10)),
				},
				GitRepoConfigRef: "test-repo",
			}

			queue.Enqueue(event)

			mu.Lock()
			enqueuedCount++
			mu.Unlock()
		}
		done <- true
	}()

	// Dequeue goroutine
	go func() {
		for {
			events := queue.DequeueAll()
			if events != nil {
				mu.Lock()
				dequeuedCount += len(events)
				shouldStop := dequeuedCount >= numOperations
				mu.Unlock()

				if shouldStop {
					break
				}
			}
		}
		done <- true
	}()

	// Wait for both goroutines
	<-done
	<-done

	// Verify counts
	mu.Lock()
	assert.Equal(t, numOperations, enqueuedCount)
	assert.Equal(t, numOperations, dequeuedCount)
	mu.Unlock()
}

func TestEventStructure(t *testing.T) {
	// Test that Event struct properly holds all required data
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      "test-pod",
				"namespace": "test-ns",
				"labels": map[string]interface{}{
					"app": "test-app",
				},
			},
			"spec": map[string]interface{}{
				"containers": []interface{}{
					map[string]interface{}{
						"name":  "test-container",
						"image": "nginx:latest",
					},
				},
			},
		},
	}

	event := Event{
		Object: obj,
		Identifier: types.ResourceIdentifier{
			Group:     "",
			Version:   "v1",
			Resource:  "pods",
			Namespace: "test-ns",
			Name:      "test-pod",
		},
		Operation: "UPDATE",
		UserInfo: UserInfo{
			Username: "test-user@example.com",
			UID:      "user-uid-123",
		},
		GitRepoConfigRef: "production-repo-config",
	}

	// Verify all fields are accessible
	assert.Equal(t, "test-pod", event.Object.GetName())
	assert.Equal(t, "test-ns", event.Object.GetNamespace())
	assert.Equal(t, "Pod", event.Object.GetKind())
	assert.Equal(t, "test-app", event.Object.GetLabels()["app"])

	assert.Equal(t, "test-pod", event.Identifier.Name)
	assert.Equal(t, "test-ns", event.Identifier.Namespace)
	assert.Equal(t, "UPDATE", event.Operation)
	assert.Equal(t, "test-user@example.com", event.UserInfo.Username)
	assert.Equal(t, "user-uid-123", event.UserInfo.UID)

	assert.Equal(t, "production-repo-config", event.GitRepoConfigRef)
}

func TestQueueBehaviorUnderLoad(t *testing.T) {
	queue := NewQueue()

	// Simulate high load scenario
	const batchSize = 1000

	// Enqueue a large batch
	for i := range batchSize {
		obj := &unstructured.Unstructured{}
		obj.SetName("load-test-pod-" + string(rune('0'+i%10)))
		obj.SetNamespace("load-test")

		event := Event{
			Object: obj,
			Identifier: types.ResourceIdentifier{
				Group:     "",
				Version:   "v1",
				Resource:  "pods",
				Namespace: "load-test",
				Name:      "load-test-pod-" + string(rune('0'+i%10)),
			},
			Operation: "CREATE",
			UserInfo: UserInfo{
				Username: "test-user",
				UID:      "load-test-uid-" + string(rune('0'+i%10)),
			},
			GitRepoConfigRef: "load-test-repo",
		}

		queue.Enqueue(event)
	}

	assert.Equal(t, batchSize, queue.Size())

	// Dequeue all at once
	events := queue.DequeueAll()
	assert.Len(t, events, batchSize)
	assert.Equal(t, 0, queue.Size())

	// Verify first and last events
	assert.Equal(t, "load-test-pod-0", events[0].Object.GetName())
	assert.Equal(t, "load-test-pod-"+string(rune('0'+(batchSize-1)%10)), events[batchSize-1].Object.GetName())
}
