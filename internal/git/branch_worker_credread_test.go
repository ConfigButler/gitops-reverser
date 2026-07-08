// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// TestCommitPendingWrites_ResolvesCredentialsOncePerPushCycle proves the branch worker does not
// re-read the Git credentials Secret for every commit in a push cycle. Only the first commit
// (hasPendingCommits=false) touches the remote through PrepareBranch and needs auth; later commits
// build on the local repo and must not issue another Secret GET — with the Secret cache disabled
// that would be a wasted API round-trip per commit. See docs/future/secret-value-retention-plan.md §5.
func TestCommitPendingWrites_ResolvesCredentialsOncePerPushCycle(t *testing.T) {
	tempDir := t.TempDir()
	remotePath := filepath.Join(tempDir, "remote")
	createBareRepo(t, remotePath)

	const credsSecretName = "git-creds"
	var credentialReads atomic.Int64

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha3.AddToScheme(scheme))

	provider := &configv1alpha3.GitProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "test-repo", Namespace: "default"},
		Spec: configv1alpha3.GitProviderSpec{
			URL:       "file://" + remotePath,
			SecretRef: &configv1alpha3.LocalSecretReference{Name: credsSecretName},
		},
	}
	// username/password resolves to HTTP basic auth, which the file:// transport ignores — so the
	// commit path exercises the real credential read without needing a network remote.
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: credsSecretName, Namespace: "default"},
		Data:       map[string][]byte{"username": []byte("git"), "password": []byte("token")},
	}

	countingClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(provider, creds).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(
				ctx context.Context,
				c client.WithWatch,
				key client.ObjectKey,
				obj client.Object,
				opts ...client.GetOption,
			) error {
				if _, ok := obj.(*corev1.Secret); ok && key.Name == credsSecretName {
					credentialReads.Add(1)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	worker := NewBranchWorker(countingClient, logr.Discard(), "test-repo", "default", "main", nil, 0)
	worker.ctx = context.Background()

	// First commit of the cycle: fetches the remote tip, so it resolves credentials exactly once.
	firstWrite, err := worker.buildGroupedPendingWrite(worker.ctx, []Event{createTestEvent(t, "pod-a")})
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*firstWrite}, false))
	require.Equal(t, int64(1), credentialReads.Load(),
		"first commit resolves credentials once for the remote fetch")

	// Second commit in the same cycle (hasPendingCommits=true): local-only, no credential read.
	secondWrite, err := worker.buildGroupedPendingWrite(worker.ctx, []Event{createTestEvent(t, "pod-b")})
	require.NoError(t, err)
	require.NoError(t, worker.commitPendingWrites([]PendingWrite{*secondWrite}, true))
	assert.Equal(t, int64(1), credentialReads.Load(),
		"a later commit in the same push cycle must not re-read the credentials Secret")
}
