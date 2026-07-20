// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// configmapsGVR is a second served namespaced type, used to prove per-type scope resolution
// is membership-exact.
var configmapsGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}

func TestTableWatchesGVR(t *testing.T) {
	table := WatchedTypeTable{Types: []WatchedType{{GVR: secretsGVR}}}
	assert.True(t, tableWatchesGVR(table, secretsGVR), "a watched type is reported present")
	assert.False(t, tableWatchesGVR(table, configmapsGVR), "an unwatched type is reported absent")
}
