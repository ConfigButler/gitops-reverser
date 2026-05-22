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
	"math"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// restConfig acquires the controller runtime REST config.
// Returns nil if no config is available (e.g., in unit tests without a cluster).
func (m *Manager) restConfig() *rest.Config {
	// ctrl.GetConfig reads KUBECONFIG or in-cluster config. In tests/e2e this is
	// set up by the test harness/Kind. In unit tests without a cluster it returns
	// an error, which callers handle gracefully.
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil
	}
	return cfg
}

func crdTriggerGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "apiextensions.k8s.io",
		Version:  "v1",
		Resource: "customresourcedefinitions",
	}
}

func apiServiceTriggerGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "apiregistration.k8s.io",
		Version:  "v1",
		Resource: "apiservices",
	}
}

// RefreshAPIResourceCatalog refreshes trusted catalog data from Kubernetes discovery.
func (m *Manager) RefreshAPIResourceCatalog(ctx context.Context) error {
	catalog := m.apiResourceCatalog()
	disco, err := m.apiResourceDiscovery()
	if err != nil {
		return err
	}
	start := time.Now()
	changed, refreshErr := catalog.Refresh(disco)
	recordCatalogRefresh(ctx, changed, refreshErr, time.Since(start))
	if refreshErr == nil {
		recordCatalogStats(ctx, catalog.Stats())
	}
	return refreshErr
}

// Catalog refresh outcome label values.
const (
	catalogRefreshChanged   = "changed"
	catalogRefreshUnchanged = "unchanged"
	catalogRefreshError     = "error"
)

// recordCatalogRefresh emits the api_catalog_refresh_total counter and the
// api_catalog_refresh_duration_seconds histogram for one refresh.
func recordCatalogRefresh(ctx context.Context, changed bool, err error, elapsed time.Duration) {
	if telemetry.APICatalogRefreshDurationSeconds != nil {
		telemetry.APICatalogRefreshDurationSeconds.Record(ctx, elapsed.Seconds())
	}
	if telemetry.APICatalogRefreshTotal == nil {
		return
	}
	outcome := catalogRefreshUnchanged
	switch {
	case err != nil:
		outcome = catalogRefreshError
	case changed:
		outcome = catalogRefreshChanged
	}
	telemetry.APICatalogRefreshTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

// recordCatalogStats sets the api_catalog_resources, api_catalog_group_versions,
// and api_catalog_generation gauges after a successful refresh. Gauges are
// idempotent, so overwriting them on every refresh is correct.
func recordCatalogStats(ctx context.Context, stats CatalogStats) {
	if telemetry.APICatalogResources != nil {
		telemetry.APICatalogResources.Record(ctx, int64(stats.AllowedResources),
			metric.WithAttributes(attribute.String("state", "allowed")))
		telemetry.APICatalogResources.Record(ctx, int64(stats.ExcludedResources),
			metric.WithAttributes(attribute.String("state", "excluded")))
	}
	if telemetry.APICatalogGroupVersions != nil {
		telemetry.APICatalogGroupVersions.Record(ctx, int64(stats.TrustedGroupVersions),
			metric.WithAttributes(attribute.String("state", "trusted")))
		telemetry.APICatalogGroupVersions.Record(ctx, int64(stats.DegradedGroupVersions),
			metric.WithAttributes(attribute.String("state", "degraded")))
	}
	if telemetry.APICatalogGeneration != nil {
		generation := stats.Generation
		if generation > math.MaxInt64 {
			generation = math.MaxInt64
		}
		telemetry.APICatalogGeneration.Record(ctx, int64(generation))
	}
}

func (m *Manager) apiResourceCatalog() *APIResourceCatalog {
	m.resourceCatalogMu.Lock()
	defer m.resourceCatalogMu.Unlock()
	if m.resourceCatalog == nil {
		m.resourceCatalog = NewAPIResourceCatalog()
	}
	return m.resourceCatalog
}

func (m *Manager) ruleGVRResolver() *RuleGVRResolver {
	return NewRuleGVRResolver(m.apiResourceCatalog())
}

func (m *Manager) apiResourceDiscovery() (apiResourceDiscovery, error) {
	if m.discoveryClient != nil {
		return m.discoveryClient()
	}
	cfg := m.restConfig()
	if cfg == nil {
		return nil, errors.New("no REST config available for API resource discovery")
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create API resource discovery client: %w", err)
	}
	return disco, nil
}

// ResolveWatchRuleResources resolves one WatchRule for controller status feedback.
func (m *Manager) ResolveWatchRuleResources(
	_ context.Context,
	rule configv1alpha1.WatchRule,
) (bool, string) {
	var misses []ResolveMiss
	resolver := m.ruleGVRResolver()
	for _, resourceRule := range rule.Spec.Rules {
		_, ruleMisses := resolver.Resolve(
			resourceRule.APIGroups,
			resourceRule.APIVersions,
			resourceRule.Resources,
			configv1alpha1.ResourceScopeNamespaced,
		)
		misses = append(misses, ruleMisses...)
	}
	return len(misses) == 0, FormatResolveMisses(misses)
}

// ResolveClusterWatchRuleResources resolves one ClusterWatchRule for status feedback.
func (m *Manager) ResolveClusterWatchRuleResources(
	_ context.Context,
	rule configv1alpha1.ClusterWatchRule,
) (bool, string) {
	var misses []ResolveMiss
	resolver := m.ruleGVRResolver()
	for _, resourceRule := range rule.Spec.Rules {
		_, ruleMisses := resolver.Resolve(
			resourceRule.APIGroups,
			resourceRule.APIVersions,
			resourceRule.Resources,
			resourceRule.Scope,
		)
		misses = append(misses, ruleMisses...)
	}
	return len(misses) == 0, FormatResolveMisses(misses)
}

func (m *Manager) signalCatalogRefresh() {
	if m.catalogRefreshCh == nil {
		return
	}
	select {
	case m.catalogRefreshCh <- struct{}{}:
	default:
	}
}

func (m *Manager) startAPISurfaceTriggerInformers(ctx context.Context, log logr.Logger) {
	cfg := m.restConfig()
	if cfg == nil {
		log.V(1).Info("skipping API surface trigger informers - no REST config available")
		return
	}
	dynamicClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Error(err, "failed to create API surface trigger client")
		return
	}

	factory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, 0)
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { m.signalCatalogRefresh() },
		UpdateFunc: func(any, any) { m.signalCatalogRefresh() },
		DeleteFunc: func(any) { m.signalCatalogRefresh() },
	}
	informers := []cache.SharedIndexInformer{
		factory.ForResource(crdTriggerGVR()).Informer(),
		factory.ForResource(apiServiceTriggerGVR()).Informer(),
	}
	for _, informer := range informers {
		if _, addErr := informer.AddEventHandler(handler); addErr != nil {
			log.Error(addErr, "failed to add API surface trigger handler")
			return
		}
	}

	factory.Start(ctx.Done())
	go waitForAPISurfaceTriggerSync(ctx, log, informers)
}

func waitForAPISurfaceTriggerSync(ctx context.Context, log logr.Logger, informers []cache.SharedIndexInformer) {
	syncFns := make([]cache.InformerSynced, 0, len(informers))
	for _, informer := range informers {
		syncFns = append(syncFns, informer.HasSynced)
	}
	if !cache.WaitForCacheSync(ctx.Done(), syncFns...) {
		log.Info("API surface trigger informer sync stopped before completion")
		return
	}
	log.V(1).Info("API surface trigger informers synced")
}
