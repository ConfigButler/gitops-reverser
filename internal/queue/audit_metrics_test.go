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

package queue

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"

	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

const pipelineEventsMetric = "gitopsreverser_audit_pipeline_events_total"
const pipelineRouteTargetsMetric = "gitopsreverser_audit_pipeline_route_targets_total"

// configmapRuleStore returns a rule store with one WatchRule matching core/v1 configmaps.
func configmapRuleStore() *rulestore.RuleStore {
	rs := rulestore.NewStore()
	rs.AddOrUpdateWatchRule(
		makeWatchRule("cm-rule", []string{"configmaps"}, []string{"v1"}, []string{""}),
		"my-target", "default",
		"my-provider", "default",
		"main", "state/",
	)
	return rs
}

func TestAuditPipelineEventsMetric_Routed(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	mr := miniredis.RunT(t)
	er := &fakeEventRouter{}
	c := newTestConsumer(t, mr, configmapRuleStore(), er)
	require.NoError(t, c.ensureConsumerGroup(context.Background()))

	ev := makeAuditEvent("create", auditv1.StageResponseComplete, "configmaps", "default", "cm")
	setAuditResponseObject(t, &ev, "v1", "ConfigMap", "default", "cm")
	pushAuditMessage(t, mr, ev)
	require.NoError(t, c.readAndProcessBatch(context.Background()))

	pipeline, ok := telemetry.CollectInt64Sum(reader, pipelineEventsMetric, map[string]string{
		"group": "", "version": "v1", "resource": "configmaps", "verb": "create", "outcome": "routed",
	})
	require.True(t, ok, "expected a routed audit_pipeline_events_total sample")
	assert.Equal(t, int64(1), pipeline)

	targets, ok := telemetry.CollectInt64Sum(reader, pipelineRouteTargetsMetric, map[string]string{
		"git_target_namespace": "default", "git_target": "my-target",
		"rule_kind": "watchrule", "outcome": "routed",
	})
	require.True(t, ok, "expected a routed audit_pipeline_route_targets_total sample")
	assert.Equal(t, int64(1), targets)
}

func TestAuditPipelineEventsMetric_Unmatched(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	mr := miniredis.RunT(t)
	er := &fakeEventRouter{}
	c := newTestConsumer(t, mr, rulestore.NewStore(), er) // empty rule store
	require.NoError(t, c.ensureConsumerGroup(context.Background()))

	ev := makeAuditEvent("create", auditv1.StageResponseComplete, "configmaps", "default", "cm")
	setAuditResponseObject(t, &ev, "v1", "ConfigMap", "default", "cm")
	pushAuditMessage(t, mr, ev)
	require.NoError(t, c.readAndProcessBatch(context.Background()))

	pipeline, ok := telemetry.CollectInt64Sum(reader, pipelineEventsMetric, map[string]string{
		"resource": "configmaps", "verb": "create", "outcome": "unmatched",
	})
	require.True(t, ok, "expected an unmatched audit_pipeline_events_total sample")
	assert.Equal(t, int64(1), pipeline)
}

func TestAuditPipelineEventsMetric_DroppedNoBody(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	mr := miniredis.RunT(t)
	er := &fakeEventRouter{}
	c := newTestConsumer(t, mr, configmapRuleStore(), er)
	require.NoError(t, c.ensureConsumerGroup(context.Background()))

	// A create event with neither requestObject nor responseObject has no usable body.
	ev := makeAuditEvent("create", auditv1.StageResponseComplete, "configmaps", "default", "cm")
	pushAuditMessage(t, mr, ev)
	require.NoError(t, c.readAndProcessBatch(context.Background()))

	pipeline, ok := telemetry.CollectInt64Sum(reader, pipelineEventsMetric, map[string]string{
		"resource": "configmaps", "verb": "create", "outcome": "dropped_no_body",
	})
	require.True(t, ok, "expected a dropped_no_body audit_pipeline_events_total sample")
	assert.Equal(t, int64(1), pipeline)
}

func TestAuditPipelineEventsMetric_RouteFailed(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	mr := miniredis.RunT(t)
	er := &fakeEventRouter{
		errFor: map[string]error{
			itypes.NewResourceReference("my-target", "default").Key(): assert.AnError,
		},
	}
	c := newTestConsumer(t, mr, configmapRuleStore(), er)
	require.NoError(t, c.ensureConsumerGroup(context.Background()))

	ev := makeAuditEvent("create", auditv1.StageResponseComplete, "configmaps", "default", "cm")
	setAuditResponseObject(t, &ev, "v1", "ConfigMap", "default", "cm")
	pushAuditMessage(t, mr, ev)
	require.NoError(t, c.readAndProcessBatch(context.Background()))

	pipeline, ok := telemetry.CollectInt64Sum(reader, pipelineEventsMetric, map[string]string{
		"resource": "configmaps", "verb": "create", "outcome": "route_failed",
	})
	require.True(t, ok, "expected a route_failed audit_pipeline_events_total sample")
	assert.Equal(t, int64(1), pipeline)

	targets, ok := telemetry.CollectInt64Sum(reader, pipelineRouteTargetsMetric, map[string]string{
		"git_target_namespace": "default", "git_target": "my-target",
		"rule_kind": "watchrule", "outcome": "route_failed",
	})
	require.True(t, ok, "expected a route_failed audit_pipeline_route_targets_total sample")
	assert.Equal(t, int64(1), targets)
}
