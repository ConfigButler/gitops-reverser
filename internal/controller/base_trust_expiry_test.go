// SPDX-License-Identifier: Apache-2.0

package controller

// The scheduled half of --base-trust-max-age.
//
// The review that prompted this test found the gap it guards: clearing the trust flag is a no-op
// on exactly the target the age exists for. An idle target is converged, so nothing publishes, so
// nothing ever spends the fetch the cleared flag merely permits, and Git stays unread. Expiry has
// to report that it fired so the reconcile can force the re-read, and that boolean is what this
// pins.

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

func baseTrustExpiryFixture(t *testing.T, maxAge time.Duration) (
	*GitTargetReconciler, *configv1alpha3.GitTarget, *git.BranchWorker,
) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha3.AddToScheme(scheme))

	target := &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "editing", Namespace: "team-a"},
		Spec: configv1alpha3.GitTargetSpec{
			Branch: "main",
			Path:   "apps",
		},
	}
	target.Spec.GitProviderRef.Name = "provider"
	provider := &configv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "provider", Namespace: "team-a"},
		Spec:       configv1alpha3.GitProviderSpec{URL: "https://example.test/acme/infra.git"},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target, provider).Build()

	manager := git.NewWorkerManager(client, logr.Discard(), git.BranchWorkerLimits{},
		itypes.SensitiveResourcePolicy{})
	// Start publishes the context EnsureWorker starts each worker under. It blocks until the
	// context is cancelled, so it runs in the background and the cancel tears the workers down.
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	go func() {
		close(started)
		_ = manager.Start(ctx)
	}()
	<-started
	t.Cleanup(cancel)

	require.Eventually(t, func() bool {
		return manager.EnsureWorker(ctx, "provider", "team-a", "main") == nil
	}, 5*time.Second, 10*time.Millisecond, "the worker manager never accepted a worker")
	worker, ok := manager.GetWorkerForTarget("provider", "team-a", "main")
	require.True(t, ok)

	return &GitTargetReconciler{
		Client:          client,
		WorkerManager:   manager,
		BaseTrustMaxAge: maxAge,
	}, target, worker
}

// TestExpireStaleBaseTrust_ReportsWhenItFired is the regression: without the boolean the caller
// cannot force a re-read, and an idle target never reads Git again.
func TestExpireStaleBaseTrust_ReportsWhenItFired(t *testing.T) {
	r, target, worker := baseTrustExpiryFixture(t, time.Minute)
	worker.SeedBaseTrustForTest(time.Now().Add(-time.Hour))

	assert.True(t, r.expireStaleBaseTrust(target, "team-a", logr.Discard()),
		"an expired base must be reported so the reconcile forces the re-read")
	assert.False(t, worker.BaseTrustedForTest(), "and the flag itself must be cleared")
}

// TestExpireStaleBaseTrust_QuietWhenNothingExpired. A reconcile that expires nothing must not
// force a re-check, or every steady pass would drag the folder over the network.
func TestExpireStaleBaseTrust_QuietWhenNothingExpired(t *testing.T) {
	r, target, worker := baseTrustExpiryFixture(t, time.Hour)
	worker.SeedBaseTrustForTest(time.Now())

	assert.False(t, r.expireStaleBaseTrust(target, "team-a", logr.Discard()))
	assert.True(t, worker.BaseTrustedForTest())
}

// TestExpireStaleBaseTrust_DisabledIsAlwaysQuiet keeps the default free of any of this.
func TestExpireStaleBaseTrust_DisabledIsAlwaysQuiet(t *testing.T) {
	r, target, worker := baseTrustExpiryFixture(t, 0)
	worker.SeedBaseTrustForTest(time.Now().Add(-24 * time.Hour))

	assert.False(t, r.expireStaleBaseTrust(target, "team-a", logr.Discard()))
	assert.True(t, worker.BaseTrustedForTest())
}
