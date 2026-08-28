// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

func cellForTest(namespace string) types.CellKey {
	return types.CellKeyFor(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, namespace)
}

func TestSourceCellForLog(t *testing.T) {
	assert.Equal(t, "unclaimed", sourceCellForLog(types.CellKey{}),
		"the zero cell is how every non-stream producer queues work, and a log line has to say so")
	assert.Equal(t, "configmaps in team-a", sourceCellForLog(cellForTest("team-a")),
		"a storm is diagnosed from which cell produced the write")
}

func TestWriteRequest_SourceCell(t *testing.T) {
	claimed := cellForTest("team-a")

	assert.Equal(t, types.CellKey{}, (&WriteRequest{}).sourceCell(), "no events, nothing claimed it")
	assert.Equal(t, claimed,
		(&WriteRequest{Events: []Event{{SourceCell: claimed}}}).sourceCell(),
		"the live path wraps one event, and that event's cell is the request's")

	mixed := &WriteRequest{Events: []Event{
		{SourceCell: claimed},
		{SourceCell: cellForTest("team-b")},
	}}
	assert.Equal(t, types.CellKey{}, mixed.sourceCell(),
		"a request spanning cells speaks for none of them; it must not borrow the first one's identity")
}
