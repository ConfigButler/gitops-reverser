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
	// Capture the UID before starting watches: the data plane keys its resume cursors
	// by GitTarget UID, which the rule-derived watch tables do not carry.
	m.rememberGitTargetUID(gitDest)
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
	m.forgetGitTargetUID(gitDest)
	m.declaredGVRsMu.Lock()
	defer m.declaredGVRsMu.Unlock()
	delete(m.declaredGVRs, gitDest.String())
}
