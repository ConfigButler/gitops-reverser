// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kwatch "k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func triggerListKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		crdTriggerGVR():        "CustomResourceDefinitionList",
		apiServiceTriggerGVR(): "APIServiceList",
	}
}

// forbiddenTriggerClient serves the CRD trigger normally and denies the given resource with a
// 403 on both list and watch — an ordinary apiserver seen by a ServiceAccount whose ClusterRole
// does not name that resource. Flip the returned flag to grant the permission.
//
// The denial is gated on an atomic rather than by rewriting the client's reaction chains: a
// reflector on another resource reads those chains concurrently, so mutating them races.
func forbiddenTriggerClient(denied schema.GroupVersionResource) (*dynamicfake.FakeDynamicClient, *atomic.Bool) {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), triggerListKinds())

	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Group: denied.Group, Resource: denied.Resource}, "",
		errors.New("RBAC: no rule grants list on this resource"))

	var denying atomic.Bool
	denying.Store(true)

	client.PrependReactor("list", denied.Resource,
		func(clienttesting.Action) (bool, runtime.Object, error) {
			if denying.Load() {
				return true, nil, forbidden
			}
			return false, nil, nil // fall through to the object tracker
		})
	client.PrependWatchReactor(denied.Resource,
		func(clienttesting.Action) (bool, kwatch.Interface, error) {
			if denying.Load() {
				return true, nil, forbidden
			}
			return false, nil, nil
		})
	return client, &denying
}

// managerWithTriggerClient wires a manager whose discovery serves BOTH triggers (so the
// ServesWatchable gate cannot be what stops anything) and whose dynamic client is the fake.
func managerWithTriggerClient(t *testing.T, client *dynamicfake.FakeDynamicClient) *Manager {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	catalog := NewAPIResourceCatalog()
	_, err := catalog.Refresh(aggregatingDiscovery())
	require.NoError(t, err)

	m := &Manager{Log: logr.Discard()}
	m.triggerCtx = ctx
	m.triggerClient = client
	m.resourceCatalog = catalog
	return m
}

func (m *Manager) triggerIsStarted(gvr schema.GroupVersionResource) bool {
	m.triggersMu.Lock()
	defer m.triggersMu.Unlock()
	_, started := m.triggersStarted[gvr]
	return started
}

func (m *Manager) triggerForbiddenLogged(gvr schema.GroupVersionResource) bool {
	m.triggersMu.Lock()
	defer m.triggersMu.Unlock()
	_, logged := m.triggersForbiddenLogged[gvr]
	return logged
}

// Discovery reports what the SERVER serves, not what the CALLER may read. On an ordinary
// apiserver apiregistration.k8s.io is served, so ServesWatchable says yes and the informer
// starts — then the reflector's LIST is denied and client-go retries it forever. That is the
// same endless, benign noise the unserved-resource gate exists to remove.
//
// The informer must stop, and stop counting as started, so a later refresh can re-arm it.
func TestEnsureAPISurfaceTriggerInformers_StopsForbiddenInformer(t *testing.T) {
	denied := apiServiceTriggerGVR()
	client, _ := forbiddenTriggerClient(denied)
	m := managerWithTriggerClient(t, client)

	m.ensureAPISurfaceTriggerInformers(m.Log)

	// Discovery serves both, so both start; only the 403 may take one down.
	require.True(t, m.triggerIsStarted(crdTriggerGVR()))

	require.Eventually(t, func() bool { return !m.triggerIsStarted(denied) },
		5*time.Second, 10*time.Millisecond,
		"a forbidden trigger informer must be stopped and un-started, not retried forever")

	// One denial must not disarm the whole API surface.
	require.True(t, m.triggerIsStarted(crdTriggerGVR()),
		"the permitted trigger must keep running")
}

// A denial that never clears must be recorded once, not once per reflector retry.
func TestEnsureAPISurfaceTriggerInformers_ForbiddenLogsOncePerDenial(t *testing.T) {
	denied := apiServiceTriggerGVR()
	client, _ := forbiddenTriggerClient(denied)
	m := managerWithTriggerClient(t, client)

	m.ensureAPISurfaceTriggerInformers(m.Log)

	require.Eventually(t, func() bool { return m.triggerForbiddenLogged(denied) },
		5*time.Second, 10*time.Millisecond, "the denial must be recorded so it logs once")
}

// Granting the RBAC later must be picked up on the next catalog refresh, with no restart —
// the same promise the unserved path makes for an aggregation layer installed later.
func TestEnsureAPISurfaceTriggerInformers_ReArmsAfterForbiddenIsGranted(t *testing.T) {
	denied := apiServiceTriggerGVR()
	client, denying := forbiddenTriggerClient(denied)
	m := managerWithTriggerClient(t, client)

	m.ensureAPISurfaceTriggerInformers(m.Log)
	require.Eventually(t, func() bool { return !m.triggerIsStarted(denied) },
		5*time.Second, 10*time.Millisecond, "forbidden informer should have been stopped")

	// Grant the permission. The reactors stay in place and start falling through.
	denying.Store(false)

	m.ensureAPISurfaceTriggerInformers(m.Log)

	require.Eventually(t, func() bool { return m.triggerIsStarted(denied) },
		5*time.Second, 10*time.Millisecond,
		"a trigger denied earlier must re-arm once RBAC allows it, without a restart")
}

// A non-RBAC error is transient — a connection reset, a 500 — and the reflector's own
// backoff owns it. Tearing the informer down there would turn a blip into a lost trigger.
func TestEnsureAPISurfaceTriggerInformers_KeepsInformerOnTransientError(t *testing.T) {
	gvr := apiServiceTriggerGVR()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), triggerListKinds())
	client.PrependReactor("list", gvr.Resource,
		func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewInternalError(errors.New("etcd leader changed"))
		})

	m := managerWithTriggerClient(t, client)
	m.ensureAPISurfaceTriggerInformers(m.Log)

	require.Never(t, func() bool { return !m.triggerIsStarted(gvr) },
		500*time.Millisecond, 25*time.Millisecond,
		"a 500 is not a permission problem; the reflector must keep retrying it")
	require.False(t, m.triggerForbiddenLogged(gvr))
}
