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
	"k8s.io/apimachinery/pkg/runtime"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"

	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

const pipelineEventsMetric = "gitopsreverser_audit_pipeline_events_total"
const pipelineRouteTargetsMetric = "gitopsreverser_audit_pipeline_route_targets_total"

// The consumer's pipeline metrics now cover only the surviving consumer route — the /scale
// subresource → parent field-patch (object mirroring moved to the per-type splice, R3). These
// exercise that route's routed / unmatched / route-failed outcomes.

// deploymentScaleRuleStore returns a rule store with one WatchRule matching apps/v1 deployments.
func deploymentScaleRuleStore() *rulestore.RuleStore {
	rs := rulestore.NewStore()
	rs.AddOrUpdateWatchRule(
		makeWatchRule("dep-rule", []string{"deployments"}, []string{"v1"}, []string{"apps"}),
		"my-target", "default",
		"my-provider", "default",
		"main", "state/",
	)
	return rs
}

// scaleEvent builds a deployments/scale audit event carrying a Scale body with spec.replicas.
func pipelineScaleEvent() auditv1.Event {
	ev := makeAuditEvent("patch", auditv1.StageResponseComplete, "deployments", "web")
	ev.ObjectRef.APIGroup = "apps"
	ev.ObjectRef.Subresource = "scale"
	ev.ResponseObject = &runtime.Unknown{
		Raw: []byte(`{"kind":"Scale","apiVersion":"autoscaling/v1","spec":{"replicas":3}}`),
	}
	return ev
}

func TestAuditPipelineEventsMetric_RoutedScale(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	mr := miniredis.RunT(t)
	er := &fakeEventRouter{}
	c := newTestConsumer(t, mr, deploymentScaleRuleStore(), er)
	require.NoError(t, c.ensureConsumerGroup(context.Background()))

	pushAuditMessage(t, mr, pipelineScaleEvent())
	require.NoError(t, c.readAndProcessBatch(context.Background()))

	pipeline, ok := telemetry.CollectInt64Sum(reader, pipelineEventsMetric, map[string]string{
		"group": "apps", "version": "v1", "resource": "deployments",
		"verb": "patch", "outcome": "routed_scale_subresource",
	})
	require.True(t, ok, "expected a routed_scale_subresource audit_pipeline_events_total sample")
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

	pushAuditMessage(t, mr, pipelineScaleEvent())
	require.NoError(t, c.readAndProcessBatch(context.Background()))

	pipeline, ok := telemetry.CollectInt64Sum(reader, pipelineEventsMetric, map[string]string{
		"resource": "deployments", "verb": "patch", "outcome": "unmatched",
	})
	require.True(t, ok, "expected an unmatched audit_pipeline_events_total sample")
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
	c := newTestConsumer(t, mr, deploymentScaleRuleStore(), er)
	require.NoError(t, c.ensureConsumerGroup(context.Background()))

	pushAuditMessage(t, mr, pipelineScaleEvent())
	require.NoError(t, c.readAndProcessBatch(context.Background()))

	pipeline, ok := telemetry.CollectInt64Sum(reader, pipelineEventsMetric, map[string]string{
		"resource": "deployments", "verb": "patch", "outcome": "route_failed",
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
