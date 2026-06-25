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
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// lateNudgeMinInterval is retained only for the obsolete late-event hook's
// rate-limit. Watch-first state capture does not call this path.
const lateNudgeMinInterval = 15 * time.Second

// NudgeTypeResyncForLateEvent is a no-op compatibility hook for older wiring.
// Watch-first ingestion consumes Kubernetes WATCH as the state source, so a late
// audit event no longer drives correctness or a resync.
func (m *Manager) NudgeTypeResyncForLateEvent(group, resource string) {
	gvr, ok := m.claimedGVRForGroupResource(group, resource)
	if !ok {
		return
	}

	m.lateNudgeMu.Lock()
	now := time.Now()
	if m.lateNudgeAt == nil {
		m.lateNudgeAt = make(map[schema.GroupVersionResource]time.Time)
	}
	limited := now.Sub(m.lateNudgeAt[gvr]) < lateNudgeMinInterval
	if !limited {
		m.lateNudgeAt[gvr] = now
	}
	m.lateNudgeMu.Unlock()
	if !limited {
		m.Log.V(1).Info("ignored late audit event nudge in watch-first mode", "gvr", gvr.String())
	}
}

func (m *Manager) claimedGVRForGroupResource(group, resource string) (schema.GroupVersionResource, bool) {
	for _, rec := range m.typeRegistryInstance().ByGroupResource(group, resource) {
		gvr := rec.Identity.GVR
		if len(m.materializerInstance().Claimants(gvr)) > 0 {
			return gvr, true
		}
	}
	return schema.GroupVersionResource{}, false
}

// materializerInstance returns the lazily-built materializer retained for
// compatibility with old tests and exported restore hooks. It is no longer on
// the active watch-first data path.
func (m *Manager) materializerInstance() *typeset.Materializer {
	m.materializerInit.Do(func() {
		if m.materializer == nil {
			m.materializer = typeset.NewMaterializer()
		}
	})
	return m.materializer
}

// DeclareForGitTarget ensures the GitTarget's watch-first data plane is running.
func (m *Manager) DeclareForGitTarget(ctx context.Context, gitDest types.ResourceReference) error {
	if err := m.EnsureGitTargetWatches(ctx, gitDest); err != nil {
		m.Log.Info("watch-first declare skipped; surface not observable",
			"gitDest", gitDest.String(), "err", err.Error())
		return err
	}
	return nil
}

// ForgetGitTargetDeclaration drops in-memory watch state for a deleted GitTarget.
func (m *Manager) ForgetGitTargetDeclaration(gitDest types.ResourceReference) {
	m.forgetGitTargetWatches(gitDest)
	m.declaredGVRsMu.Lock()
	defer m.declaredGVRsMu.Unlock()
	delete(m.declaredGVRs, gitDest.String())
}

// RestoreSyncedCheckpoint is kept as a harmless compatibility hook for old
// composition roots/tests. Watch-first no longer restores Redis checkpoints at
// startup.
func (m *Manager) RestoreSyncedCheckpoint(group, version, resource, rv string) {
	if resource == "" || rv == "" {
		return
	}
	m.materializerInstance().RestoreSynced(
		schema.GroupVersionResource{Group: group, Version: version, Resource: resource}, rv)
}

// GitTargetMaterializationSummary is a bounded per-GitTarget roll-up kept for
// the existing status shape. In watch-first mode Claimed is the resolved
// followable watch set and Synced means its watches are declared.
type GitTargetMaterializationSummary struct {
	Claimed             int
	Synced              int
	Pending             int
	Failing             int
	NotFollowable       int
	FailingNoCheckpoint int
}

// MaterializationSummaryForGitTarget reports the GitTarget's current watch set
// using the legacy status fields.
func (m *Manager) MaterializationSummaryForGitTarget(gitDest types.ResourceReference) GitTargetMaterializationSummary {
	table, ok := m.watchedTypeTableForGitDest(gitDest)
	if !ok {
		return GitTargetMaterializationSummary{}
	}
	s := GitTargetMaterializationSummary{Claimed: len(table.Types), Synced: len(table.Types)}
	for _, wt := range table.Types {
		if m.typeWobbling(wt.GVR) {
			s.Synced--
			s.Pending++
		}
	}
	return s
}
