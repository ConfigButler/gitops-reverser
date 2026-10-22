// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"testing"

	meta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestGitProvider_GitReceiverKey(t *testing.T) {
	provider := &GitProvider{ObjectMeta: metav1.ObjectMeta{Name: "config", Namespace: "tenant-a"}}

	_, ok := provider.GitReceiverKey()
	assert.False(t, ok, "no reference names no receiver")

	provider.Spec.GitReceiverRef = &meta.NamespacedObjectReference{}
	_, ok = provider.GitReceiverKey()
	assert.False(t, ok, "an empty name names no receiver")

	provider.Spec.GitReceiverRef = &meta.NamespacedObjectReference{Name: "hook"}
	key, ok := provider.GitReceiverKey()
	require.True(t, ok)
	assert.Equal(t, types.NamespacedName{Namespace: "tenant-a", Name: "hook"}, key,
		"an omitted namespace is the provider's own")

	provider.Spec.GitReceiverRef.Namespace = "platform"
	key, ok = provider.GitReceiverKey()
	require.True(t, ok)
	assert.Equal(t, types.NamespacedName{Namespace: "platform", Name: "hook"}, key)
}

func TestGitReceiver_AdmitsNamespace(t *testing.T) {
	receiver := &GitReceiver{ObjectMeta: metav1.ObjectMeta{Name: "hook", Namespace: "platform"}}
	assert.Equal(t, types.NamespacedName{Namespace: "platform", Name: "hook"}, receiver.Key())

	admitted, err := receiver.AdmitsNamespace("platform", nil)
	require.NoError(t, err)
	assert.True(t, admitted, "its own namespace is always admitted")

	admitted, err = receiver.AdmitsNamespace("tenant-a", nil)
	require.NoError(t, err)
	assert.False(t, admitted, "omitted accessFrom admits no other namespace")

	receiver.Spec.AccessFrom = &NamespaceMatcher{Selector: &metav1.LabelSelector{
		MatchLabels: map[string]string{"tier": "trusted"},
	}}
	admitted, err = receiver.AdmitsNamespace("tenant-a", map[string]string{"tier": "trusted"})
	require.NoError(t, err)
	assert.True(t, admitted)
	admitted, err = receiver.AdmitsNamespace("tenant-b", map[string]string{"tier": "other"})
	require.NoError(t, err)
	assert.False(t, admitted)
}

// TestReceiverPath_KeyedByTokenAndIdentity is the path's whole contract: it names neither the
// namespace nor the receiver, and changes with each of the three inputs.
func TestReceiverPath_KeyedByTokenAndIdentity(t *testing.T) {
	path := ReceiverPath([]byte("token"), "platform", "hook")
	assert.Regexp(t, `^/git-push/[0-9a-f]{64}$`, path)
	assert.NotContains(t, path, "platform")
	assert.NotContains(t, path, "hook")
	assert.Equal(t, path, ReceiverPath([]byte("token"), "platform", "hook"), "the path is stable")
	assert.NotEqual(t, path, ReceiverPath([]byte("other"), "platform", "hook"))
	assert.NotEqual(t, path, ReceiverPath([]byte("token"), "tenant-a", "hook"))
	assert.NotEqual(t, path, ReceiverPath([]byte("token"), "platform", "other"))
}
