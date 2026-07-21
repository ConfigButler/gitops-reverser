// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// bootstrapRuleStore loads existing WatchRule and ClusterWatchRule objects into the in-memory RuleStore
// before the first reconcile so startup behavior matches steady-state controller reconciles.
func (m *Manager) bootstrapRuleStore(ctx context.Context, log logr.Logger) error {
	if m.RuleStore == nil || m.Client == nil {
		return nil
	}

	var watchRules configv1alpha3.WatchRuleList
	if err := m.Client.List(ctx, &watchRules); err != nil {
		return fmt.Errorf("listing WatchRules: %w", err)
	}
	for i := range watchRules.Items {
		rule := watchRules.Items[i]
		if err := m.bootstrapWatchRule(ctx, rule); err != nil {
			log.Error(err, "Failed to seed WatchRule into store",
				"name", rule.Name, "namespace", rule.Namespace)
		}
	}

	var clusterWatchRules configv1alpha3.ClusterWatchRuleList
	if err := m.Client.List(ctx, &clusterWatchRules); err != nil {
		return fmt.Errorf("listing ClusterWatchRules: %w", err)
	}
	for i := range clusterWatchRules.Items {
		rule := clusterWatchRules.Items[i]
		if err := m.bootstrapClusterWatchRule(ctx, rule); err != nil {
			log.Error(err, "Failed to seed ClusterWatchRule into store", "name", rule.Name)
		}
	}

	m.RuleStore.MarkReady()
	return nil
}

func (m *Manager) bootstrapWatchRule(ctx context.Context, rule configv1alpha3.WatchRule) error {
	targetNS := rule.Namespace

	target, provider, err := m.resolveTargetAndProvider(ctx, client.ObjectKey{
		Name:      rule.Spec.TargetRef.Name,
		Namespace: targetNS,
	})
	if err != nil {
		return err
	}

	// Route through the SHARED gated compile path, never straight at the store. Bootstrap runs
	// BEFORE the first reconcile on every restart, so a source-namespace gate the reconciler alone
	// enforced would be bypassed for the whole startup window — long enough to compile a denied
	// override and watch a namespace the policy refuses. A denial is not fatal to startup: the
	// rule is simply left out of the store (bootstrap has no controllers yet and cannot publish
	// status), and the first reconcile re-decides and writes the terminal condition.
	resolved, err := CompileWatchRule(ctx, m.Client, m.RuleStore, m, rule, target, provider)
	if err != nil {
		return fmt.Errorf("evaluating source-namespace authorization for WatchRule %s/%s: %w",
			rule.Namespace, rule.Name, err)
	}
	if !resolved.Admitted() {
		return fmt.Errorf("WatchRule %s/%s source-namespace scope was not authorized: %s",
			rule.Namespace, rule.Name, resolved.Message)
	}

	return nil
}

func (m *Manager) bootstrapClusterWatchRule(ctx context.Context, rule configv1alpha3.ClusterWatchRule) error {
	targetNS := rule.Spec.TargetRef.Namespace

	target, provider, err := m.resolveTargetAndProvider(ctx, client.ObjectKey{
		Name:      rule.Spec.TargetRef.Name,
		Namespace: targetNS,
	})
	if err != nil {
		return err
	}

	// Route through the SHARED gated compile path, never straight at the store. Bootstrap runs
	// BEFORE the first reconcile on every restart, so a gate the reconciler alone enforced would be
	// bypassed for the whole startup window — long enough to compile a rule and plan a stream for a
	// target the provider never admitted, or to open a namespaced watch for a stored
	// `scope: Namespaced` this release no longer supports. A refusal is not fatal to startup: the
	// rule is simply left out of the store, and the reconciler's own gate re-decides (and can grant
	// it) as soon as the controller's initial sync reaches this rule.
	decision, err := CompileClusterWatchRule(ctx, m.Client, m.RuleStore, rule, target, provider)
	if err != nil {
		return fmt.Errorf("evaluating admission for ClusterWatchRule %q: %w", rule.Name, err)
	}
	if !decision.Admitted {
		return fmt.Errorf("ClusterWatchRule %q was not compiled: %s", rule.Name, decision.Message)
	}

	return nil
}

func (m *Manager) resolveTargetAndProvider(
	ctx context.Context,
	targetKey client.ObjectKey,
) (configv1alpha3.GitTarget, configv1alpha3.GitProvider, error) {
	var target configv1alpha3.GitTarget
	if err := m.Client.Get(ctx, targetKey, &target); err != nil {
		return configv1alpha3.GitTarget{}, configv1alpha3.GitProvider{},
			fmt.Errorf("resolving GitTarget %s/%s: %w", targetKey.Namespace, targetKey.Name, err)
	}

	providerKey := client.ObjectKey{
		Name:      target.Spec.ProviderRef.Name,
		Namespace: target.Namespace,
	}
	var provider configv1alpha3.GitProvider
	if err := m.Client.Get(ctx, providerKey, &provider); err != nil {
		return configv1alpha3.GitTarget{}, configv1alpha3.GitProvider{},
			fmt.Errorf("resolving GitProvider %s/%s: %w", providerKey.Namespace, providerKey.Name, err)
	}

	return target, provider, nil
}
