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
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

const targetReconcileCompletedMetric = "gitopsreverser_target_reconcile_completed_total"

// recordTargetReconcileCompleted must increment the per-GitTarget counter and
// carry the gittarget_* and trigger labels.
func TestRecordTargetReconcileCompleted_IncrementsPerTrigger(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	m := &Manager{Log: logr.Discard()}
	gitDest := types.NewResourceReference("my-target", "my-ns")

	m.recordTargetReconcileCompleted(gitDest, "rule_change")

	value, ok := telemetry.CollectInt64Sum(reader, targetReconcileCompletedMetric,
		map[string]string{"gittarget_namespace": "my-ns", "gittarget_name": "my-target", "trigger": "rule_change"})
	require.True(t, ok, "expected a target_reconcile_completed_total sample")
	assert.Equal(t, int64(1), value)

	// A second pass increments the counter, proving a delta over a baseline
	// distinguishes a fresh reconcile from a stale latched value.
	m.recordTargetReconcileCompleted(gitDest, "rule_change")
	value, ok = telemetry.CollectInt64Sum(reader, targetReconcileCompletedMetric,
		map[string]string{"gittarget_namespace": "my-ns", "gittarget_name": "my-target", "trigger": "rule_change"})
	require.True(t, ok)
	assert.Equal(t, int64(2), value)

	// A different trigger is a distinct series, not folded into the first.
	m.recordTargetReconcileCompleted(gitDest, "startup_replay")
	value, ok = telemetry.CollectInt64Sum(reader, targetReconcileCompletedMetric,
		map[string]string{"gittarget_namespace": "my-ns", "gittarget_name": "my-target", "trigger": "startup_replay"})
	require.True(t, ok)
	assert.Equal(t, int64(1), value)
}
