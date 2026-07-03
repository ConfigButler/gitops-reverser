// SPDX-License-Identifier: Apache-2.0

package recorder

import (
	"context"
	"encoding/json"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
)

// rewatchBackoff is the pause before reopening a closed watch.
const rewatchBackoff = 200 * time.Millisecond

// watchAdder is the slice of the store the watch recorder needs; narrowing it
// keeps the recorder testable without a cluster.
type watchAdder interface {
	Add(r mutationlab.Record) mutationlab.Record
}

// Watch records native watch events. It is expected to be the strongest source
// for "what object state exists or disappeared" — and, per the lab's central
// hypothesis, to carry the full object precisely where the audit body goes
// shallow. It is not expected to know the user who caused the change.
type Watch struct {
	store  watchAdder
	client dynamic.Interface
}

// NewWatch returns a Watch recorder over the given dynamic client.
func NewWatch(store watchAdder, client dynamic.Interface) *Watch {
	return &Watch{store: store, client: client}
}

// Start opens a watch per GVR (all namespaces) and records every event until ctx
// is cancelled. Each GVR is watched in its own goroutine; a closed or failed
// watch is reopened after a short backoff.
func (w *Watch) Start(ctx context.Context, gvrs []schema.GroupVersionResource) {
	for _, gvr := range gvrs {
		go w.watchGVR(ctx, gvr)
	}
}

func (w *Watch) watchGVR(ctx context.Context, gvr schema.GroupVersionResource) {
	for ctx.Err() == nil {
		wi, err := w.client.Resource(gvr).
			Namespace(metav1.NamespaceAll).
			Watch(ctx, metav1.ListOptions{AllowWatchBookmarks: true})
		if err != nil {
			if sleepCtx(ctx, time.Second) {
				return
			}
			continue
		}
		w.drain(ctx, wi)
		if sleepCtx(ctx, rewatchBackoff) {
			return
		}
	}
}

func (w *Watch) drain(ctx context.Context, wi watch.Interface) {
	defer wi.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-wi.ResultChan():
			if !ok {
				return
			}
			w.store.Add(buildWatchRecord(ev.Type, ev.Object))
		}
	}
}

// buildWatchRecord converts a single watch event into a Record. It is pure so the
// conversion can be unit-tested without a cluster.
func buildWatchRecord(eventType watch.EventType, obj runtime.Object) mutationlab.Record {
	rec := mutationlab.Record{
		Source:     mutationlab.SourceWatch,
		ObservedAt: time.Now(),
		Summary:    mutationlab.RecordSummary{WatchType: string(eventType), HasObject: obj != nil},
		Raw:        watchRaw(eventType, obj),
	}
	if u, ok := obj.(*unstructured.Unstructured); ok {
		rec.Key = watchObjectKey(u)
		rec.Scenario = scenarioFromLabels(u.GetLabels(), u.GetNamespace())
	}
	return rec
}

func watchRaw(eventType watch.EventType, obj runtime.Object) json.RawMessage {
	envelope := map[string]any{"type": string(eventType)}
	if u, ok := obj.(*unstructured.Unstructured); ok {
		envelope["object"] = u.Object
	} else if obj != nil {
		envelope["object"] = obj
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return json.RawMessage(`{"type":"` + string(eventType) + `"}`)
	}
	return raw
}

func watchObjectKey(u *unstructured.Unstructured) mutationlab.ObjectKey {
	gvk := u.GroupVersionKind()
	return mutationlab.ObjectKey{
		Group:           gvk.Group,
		Version:         gvk.Version,
		Namespace:       u.GetNamespace(),
		Name:            u.GetName(),
		UID:             string(u.GetUID()),
		ResourceVersion: u.GetResourceVersion(),
	}
}

// sleepCtx waits d or until ctx is done; it returns true if ctx was cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-t.C:
		return false
	}
}
