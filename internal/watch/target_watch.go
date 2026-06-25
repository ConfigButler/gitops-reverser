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
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/utils/ptr"

	configv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

const targetWatchBackoff = 2 * time.Second

var errTargetWatchClosed = errors.New("target watch result channel closed")

type targetWatchSet struct {
	cancel context.CancelFunc
	specs  map[targetWatchKey]string
}

type targetWatchKey struct {
	GVR       schema.GroupVersionResource
	Namespace string
}

// EnsureGitTargetWatches makes the GitTarget's raw watch set match its current
// claimed, followable (GVR, scope) table. Each watch replays current state with
// sendInitialEvents, performs a scoped mark-and-sweep at the initial-events-end
// bookmark, then streams live object events to the GitTargetEventStream.
func (m *Manager) EnsureGitTargetWatches(ctx context.Context, gitDest types.ResourceReference) error {
	if m.EventRouter == nil {
		return nil
	}
	if err := m.RefreshAPIResourceCatalog(ctx); err != nil {
		return fmt.Errorf("refresh API resource catalog for %s: %w", gitDest.String(), err)
	}
	m.refreshWatchedTypeTables()
	if !m.typeRegistryInstance().Ready() {
		return fmt.Errorf("aborting watch setup for %s: the cluster API surface has not been observed yet",
			gitDest.String())
	}

	table := m.residentWatchedTypeTable(gitDest)
	if retained := m.retainedWatchedTypes(table); len(retained) > 0 {
		return fmt.Errorf("aborting watch setup for %s: %s within the removal grace (currently unserved)",
			gitDest.String(), gvkListSummary(retained))
	}
	return m.replaceGitTargetWatches(ctx, table)
}

func (m *Manager) replaceGitTargetWatches(ctx context.Context, table WatchedTypeTable) error {
	specs := targetWatchSpecs(table)
	keys := sortedTargetWatchSpecKeys(specs)
	childCtx, cancel := context.WithCancel(ctx)

	m.targetWatchesMu.Lock()
	if m.targetWatches == nil {
		m.targetWatches = map[string]*targetWatchSet{}
	}
	key := table.GitDest.Key()
	if prior := m.targetWatches[key]; prior != nil {
		if equalTargetWatchSpecs(prior.specs, specs) {
			m.targetWatchesMu.Unlock()
			cancel()
			return nil
		}
		prior.cancel()
	}
	m.targetWatches[key] = &targetWatchSet{cancel: cancel, specs: specs}
	m.targetWatchesMu.Unlock()

	log := m.Log.WithName("target-watch").WithValues("gitDest", table.GitDest.String())
	for _, watchKey := range keys {
		ops := table.operationsFor(watchKey)
		go m.runTargetWatch(childCtx, log, table.GitDest, watchKey, ops)
	}
	log.Info("watch-first target watch set reconciled", "watchCount", len(keys))
	return nil
}

func (m *Manager) refreshRunningTargetWatches(ctx context.Context) {
	m.targetWatchesMu.Lock()
	running := make(map[string]struct{}, len(m.targetWatches))
	for key := range m.targetWatches {
		running[key] = struct{}{}
	}
	m.targetWatchesMu.Unlock()
	if len(running) == 0 {
		return
	}
	for _, table := range m.residentWatchedTypeTables() {
		if _, ok := running[table.GitDest.Key()]; !ok {
			continue
		}
		if err := m.replaceGitTargetWatches(ctx, table); err != nil {
			m.Log.Error(err, "refresh running GitTarget watches failed", "gitDest", table.GitDest.String())
		}
	}
}

func (m *Manager) forgetGitTargetWatches(gitDest types.ResourceReference) {
	m.targetWatchesMu.Lock()
	defer m.targetWatchesMu.Unlock()
	if set := m.targetWatches[gitDest.Key()]; set != nil {
		set.cancel()
		delete(m.targetWatches, gitDest.Key())
	}
}

func targetWatchSpecs(table WatchedTypeTable) map[targetWatchKey]string {
	out := map[targetWatchKey]string{}
	for _, wt := range table.Types {
		namespaces := wt.SnapshotNamespaces()
		if len(namespaces) == 0 {
			key := targetWatchKey{GVR: wt.GVR}
			out[key] = operationSpec(wt.NamespaceOps[""])
			continue
		}
		for _, ns := range namespaces {
			key := targetWatchKey{GVR: wt.GVR, Namespace: ns}
			out[key] = operationSpec(wt.NamespaceOps[ns])
		}
	}
	return out
}

func sortedTargetWatchSpecKeys(specs map[targetWatchKey]string) []targetWatchKey {
	out := make([]targetWatchKey, 0, len(specs))
	for key := range specs {
		out = append(out, key)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GVR.String() == out[j].GVR.String() {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].GVR.String() < out[j].GVR.String()
	})
	return out
}

func operationSpec(ops OperationSet) string {
	if len(ops) == 0 {
		return "*"
	}
	return fmt.Sprint(ops.Sorted())
}

func equalTargetWatchSpecs(a, b map[targetWatchKey]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, av := range a {
		if b[key] != av {
			return false
		}
	}
	return true
}

func (t WatchedTypeTable) operationsFor(key targetWatchKey) OperationSet {
	for _, wt := range t.Types {
		if wt.GVR != key.GVR {
			continue
		}
		if ops := wt.NamespaceOps[key.Namespace]; ops != nil {
			return ops
		}
		if key.Namespace != "" {
			return wt.NamespaceOps[""]
		}
	}
	return nil
}

func (m *Manager) runTargetWatch(
	ctx context.Context,
	log logr.Logger,
	gitDest types.ResourceReference,
	key targetWatchKey,
	ops OperationSet,
) {
	for ctx.Err() == nil {
		err := m.targetWatchReplayAndStream(ctx, log, gitDest, key, ops)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Info("target watch session ended; reconnecting",
				"gvr", key.GVR.String(), "namespace", key.Namespace, "err", err.Error())
		}
		if !sleepOrDone(ctx, targetWatchBackoff) {
			return
		}
	}
}

func (m *Manager) targetWatchReplayAndStream(
	ctx context.Context,
	log logr.Logger,
	gitDest types.ResourceReference,
	key targetWatchKey,
	ops OperationSet,
) error {
	opts := metav1.ListOptions{
		SendInitialEvents:    ptr.To(true),
		ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
		AllowWatchBookmarks:  true,
	}
	replaying := true
	w, err := m.openTargetWatch(ctx, key.GVR, key.Namespace, opts)
	if err != nil {
		if watchListForbidden(err) {
			log.Info("target watch replay unsupported; falling back to live watch",
				"gvr", key.GVR.String(), "namespace", key.Namespace, "err", err.Error())
			w, err = m.openTargetWatch(ctx, key.GVR, key.Namespace, metav1.ListOptions{
				AllowWatchBookmarks: true,
			})
			replaying = false
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("open target watch %s/%q: %w", key.GVR.String(), key.Namespace, err)
		}
	}
	defer w.Stop()

	var replay []manifestanalyzer.DesiredResource
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.ResultChan():
			if !ok {
				return errTargetWatchClosed
			}
			nextReplaying, err := m.handleTargetWatchSessionEvent(
				ctx, log, gitDest, key, ops, ev, replaying, &replay,
			)
			if err != nil {
				return err
			}
			replaying = nextReplaying
		}
	}
}

func (m *Manager) handleTargetWatchSessionEvent(
	ctx context.Context,
	log logr.Logger,
	gitDest types.ResourceReference,
	key targetWatchKey,
	ops OperationSet,
	ev watch.Event,
	replaying bool,
	replay *[]manifestanalyzer.DesiredResource,
) (bool, error) {
	if !replaying {
		return false, m.routeLiveTargetWatchEvent(log, gitDest, key, ops, ev)
	}
	done, rv, err := m.foldTargetReplayEvent(log, gitDest, key, ev, replay)
	if err != nil || !done {
		return true, err
	}
	if err := m.enqueueReplayResync(ctx, log, gitDest, key.GVR, *replay, rv); err != nil {
		return true, err
	}
	*replay = nil
	return false, nil
}

func (m *Manager) foldTargetReplayEvent(
	log logr.Logger,
	gitDest types.ResourceReference,
	key targetWatchKey,
	ev watch.Event,
	replay *[]manifestanalyzer.DesiredResource,
) (bool, string, error) {
	switch ev.Type {
	case watch.Bookmark:
		u, ok := ev.Object.(*unstructured.Unstructured)
		if !ok {
			return false, "", fmt.Errorf("target replay bookmark carried %T for %s", ev.Object, key.GVR.String())
		}
		if u.GetAnnotations()[metav1.InitialEventsAnnotationKey] != "true" {
			return false, "", nil
		}
		log.Info("target watch replay complete",
			"gitDest", gitDest.String(), "gvr", key.GVR.String(), "namespace", key.Namespace,
			"count", len(*replay), "resourceVersion", u.GetResourceVersion())
		return true, u.GetResourceVersion(), nil
	case watch.Added, watch.Modified:
		u, ok := ev.Object.(*unstructured.Unstructured)
		if !ok {
			return false, "", fmt.Errorf("target replay event carried %T for %s", ev.Object, key.GVR.String())
		}
		if desired, ok := desiredFromObject(key.GVR, u); ok {
			*replay = append(*replay, desired)
		}
		return false, "", nil
	case watch.Deleted:
		return false, "", nil
	case watch.Error:
		return false, "", fmt.Errorf("target replay watch error for %s: %v", key.GVR.String(), ev.Object)
	default:
		return false, "", nil
	}
}

func (m *Manager) enqueueReplayResync(
	ctx context.Context,
	log logr.Logger,
	gitDest types.ResourceReference,
	gvr schema.GroupVersionResource,
	desired []manifestanalyzer.DesiredResource,
	revision string,
) error {
	if m.EventRouter == nil {
		return nil
	}
	resultCh, enqueued, err := m.EventRouter.enqueueScopedResync(ctx, gitDest, gvr, desired, revision, false)
	if err != nil {
		return err
	}
	if !enqueued {
		return fmt.Errorf("target replay resync for %s on %s dropped: %w",
			gvr.String(), gitDest.String(), git.ErrFinalizeQueueFull)
	}
	go m.EventRouter.drainScopedResync(gitDest, gvr, "reconcile", resultCh)
	log.V(1).Info("target replay resync enqueued",
		"gitDest", gitDest.String(), "gvr", gvr.String(), "revision", revision, "count", len(desired))
	return nil
}

func watchListForbidden(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "sendInitialEvents") && strings.Contains(msg, "Forbidden")
}

func (m *Manager) routeLiveTargetWatchEvent(
	log logr.Logger,
	gitDest types.ResourceReference,
	key targetWatchKey,
	ops OperationSet,
	ev watch.Event,
) error {
	switch ev.Type {
	case watch.Bookmark:
		return nil
	case watch.Added, watch.Modified, watch.Deleted:
		op := operationForWatchEvent(ev.Type)
		if !ops.Match(op) {
			return nil
		}
		u, ok := ev.Object.(*unstructured.Unstructured)
		if !ok {
			log.V(1).Info("target watch non-unstructured event skipped",
				"gvr", key.GVR.String(), "type", string(ev.Type))
			return nil
		}
		event := targetWatchGitEvent(key.GVR, u, op)
		if err := m.EventRouter.RouteToGitTargetEventStream(event, gitDest); err != nil {
			log.V(1).Info("target watch route failed",
				"gitDest", gitDest.String(), "gvr", key.GVR.String(), "err", err.Error())
		}
		return nil
	case watch.Error:
		return fmt.Errorf("target watch error for %s: %v", key.GVR.String(), ev.Object)
	default:
		return nil
	}
}

func targetWatchGitEvent(gvr schema.GroupVersionResource, u *unstructured.Unstructured, op string) git.Event {
	event := git.Event{
		Identifier: types.NewResourceIdentifier(gvr.Group, gvr.Version, gvr.Resource, u.GetNamespace(), u.GetName()),
		Operation:  op,
	}
	if op != string(configv1alpha2.OperationDelete) {
		event.Object = sanitize.Sanitize(u)
	}
	return event
}

func operationForWatchEvent(eventType watch.EventType) string {
	switch eventType {
	case watch.Added:
		return string(configv1alpha2.OperationCreate)
	case watch.Modified:
		return string(configv1alpha2.OperationUpdate)
	case watch.Deleted:
		return string(configv1alpha2.OperationDelete)
	case watch.Bookmark, watch.Error:
		return ""
	default:
		return ""
	}
}

// Match reports whether the operation is included in the operation set. A nil or
// empty set means all operations, matching WatchRule semantics.
func (s OperationSet) Match(op string) bool {
	if len(s) == 0 {
		return true
	}
	if _, ok := s["*"]; ok {
		return true
	}
	_, ok := s[op]
	return ok
}

func (m *Manager) openTargetWatch(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	namespace string,
	opts metav1.ListOptions,
) (watch.Interface, error) {
	if m.targetWatchOpen != nil {
		return m.targetWatchOpen(ctx, gvr, namespace, opts)
	}
	dc := m.dynamicClientFromConfig(m.Log)
	if dc == nil {
		return nil, errors.New("no dynamic client for target watch")
	}
	resource := dc.Resource(gvr)
	if namespace != "" {
		return resource.Namespace(namespace).Watch(ctx, opts)
	}
	return resource.Watch(ctx, opts)
}

func sleepOrDone(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
