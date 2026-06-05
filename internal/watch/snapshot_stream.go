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
	"strconv"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// ClusterSnapshot is one consistent, revision-pinned view of every watched resource a
// GitTarget tracks. It is produced by the streaming-list watch and consumed by the
// resync mark-and-sweep: Desired is the complete set the worker folds over the git
// folder; Revision is the joined initial-events-end bookmark resourceVersion the whole
// snapshot is pinned to.
type ClusterSnapshot struct {
	Desired  []manifestanalyzer.DesiredResource
	Revision string
}

// StreamClusterSnapshotForGitDest gathers the GitTarget's complete watched resource set
// via the Kubernetes streaming-list watch (WATCH with sendInitialEvents=true,
// resourceVersionMatch=NotOlderThan, allowWatchBookmarks=true), described in
// docs/design/manifest/reconcile-via-watchlist-mark-and-sweep.md.
//
// Each watched (GVR, namespace scope) opens its own stream: every initial ADDED event
// is folded into the desired set, and the stream is read until its initial-events-end
// bookmark, which pins that type's resourceVersion. The snapshot is the JOIN of all
// streams' bookmarks — and only the join. If any stream errors or closes before its
// bookmark, the whole gather ABORTS and returns nothing: a partial mark must never
// drive a sweep (the same fail-closed rule the old LIST snapshot used).
//
// Streaming is the primary path. The one concession is a per-type consistent LIST
// fallback (streamInitialEvents → listInitialEvents) for a server that cannot stream at
// all — aggregated apiservers reject sendInitialEvents — so a single non-streaming type
// no longer aborts the whole snapshot. This is NOT a return to the old LIST+WATCH
// steady state (the informers still own live events); a transient watch error still
// aborts, never silently turning an unobservable surface into an empty (destructive)
// snapshot.
//
// An empty desired set is authoritative only because it can only be produced when
// every stream reached its bookmark — the cluster genuinely holds no watched
// resources, so the mirror is swept clean to match.
func (m *Manager) StreamClusterSnapshotForGitDest(
	ctx context.Context,
	gitDest types.ResourceReference,
) (ClusterSnapshot, error) {
	log := m.Log.WithValues("gitDest", gitDest.String())

	gvrs, err := m.resolveSnapshotGVRs(ctx, gitDest)
	if err != nil {
		return ClusterSnapshot{}, err
	}

	dc := m.dynamicClientFromConfig(log)
	if dc == nil {
		return ClusterSnapshot{}, errors.New("no dynamic client available")
	}

	tasks := snapshotStreamTasks(gvrs)
	if len(tasks) == 0 {
		log.Info("Streamed cluster snapshot", "resources", 0, "streams", 0)
		return ClusterSnapshot{}, nil
	}

	desired, revision, err := m.joinSnapshotStreams(ctx, dc, tasks)
	if err != nil {
		return ClusterSnapshot{}, fmt.Errorf(
			"aborting streaming snapshot for %s: %w; refusing to sweep on a partial stream",
			gitDest.String(), err)
	}

	log.Info("Streamed cluster snapshot", "resources", len(desired), "streams", len(tasks), "revision", revision)
	return ClusterSnapshot{Desired: desired, Revision: revision}, nil
}

// joinSnapshotStreams runs every stream concurrently and joins them at their bookmarks.
// The first stream to fail cancels the rest (so a doomed gather stops promptly) and the
// failure is returned; otherwise the desired sets are unioned and the revision is the
// max bookmark resourceVersion across types.
func (m *Manager) joinSnapshotStreams(
	ctx context.Context,
	dc dynamic.Interface,
	tasks []snapshotStreamTask,
) ([]manifestanalyzer.DesiredResource, string, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		mu       sync.Mutex
		desired  []manifestanalyzer.DesiredResource
		maxRV    string
		firstErr error
		wg       sync.WaitGroup
	)
	for _, task := range tasks {
		wg.Add(1)
		go func(task snapshotStreamTask) {
			defer wg.Done()
			items, rv, err := m.streamInitialEvents(streamCtx, dc, task.gvr, task.namespace)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
					cancel() // abort peers: a partial mark must never drive a sweep
				}
				return
			}
			desired = append(desired, items...)
			if maxResourceVersion(rv, maxRV) == rv {
				maxRV = rv
			}
		}(task)
	}
	wg.Wait()

	if firstErr != nil {
		return nil, "", firstErr
	}
	return desired, maxRV, nil
}

// snapshotStreamTask is one stream to open: a resolved GVR scoped either cluster-wide
// (namespace == "") or to a single namespace.
type snapshotStreamTask struct {
	gvr       schema.GroupVersionResource
	namespace string
}

// snapshotStreamTasks expands the resolved watched types into one task per stream: a
// cluster-wide type is one stream; a namespaced type is one stream per watched
// namespace, matching how the resolver scoped it.
func snapshotStreamTasks(gvrs []snapshotGVR) []snapshotStreamTask {
	var tasks []snapshotStreamTask
	for _, sg := range gvrs {
		if len(sg.namespaces) == 0 {
			tasks = append(tasks, snapshotStreamTask{gvr: sg.gvr})
			continue
		}
		for _, ns := range sg.namespaces {
			tasks = append(tasks, snapshotStreamTask{gvr: sg.gvr, namespace: ns})
		}
	}
	return tasks
}

// streamInitialEvents opens one streaming-list watch and folds its initial ADDED events
// into desired resources, returning once the initial-events-end bookmark arrives with
// the resourceVersion the set is consistent at. A closed channel or watch.Error before
// the bookmark is a failure — the initial sync did not complete — so the caller aborts
// rather than treating a truncated set as the cluster's full state.
//
// Streaming is the primary path, but a server that cannot stream initial events (most
// commonly an aggregated apiserver, which rejects sendInitialEvents outright) falls back
// to one consistent LIST at the latest revision for THIS type only. This is the design's
// per-type "availability fallback": it is not a return to LIST+WATCH steady state — the
// informers still own live events — just a consistent initial snapshot for a type that
// cannot stream one.
func (m *Manager) streamInitialEvents(
	ctx context.Context,
	dc dynamic.Interface,
	gvr schema.GroupVersionResource,
	namespace string,
) ([]manifestanalyzer.DesiredResource, string, error) {
	ri := resourceInterfaceFor(dc, gvr, namespace)

	w, err := ri.Watch(ctx, streamingListOptions())
	if err != nil {
		if isStreamingWatchUnsupported(err) {
			m.Log.V(1).Info("server cannot stream initial events; falling back to a consistent list",
				"gvr", gvr.String(), "namespace", namespace, "reason", err.Error())
			return listInitialEvents(ctx, ri, gvr)
		}
		return nil, "", fmt.Errorf("open streaming watch for %s: %w", gvr.String(), err)
	}
	defer w.Stop()

	var desired []manifestanalyzer.DesiredResource
	for {
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case event, ok := <-w.ResultChan():
			if !ok {
				return nil, "", fmt.Errorf(
					"streaming watch for %s closed before the initial-events-end bookmark", gvr.String())
			}
			switch event.Type {
			case watch.Added:
				if dr, ok := desiredFromObject(gvr, event.Object); ok {
					desired = append(desired, dr)
				}
			case watch.Bookmark:
				if rv, done := initialEventsEndRevision(event.Object); done {
					return desired, rv, nil
				}
			case watch.Error:
				return nil, "", fmt.Errorf(
					"streaming watch for %s returned an error event: %v", gvr.String(), event.Object)
			case watch.Modified, watch.Deleted:
				// Live changes that race the initial sync arrive after the bookmark; any
				// before it would only refine an object we are about to re-list, so the
				// pre-bookmark set stays the authoritative initial view.
			}
		}
	}
}

// streamingListOptions is the watch request that asks the API server to replay every
// existing object as a synthetic ADDED event, then a bookmark, then live changes.
func streamingListOptions() metav1.ListOptions {
	sendInitialEvents := true
	return metav1.ListOptions{
		AllowWatchBookmarks:  true,
		SendInitialEvents:    &sendInitialEvents,
		ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
		ResourceVersion:      "",
	}
}

// resourceInterfaceFor scopes a dynamic resource client to a namespace, or leaves it
// cluster-wide when namespace is empty.
func resourceInterfaceFor(
	dc dynamic.Interface,
	gvr schema.GroupVersionResource,
	namespace string,
) dynamic.ResourceInterface {
	if namespace != "" {
		return dc.Resource(gvr).Namespace(namespace)
	}
	return dc.Resource(gvr)
}

// isStreamingWatchUnsupported reports whether a Watch error means the server cannot
// serve a streaming-list watch (so a consistent LIST is the right per-type fallback),
// as opposed to a transient failure that must abort the snapshot. The options carry
// nothing but the streaming fields, so an Invalid/BadRequest from this call is about
// sendInitialEvents; the message check is a version-robust backstop.
func isStreamingWatchUnsupported(err error) bool {
	if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "sendInitialEvents") || strings.Contains(msg, "WatchList")
}

// listInitialEvents is the per-type fallback: one consistent LIST at the server's latest
// revision, folded into the desired set the same way the stream's ADDED events are. The
// list's own resourceVersion pins this type's contribution to the snapshot.
func listInitialEvents(
	ctx context.Context,
	ri dynamic.ResourceInterface,
	gvr schema.GroupVersionResource,
) ([]manifestanalyzer.DesiredResource, string, error) {
	list, err := ri.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("fallback list for %s: %w", gvr.String(), err)
	}
	var desired []manifestanalyzer.DesiredResource
	for i := range list.Items {
		if dr, ok := desiredFromObject(gvr, &list.Items[i]); ok {
			desired = append(desired, dr)
		}
	}
	return desired, list.GetResourceVersion(), nil
}

// desiredFromObject converts a streamed object into a desired resource, pairing the
// GVR-derived API identity with the sanitized object the writer will materialise.
func desiredFromObject(
	gvr schema.GroupVersionResource,
	obj interface{},
) (manifestanalyzer.DesiredResource, bool) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok || u == nil {
		return manifestanalyzer.DesiredResource{}, false
	}
	id := types.NewResourceIdentifier(gvr.Group, gvr.Version, gvr.Resource, u.GetNamespace(), u.GetName())
	return manifestanalyzer.DesiredResource{Resource: id, Object: sanitize.Sanitize(u)}, true
}

// initialEventsEndRevision reports whether a bookmark event marks the end of the
// initial sync, and the resourceVersion it pins. Bookmarks without the annotation are
// ordinary progress notifications and are ignored.
func initialEventsEndRevision(obj interface{}) (string, bool) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok || u == nil {
		return "", false
	}
	if u.GetAnnotations()[metav1.InitialEventsAnnotationKey] != "true" {
		return "", false
	}
	return u.GetResourceVersion(), true
}

// maxResourceVersion returns the larger of two resourceVersions, comparing numerically
// when both parse (etcd resourceVersions are monotonic integers) and falling back to
// the non-empty one otherwise. The pinned revision is informational here — the plan is
// applied at the worktree commit — so a best-effort max is sufficient.
func maxResourceVersion(a, b string) string {
	if b == "" {
		return a
	}
	if a == "" {
		return b
	}
	ai, aerr := strconv.ParseUint(a, 10, 64)
	bi, berr := strconv.ParseUint(b, 10, 64)
	if aerr == nil && berr == nil {
		if ai >= bi {
			return a
		}
		return b
	}
	if a >= b {
		return a
	}
	return b
}

// snapshotGVR is one resolved watched resource type with the namespace scope to gather
// it under: an empty namespaces slice means cluster-wide.
type snapshotGVR struct {
	gvr        schema.GroupVersionResource
	namespaces []string
}

// gvrSnapshotEntry accumulates the namespace scope for a GVR while resolving rules. A
// cluster-wide entry overrides any namespaces (a cluster-wide stream sees them all).
type gvrSnapshotEntry struct {
	namespaces  map[string]struct{}
	clusterWide bool
}

// resolveSnapshotGVRs resolves the GitTarget's WatchRules and ClusterWatchRules into the
// concrete watched (GVR, namespace-scope) set to stream. It refreshes the trusted API
// catalog first and fails closed on a blocking resolve miss (catalog unavailable or
// discovery degraded), so a snapshot is never built from partial knowledge of the API
// surface.
func (m *Manager) resolveSnapshotGVRs(
	ctx context.Context,
	gitDest types.ResourceReference,
) ([]snapshotGVR, error) {
	if err := m.RefreshAPIResourceCatalog(ctx); err != nil {
		return nil, fmt.Errorf("refresh API resource catalog for %s: %w", gitDest.String(), err)
	}
	resolver := m.ruleGVRResolver()
	gvrMap := map[schema.GroupVersionResource]*gvrSnapshotEntry{}

	blockingMisses := m.collectWatchRuleGVRs(gitDest, resolver, gvrMap)
	blockingMisses = append(blockingMisses, m.collectClusterWatchRuleGVRs(gitDest, resolver, gvrMap)...)

	if len(blockingMisses) > 0 {
		return nil, fmt.Errorf(
			"aborting cluster snapshot for %s: %s; refusing to snapshot a partial cluster view",
			gitDest.String(), FormatResolveMisses(blockingMisses))
	}

	return sortedSnapshotGVRs(gvrMap), nil
}

// collectWatchRuleGVRs folds this GitTarget's namespaced WatchRules into gvrMap,
// scoping each resolved GVR to its rule's namespace, and returns any blocking misses.
func (m *Manager) collectWatchRuleGVRs(
	gitDest types.ResourceReference,
	resolver *RuleGVRResolver,
	gvrMap map[schema.GroupVersionResource]*gvrSnapshotEntry,
) []ResolveMiss {
	var blockingMisses []ResolveMiss
	for _, rule := range m.RuleStore.SnapshotWatchRules() {
		if rule.GitTargetRef != gitDest.Name || rule.GitTargetNamespace != gitDest.Namespace {
			continue
		}
		for _, rr := range rule.ResourceRules {
			gvrs, miss := m.gvrsFromResourceRule(rr, resolver)
			blockingMisses = append(blockingMisses, blockingSnapshotMisses(miss)...)
			for _, gvr := range gvrs {
				entry := ensureSnapshotEntry(gvrMap, gvr.schema())
				if !entry.clusterWide {
					entry.namespaces[rule.Source.Namespace] = struct{}{}
				}
			}
		}
	}
	return blockingMisses
}

// collectClusterWatchRuleGVRs folds this GitTarget's ClusterWatchRules into gvrMap as
// cluster-wide entries, and returns any blocking misses.
func (m *Manager) collectClusterWatchRuleGVRs(
	gitDest types.ResourceReference,
	resolver *RuleGVRResolver,
	gvrMap map[schema.GroupVersionResource]*gvrSnapshotEntry,
) []ResolveMiss {
	var blockingMisses []ResolveMiss
	for _, cwrRule := range m.RuleStore.SnapshotClusterWatchRules() {
		if cwrRule.GitTargetRef != gitDest.Name || cwrRule.GitTargetNamespace != gitDest.Namespace {
			continue
		}
		gvrs, miss := m.gvrsFromClusterRule(cwrRule, resolver)
		blockingMisses = append(blockingMisses, blockingSnapshotMisses(miss)...)
		for _, gvr := range gvrs {
			ensureSnapshotEntry(gvrMap, gvr.schema()).clusterWide = true
		}
	}
	return blockingMisses
}

// ensureSnapshotEntry returns the entry for a GVR, creating it on first sight.
func ensureSnapshotEntry(
	gvrMap map[schema.GroupVersionResource]*gvrSnapshotEntry,
	gvr schema.GroupVersionResource,
) *gvrSnapshotEntry {
	entry := gvrMap[gvr]
	if entry == nil {
		entry = &gvrSnapshotEntry{namespaces: map[string]struct{}{}}
		gvrMap[gvr] = entry
	}
	return entry
}

// sortedSnapshotGVRs converts the resolved entries into a deterministic, sorted slice so
// the gathered snapshot and its diagnostics are stable across runs. A cluster-wide entry
// yields no namespaces (it is streamed cluster-wide).
func sortedSnapshotGVRs(gvrMap map[schema.GroupVersionResource]*gvrSnapshotEntry) []snapshotGVR {
	out := make([]snapshotGVR, 0, len(gvrMap))
	for gvr, entry := range gvrMap {
		var namespaces []string
		if !entry.clusterWide {
			for ns := range entry.namespaces {
				namespaces = append(namespaces, ns)
			}
			sort.Strings(namespaces)
		}
		out = append(out, snapshotGVR{gvr: gvr, namespaces: namespaces})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].gvr.String() < out[j].gvr.String()
	})
	return out
}
