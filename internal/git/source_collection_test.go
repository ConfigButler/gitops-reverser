// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func collectionForTest(namespace string) types.CollectionKey {
	return types.CollectionKeyFor(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, namespace)
}

func TestSourceCollectionForLog(t *testing.T) {
	assert.Equal(t, "unclaimed", sourceCollectionForLog(types.CollectionKey{}),
		"the zero collection is how every non-stream producer queues work, and a log line has to say so")
	assert.Equal(t, "configmaps in team-a", sourceCollectionForLog(collectionForTest("team-a")),
		"a storm is diagnosed from which collection produced the write")
}

func TestWriteRequest_SourceCollection(t *testing.T) {
	claimed := collectionForTest("team-a")

	assert.Equal(t, types.CollectionKey{}, (&WriteRequest{}).sourceCollection(), "no events, nothing claimed it")
	assert.Equal(t, claimed,
		(&WriteRequest{Events: []Event{{SourceCollection: claimed}}}).sourceCollection(),
		"the live path wraps one event, and that event's collection is the request's")

	mixed := &WriteRequest{Events: []Event{
		{SourceCollection: claimed},
		{SourceCollection: collectionForTest("team-b")},
	}}
	assert.Equal(t, types.CollectionKey{}, mixed.sourceCollection(),
		"a request spanning collections speaks for none of them; it must not borrow the first one's identity")
}
