// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestGitReceiver_Key(t *testing.T) {
	receiver := &GitReceiver{ObjectMeta: metav1.ObjectMeta{Name: "hook", Namespace: "platform"}}
	assert.Equal(t, types.NamespacedName{Namespace: "platform", Name: "hook"}, receiver.Key())
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
