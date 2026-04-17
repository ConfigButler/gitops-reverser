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
	"fmt"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
)

// bootstrapRuleStore loads existing WatchRule and ClusterWatchRule objects into the in-memory RuleStore
// before the first reconcile so startup behavior matches steady-state controller reconciles.
func (m *Manager) bootstrapRuleStore(ctx context.Context, log logr.Logger) error {
	if m.RuleStore == nil || m.Client == nil {
		return nil
	}

	var watchRules configv1alpha1.WatchRuleList
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

	var clusterWatchRules configv1alpha1.ClusterWatchRuleList
	if err := m.Client.List(ctx, &clusterWatchRules); err != nil {
		return fmt.Errorf("listing ClusterWatchRules: %w", err)
	}
	for i := range clusterWatchRules.Items {
		rule := clusterWatchRules.Items[i]
		if err := m.bootstrapClusterWatchRule(ctx, rule); err != nil {
			log.Error(err, "Failed to seed ClusterWatchRule into store", "name", rule.Name)
		}
	}

	return nil
}

func (m *Manager) bootstrapWatchRule(ctx context.Context, rule configv1alpha1.WatchRule) error {
	targetNS := rule.Namespace

	target, provider, err := m.resolveTargetAndProvider(ctx, client.ObjectKey{
		Name:      rule.Spec.TargetRef.Name,
		Namespace: targetNS,
	})
	if err != nil {
		return err
	}

	m.RuleStore.AddOrUpdateWatchRule(
		rule,
		target.Name, target.Namespace,
		provider.Name, provider.Namespace,
		target.Spec.Branch,
		target.Spec.Path,
	)

	return nil
}

func (m *Manager) bootstrapClusterWatchRule(ctx context.Context, rule configv1alpha1.ClusterWatchRule) error {
	targetNS := rule.Spec.TargetRef.Namespace

	target, provider, err := m.resolveTargetAndProvider(ctx, client.ObjectKey{
		Name:      rule.Spec.TargetRef.Name,
		Namespace: targetNS,
	})
	if err != nil {
		return err
	}

	m.RuleStore.AddOrUpdateClusterWatchRule(
		rule,
		target.Name, target.Namespace,
		provider.Name, provider.Namespace,
		target.Spec.Branch,
		target.Spec.Path,
	)

	return nil
}

func (m *Manager) resolveTargetAndProvider(
	ctx context.Context,
	targetKey client.ObjectKey,
) (configv1alpha1.GitTarget, configv1alpha1.GitProvider, error) {
	var target configv1alpha1.GitTarget
	if err := m.Client.Get(ctx, targetKey, &target); err != nil {
		return configv1alpha1.GitTarget{}, configv1alpha1.GitProvider{},
			fmt.Errorf("resolving GitTarget %s/%s: %w", targetKey.Namespace, targetKey.Name, err)
	}

	providerKey := client.ObjectKey{
		Name:      target.Spec.ProviderRef.Name,
		Namespace: target.Namespace,
	}
	var provider configv1alpha1.GitProvider
	if err := m.Client.Get(ctx, providerKey, &provider); err != nil {
		return configv1alpha1.GitTarget{}, configv1alpha1.GitProvider{},
			fmt.Errorf("resolving GitProvider %s/%s: %w", providerKey.Namespace, providerKey.Name, err)
	}

	return target, provider, nil
}
