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
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/utils/ptr"

	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// streamCheckpointTimeout bounds the WATCH-first initial-events stream. A conformant apiserver
// delivers the whole initial set plus the initial-events-end bookmark well inside this; a
// non-conformant aggregated backend that accepts the watch but never emits the bookmark would
// otherwise hang the (serial) materialization driver — see docs/design/upgrade-finding.md. When
// it fires we fall back to a consistent LIST, never to a partial checkpoint.
const streamCheckpointTimeout = 60 * time.Second

// errStreamFallback marks a WATCH-first attempt that could not prove a complete initial set:
// the server rejected streaming-list, closed or timed out before the initial-events-end
// bookmark, or streamed a malformed object. mirrorTypeObjects answers it by retrying the type
// through the consistent LIST path. Ordinary context cancellation is NOT wrapped in it, so a
// shutdown ends the sync rather than falling back.
var errStreamFallback = errors.New("streaming-list checkpoint unavailable")

// objectEnvelope is the per-object value stored in "<base>:objects:items". It lifts the
// identity, resourceVersion, and generation out of the body — sanitize strips exactly those
// server fields, so they would otherwise be unreadable — and stores them beside the sanitized
// object under the same field names the audit stream overview uses, keeping the two
// structures directly joinable.
type objectEnvelope struct {
	APIGroup        string          `json:"api_group"`
	APIVersion      string          `json:"api_version"`
	Resource        string          `json:"resource"`
	Kind            string          `json:"kind,omitempty"`
	Namespace       string          `json:"namespace,omitempty"`
	Name            string          `json:"name"`
	UID             string          `json:"uid,omitempty"`
	ResourceVersion string          `json:"resource_version,omitempty"`
	Generation      int64           `json:"generation,omitempty"`
	Object          json.RawMessage `json:"object"`
}

// mirrorTypeObjects captures the current set of objects for a type and replaces the type's
// checkpoint in the per-resource-type keyspace, returning the resourceVersion the checkpoint is
// pinned to, which the Materializer records as the type's Synced revision. It is the
// demand-driven checkpoint fill the materialization driver runs for a CLAIMED type (L-3,
// runTypeCheckpointSync), no longer unconditionally on every activation.
//
// It prefers a WATCH-first streaming-list (streamTypeObjects), which pins the checkpoint to the
// initial-events-end bookmark and avoids forcing one large LIST response per re-anchor. A
// non-conformant watch path (rejected streaming-list, no bookmark, malformed stream) falls back
// to the original consistent LIST (listTypeObjects); no checkpoint is ever pinned to a partial
// watch. A nil mirror or missing dynamic client is a benign no-op (empty rv, no error); a fill
// or replace error is returned so the driver records SyncFailed and the prior checkpoint (if
// any) keeps serving. See docs/design/stream/watch-list-checkpoint-plan.md and
// docs/design/stream/per-resource-type-rv-keyed-streams-experiment.md.
func (m *Manager) mirrorTypeObjects(
	ctx context.Context, log logr.Logger, gvr schema.GroupVersionResource,
) (string, error) {
	if m.ObjectMirror == nil {
		return "", nil
	}
	dc := m.dynamicClientFromConfig(log)
	if dc == nil {
		return "", nil
	}

	path := "watch"
	items, rv, err := m.streamTypeObjects(ctx, log, dc, gvr)
	if err != nil {
		if !errors.Is(err, errStreamFallback) {
			// Shutdown/cancellation: end the sync, do not fall back.
			return "", err
		}
		log.V(1).Info("objects-mirror: WATCH-first unavailable, falling back to LIST",
			"gvr", gvr.String(), "reason", err.Error())
		path = "list"
		items, rv, err = m.listTypeObjects(ctx, log, dc, gvr)
		if err != nil {
			return "", err
		}
	}

	if err := m.ObjectMirror.ReplaceTypeObjects(ctx, gvr.Group, gvr.Version, gvr.Resource, items, rv); err != nil {
		return "", fmt.Errorf("objects-mirror: replace %s: %w", gvr.String(), err)
	}
	if telemetry.MaterializationCheckpointFillsTotal != nil {
		telemetry.MaterializationCheckpointFillsTotal.Add(ctx, 1,
			metric.WithAttributes(attribute.String("path", path)))
	}
	log.Info("objects-mirror: snapshot loaded",
		"gvr", gvr.String(), "count", len(items), "resourceVersion", rv, "path", path)
	return rv, nil
}

// streamTypeObjects fills the checkpoint with a Kubernetes streaming-list watch
// (sendInitialEvents=true, resourceVersionMatch=NotOlderThan, allowWatchBookmarks=true). It
// folds the initial ADDED events into the same envelope map listTypeObjects builds and stops at
// the initial-events-end bookmark, pinning the checkpoint to THAT bookmark's resourceVersion —
// not the last object's rv. Live events after the bookmark are intentionally ignored: freshness
// past R belongs to the Redis audit tail and the periodic/heal reconcile.
//
// It returns errStreamFallback (wrapped) when it cannot prove a complete initial set — the
// caller then retries the type through the LIST path. Parent-context cancellation is returned
// unwrapped so a shutdown ends the sync instead.
func (m *Manager) streamTypeObjects(
	ctx context.Context, log logr.Logger, dc dynamic.Interface, gvr schema.GroupVersionResource,
) (map[string]string, string, error) {
	timeout := streamCheckpointTimeout
	if m.streamCheckpointTimeoutOverride > 0 {
		timeout = m.streamCheckpointTimeoutOverride
	}
	streamCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Empty namespace watches across all namespaces (and is correct for cluster-scoped types).
	opts := metav1.ListOptions{
		SendInitialEvents:    ptr.To(true),
		ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
		AllowWatchBookmarks:  true,
		TimeoutSeconds:       ptr.To(int64(streamCheckpointTimeout / time.Second)),
	}

	w, err := m.openCheckpointWatch(streamCtx, dc, gvr, opts)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", fmt.Errorf("%w: open watch %s: %w", errStreamFallback, gvr.String(), err)
	}
	defer w.Stop()

	items := make(map[string]string)
	for {
		select {
		case <-streamCtx.Done():
			if ctx.Err() != nil {
				return nil, "", ctx.Err() // parent shutdown: end the sync
			}
			// Our own deadline fired: the bookmark never arrived (e.g. a backend that accepts the
			// watch but does not honor sendInitialEvents).
			return nil, "", fmt.Errorf("%w: no initial-events-end bookmark for %s within %s",
				errStreamFallback, gvr.String(), timeout)
		case ev, ok := <-w.ResultChan():
			if !ok {
				return nil, "", fmt.Errorf("%w: watch closed before initial-events-end bookmark for %s",
					errStreamFallback, gvr.String())
			}
			rv, done, ferr := foldStreamEvent(items, gvr, ev, log)
			if ferr != nil {
				return nil, "", ferr
			}
			if done {
				return items, rv, nil
			}
		}
	}
}

// foldStreamEvent applies one streaming-list event to the checkpoint accumulator. It returns
// done=true with the bookmark's resourceVersion when the terminal initial-events-end bookmark
// arrives; an errStreamFallback-wrapped error when the stream is untrustworthy (a non-object
// payload or a watch.Error event). A per-object marshal failure is logged and skipped, matching
// the LIST path, rather than abandoning the whole watch.
func foldStreamEvent(
	items map[string]string, gvr schema.GroupVersionResource, ev watch.Event, log logr.Logger,
) (string, bool, error) {
	switch ev.Type {
	case watch.Bookmark:
		u, ok := ev.Object.(*unstructured.Unstructured)
		if !ok {
			return "", false, fmt.Errorf("%w: bookmark carried %T for %s",
				errStreamFallback, ev.Object, gvr.String())
		}
		if u.GetAnnotations()[metav1.InitialEventsAnnotationKey] == "true" {
			return u.GetResourceVersion(), true, nil
		}
		// An interim progress bookmark: the initial set is not complete yet, keep reading.
		return "", false, nil
	case watch.Added, watch.Modified:
		u, ok := ev.Object.(*unstructured.Unstructured)
		if !ok {
			return "", false, fmt.Errorf("%w: initial event carried %T for %s",
				errStreamFallback, ev.Object, gvr.String())
		}
		raw, mErr := objectEnvelopeJSON(gvr, u)
		if mErr != nil {
			log.Error(mErr, "objects-mirror: marshal failed", "gvr", gvr.String(), "object", objectIdentity(u))
			return "", false, nil
		}
		items[objectIdentity(u)] = raw
		return "", false, nil
	case watch.Deleted:
		// Streaming initial events are ADDED-only; a delete before the bookmark is unexpected but
		// harmless to honor — drop it so the accumulated set stays consistent.
		if u, ok := ev.Object.(*unstructured.Unstructured); ok {
			delete(items, objectIdentity(u))
		}
		return "", false, nil
	case watch.Error:
		return "", false, fmt.Errorf("%w: watch error event for %s: %v",
			errStreamFallback, gvr.String(), ev.Object)
	default: // an unrecognized event type — the initial set is untrustworthy.
		return "", false, fmt.Errorf("%w: unexpected watch event %q for %s",
			errStreamFallback, ev.Type, gvr.String())
	}
}

// openCheckpointWatch opens the streaming-list watch for the checkpoint fill, honoring the
// m.watchCheckpointObjects test seam when set (the dynamic fake client does not implement
// streaming-list). Production builds the watch from the dynamic client.
func (m *Manager) openCheckpointWatch(
	ctx context.Context, dc dynamic.Interface, gvr schema.GroupVersionResource, opts metav1.ListOptions,
) (watch.Interface, error) {
	if m.watchCheckpointObjects != nil {
		return m.watchCheckpointObjects(ctx, gvr, opts)
	}
	return dc.Resource(gvr).Watch(ctx, opts)
}

// listTypeObjects is the consistent-LIST fallback (and the original checkpoint behavior): one
// dynamic LIST of the whole type, folded into the same envelope map, pinned to the collection
// resourceVersion. Empty namespace lists across all namespaces (and is correct for
// cluster-scoped types).
func (m *Manager) listTypeObjects(
	ctx context.Context, log logr.Logger, dc dynamic.Interface, gvr schema.GroupVersionResource,
) (map[string]string, string, error) {
	list, err := dc.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("objects-mirror: list %s: %w", gvr.String(), err)
	}
	items := make(map[string]string, len(list.Items))
	for i := range list.Items {
		obj := &list.Items[i]
		raw, err := objectEnvelopeJSON(gvr, obj)
		if err != nil {
			log.Error(err, "objects-mirror: marshal failed",
				"gvr", gvr.String(), "object", objectIdentity(obj))
			continue
		}
		items[objectIdentity(obj)] = raw
	}
	return items, list.GetResourceVersion(), nil
}

// clearTypeObjects drops a removed type's stored object snapshot. Best-effort like the load.
func (m *Manager) clearTypeObjects(ctx context.Context, log logr.Logger, gvr schema.GroupVersionResource) {
	if m.ObjectMirror == nil {
		return
	}
	if err := m.ObjectMirror.DeleteTypeObjects(ctx, gvr.Group, gvr.Resource); err != nil {
		log.Error(err, "objects-mirror: delete failed", "gvr", gvr.String())
		return
	}
	log.Info("objects-mirror: snapshot cleared", "gvr", gvr.String())
}

// objectEnvelopeJSON builds the stored value for one object: its identity, resourceVersion,
// and generation (read from the original object, since sanitize strips them) wrapped around
// the sanitized body. The GVR supplies the group/version/resource so each entry is
// self-describing without consulting its key.
func objectEnvelopeJSON(gvr schema.GroupVersionResource, obj *unstructured.Unstructured) (string, error) {
	body, err := sanitize.Sanitize(obj).MarshalJSON()
	if err != nil {
		return "", fmt.Errorf("marshal sanitized object: %w", err)
	}
	raw, err := json.Marshal(objectEnvelope{
		APIGroup:        gvr.Group,
		APIVersion:      gvr.Version,
		Resource:        gvr.Resource,
		Kind:            obj.GetKind(),
		Namespace:       obj.GetNamespace(),
		Name:            obj.GetName(),
		UID:             string(obj.GetUID()),
		ResourceVersion: obj.GetResourceVersion(),
		Generation:      obj.GetGeneration(),
		Object:          body,
	})
	if err != nil {
		return "", fmt.Errorf("marshal object envelope: %w", err)
	}
	return string(raw), nil
}

// objectIdentity is the per-object hash field: "<namespace>/<name>" for namespaced objects,
// or just "<name>" for cluster-scoped ones.
func objectIdentity(obj *unstructured.Unstructured) string {
	if ns := obj.GetNamespace(); ns != "" {
		return ns + "/" + obj.GetName()
	}
	return obj.GetName()
}
