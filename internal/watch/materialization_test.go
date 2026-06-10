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
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/typeset"
)

// secretsGVR is defined in manager_snapshot_test.go (same package): core v1 secrets.

// TestDeclareForGitTarget_ClaimsResolvedSetAndIsIdempotent proves the L-2 wiring: a
// reconcile declares the GitTarget's full resolved type-set on the materialization axis,
// and re-reconciling is an idempotent renew (DEC-L3) — the claimant set stays stable.
func TestDeclareForGitTarget_ClaimsResolvedSetAndIsIdempotent(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-cm", "test-target", "configmaps"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-sec", "test-target", "secrets"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	gitDest := gitDestRef("test-target")
	ref := typeset.GitTargetRef(gitDest.String())

	require.NoError(t, manager.DeclareForGitTarget(context.Background(), gitDest))
	mat := manager.materializerInstance()
	require.Equal(t, []typeset.GitTargetRef{ref}, mat.Claimants(configMapGVR))
	require.Equal(t, []typeset.GitTargetRef{ref}, mat.Claimants(secretsGVR))

	// Re-declaring the same resolved set is an idempotent renew: the claimants are stable.
	require.NoError(t, manager.DeclareForGitTarget(context.Background(), gitDest))
	require.Equal(t, []typeset.GitTargetRef{ref}, mat.Claimants(configMapGVR))
	require.Equal(t, []typeset.GitTargetRef{ref}, mat.Claimants(secretsGVR))
}

// TestDeclareForGitTarget_OnlyClaimsResolvedTypes proves a type no rule resolves to is never
// claimed. Combined with the sweep's lease GC (typeset.Materializer tests), this is how a
// type dropped from a WatchRule stops being renewed and is released at the next sweep: a
// later, smaller resolved set simply omits it.
func TestDeclareForGitTarget_OnlyClaimsResolvedTypes(t *testing.T) {
	manager, store := makeWatchedTypeManager(t)
	store.AddOrUpdateClusterWatchRule(
		clusterRuleForResource("rule-cm", "test-target", "configmaps"),
		"test-target", "test-ns", "test-provider", "test-ns", "main", "test-path",
	)
	gitDest := gitDestRef("test-target")
	ref := typeset.GitTargetRef(gitDest.String())

	require.NoError(t, manager.DeclareForGitTarget(context.Background(), gitDest))
	mat := manager.materializerInstance()
	require.Equal(t, []typeset.GitTargetRef{ref}, mat.Claimants(configMapGVR))
	require.Empty(t, mat.Claimants(secretsGVR), "a type no rule resolves to must not be claimed")
}

// TestDeclareForGitTarget_FailsClosedDeclaresNothing proves the fail-closed discipline: an
// unobservable API surface returns an error and declares nothing — a partial or empty set on
// an unobserved surface would read as a withdrawal and wrongly age out live claims.
func TestDeclareForGitTarget_FailsClosedDeclaresNothing(t *testing.T) {
	manager := &Manager{
		Log: logr.Discard(),
		discoveryClient: func() (apiResourceDiscovery, error) {
			return nil, errors.New("discovery unavailable")
		},
	}
	gitDest := gitDestRef("test-target")

	err := manager.DeclareForGitTarget(context.Background(), gitDest)
	require.Error(t, err, "an unobservable API surface must fail closed")
	require.Empty(t, manager.materializerInstance().Claimants(configMapGVR),
		"a failed resolve must declare nothing")
}

// TestDistinctClaimGVRs_CollapsesScopes proves the claim keys on (ref, GVR) only, so the
// resolved (GVR, namespace-scope) stream set collapses to its distinct GVRs in resolver order.
func TestDistinctClaimGVRs_CollapsesScopes(t *testing.T) {
	deployGVR := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	out := distinctClaimGVRs([]snapshotGVR{
		{gvr: deployGVR, namespaces: []string{"ns-a"}},
		{gvr: deployGVR, namespaces: []string{"ns-b"}}, // same GVR, different scope -> collapses
		{gvr: configMapGVR},
	})
	require.Equal(t, []schema.GroupVersionResource{deployGVR, configMapGVR}, out)
}

// TestStartMaterializationSweep_AgesOutUnrenewedLease proves the periodic sweep ticker
// actually drives Sweep on its (injected fast) interval: an unrenewed claim is GC'd once its
// renewal predates the previous tick (DEC-L5), and the goroutine stops on context cancel.
func TestStartMaterializationSweep_AgesOutUnrenewedLease(t *testing.T) {
	manager := &Manager{Log: logr.Discard(), materializationSweepIntervalOverride: 5 * time.Millisecond}
	ref := typeset.GitTargetRef("test-ns/lapsed-target")
	manager.materializerInstance().Declare(ref, []schema.GroupVersionResource{configMapGVR})
	require.NotEmpty(t, manager.materializerInstance().Claimants(configMapGVR))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.startMaterializationSweep(ctx, logr.Discard())

	require.Eventually(t, func() bool {
		return len(manager.materializerInstance().Claimants(configMapGVR)) == 0
	}, 2*time.Second, 5*time.Millisecond, "the periodic sweep must age out an unrenewed lease")
}
