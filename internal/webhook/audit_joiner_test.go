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

package webhook

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	"sigs.k8s.io/yaml"
)

// decide is a small helper that classifies quality the way AuditHandler does, then calls Decide.
func decide(
	ctx context.Context,
	t *testing.T,
	joiner *RedisAuditEventJoiner,
	source AuditSource,
	event *auditv1.Event,
) (AuditJoinDecision, error) {
	t.Helper()
	return joiner.Decide(ctx, source, event, classifyAuditEventQuality(source, event))
}

func TestRedisAuditEventJoiner_WaitOfficialParksAdditionalBody(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoiner(t, mr)

	event := testAuditEvent("audit-1", "wardle.example.com", true)
	decision, err := decide(context.Background(), t, joiner, AuditSourceAdditional, &event)
	require.NoError(t, err)

	assert.Equal(t, AuditJoinActionParked, decision.Action)
	assert.True(t, mr.Exists(bodyKey("audit-1")))
	assert.False(t, mr.Exists(decisionKey("audit-1")))
}

func TestRedisAuditEventJoiner_WaitOfficialMergesParkedBody(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoiner(t, mr)
	ctx := context.Background()

	additional := testAuditEvent("audit-2", "wardle.example.com", true)
	additional.ObjectRef.Name = "flunder-a"
	additional.ObjectRef.Namespace = "team-a"
	additional.Annotations = map[string]string{"audit.k8s.io/proxy.requestObject.truncated": "true"}
	_, err := decide(ctx, t, joiner, AuditSourceAdditional, &additional)
	require.NoError(t, err)

	official := testAuditEvent("audit-2", "wardle.example.com", false)
	decision, err := decide(ctx, t, joiner, AuditSourceOfficial, &official)
	require.NoError(t, err)

	require.Equal(t, AuditJoinActionEmit, decision.Action)
	require.Equal(t, AuditJoinResultMerged, decision.Result)
	require.NotNil(t, decision.Event.RequestObject)
	require.NotNil(t, decision.Event.ResponseObject)
	assert.Equal(t, "flunder-a", decision.Event.ObjectRef.Name)
	assert.Equal(t, "team-a", decision.Event.ObjectRef.Namespace)
	assert.Equal(t, "true", decision.Event.Annotations["audit.k8s.io/proxy.requestObject.truncated"])

	require.NoError(t, joiner.CommitDecision(ctx, decision.AuditID, decision.Result))
	assert.True(t, mr.Exists(decisionKey("audit-2")))
	assert.False(t, mr.Exists(bodyKey("audit-2")))
}

func TestRedisAuditEventJoiner_DoesNotMergeDeleteOptionsBody(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoiner(t, mr)
	ctx := context.Background()

	additional := testAuditEvent("audit-delete-1", "wardle.example.com", false)
	additional.Verb = "delete"
	additional.RequestObject = &runtime.Unknown{Raw: []byte(`{"propagationPolicy":"Background"}`)}
	_, err := decide(ctx, t, joiner, AuditSourceAdditional, &additional)
	require.NoError(t, err)

	official := testAuditEvent("audit-delete-1", "wardle.example.com", false)
	official.Verb = "delete"
	official.ObjectRef.Name = "flunder-a"
	decision, err := decide(ctx, t, joiner, AuditSourceOfficial, &official)
	require.NoError(t, err)

	require.Equal(t, AuditJoinActionEmit, decision.Action)
	assert.Equal(t, AuditJoinResultMerged, decision.Result)
	assert.Nil(t, decision.Event.RequestObject)
	assert.Nil(t, decision.Event.ResponseObject)
}

func TestRedisAuditEventJoiner_ShallowOfficialWithoutParkedBodyDropsImmediately(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoiner(t, mr)

	official := testAuditEvent("audit-shallow-1", "wardle.example.com", false)
	decision, err := decide(context.Background(), t, joiner, AuditSourceOfficial, &official)
	require.NoError(t, err)

	assert.Equal(t, AuditJoinActionDrop, decision.Action)
	assert.False(t, mr.Exists(bodyKey("audit-shallow-1")), "shallow official must not park")
	assert.False(t, mr.Exists(decisionKey("audit-shallow-1")), "shallow drop must not claim a decision")
}

func TestRedisAuditEventJoiner_ShallowOfficialWaitsForLateAdditionalBody(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoinerWithOfficialBodyWait(t, mr, 200*time.Millisecond)
	ctx := context.Background()

	additional := testAuditEvent("audit-late-body-1", "wardle.example.com", true)
	official := testAuditEvent("audit-late-body-1", "wardle.example.com", false)

	errCh := make(chan error, 1)
	go func() {
		time.Sleep(30 * time.Millisecond)
		_, err := joiner.Decide(
			ctx,
			AuditSourceAdditional,
			&additional,
			classifyAuditEventQuality(AuditSourceAdditional, &additional),
		)
		errCh <- err
	}()

	decision, err := decide(ctx, t, joiner, AuditSourceOfficial, &official)
	require.NoError(t, err)
	require.NoError(t, <-errCh)

	require.Equal(t, AuditJoinActionEmit, decision.Action)
	require.Equal(t, AuditJoinResultMerged, decision.Result)
	require.NotNil(t, decision.Event.RequestObject)
	require.NotNil(t, decision.Event.ResponseObject)
	assert.False(t, mr.Exists(bodyKey("audit-late-body-1")))
}

func TestRedisAuditEventJoiner_ShallowOfficialTimesOutWaitingForBody(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoinerWithOfficialBodyWait(t, mr, 100*time.Millisecond)

	official := testAuditEvent("audit-wait-timeout-1", "wardle.example.com", false)

	start := time.Now()
	decision, err := decide(context.Background(), t, joiner, AuditSourceOfficial, &official)
	require.NoError(t, err)

	assert.GreaterOrEqual(t, time.Since(start), 100*time.Millisecond,
		"shallow official should hold the canonical gate for the full body wait before dropping")
	assert.Equal(t, AuditJoinActionDrop, decision.Action)
	assert.False(t, mr.Exists(bodyKey("audit-wait-timeout-1")), "timed-out wait must not park")
	assert.False(t, mr.Exists(decisionKey("audit-wait-timeout-1")), "shallow drop must not claim a decision")
}

func TestRedisAuditEventJoiner_WaitOfficialEmitsNamedOfficialWithoutBody(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoiner(t, mr)

	official := testAuditEvent("audit-3b", "wardle.example.com", false)
	official.Verb = "delete"
	official.ObjectRef.Name = "flunder-a"
	official.ObjectRef.Namespace = "team-a"
	decision, err := decide(context.Background(), t, joiner, AuditSourceOfficial, &official)
	require.NoError(t, err)

	assert.Equal(t, AuditJoinActionEmit, decision.Action)
	assert.Equal(t, AuditJoinResultAsIs, decision.Result)
	assert.Nil(t, decision.Event.RequestObject)
}

func TestRedisAuditEventJoiner_ParksAdditionalForAnyAPIGroup(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoiner(t, mr)

	event := testAuditEvent("audit-6", "unexpected.example.com", true)
	decision, err := decide(context.Background(), t, joiner, AuditSourceAdditional, &event)
	require.NoError(t, err)

	assert.Equal(t, AuditJoinActionParked, decision.Action)
	assert.True(t, mr.Exists(bodyKey("audit-6")))
	assert.False(t, mr.Exists(decisionKey("audit-6")))
}

func TestRedisAuditEventJoiner_DropsAdditionalWithoutBodyAsMalformed(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoiner(t, mr)

	event := testAuditEvent("audit-malformed", "wardle.example.com", false)
	decision, err := decide(context.Background(), t, joiner, AuditSourceAdditional, &event)
	require.NoError(t, err)

	assert.Equal(t, AuditJoinActionDrop, decision.Action)
	assert.False(t, mr.Exists(bodyKey("audit-malformed")))
	assert.False(t, mr.Exists(decisionKey("audit-malformed")))
}

func TestClassifyAuditEventQuality_DeleteCollectionFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/audit-events/config-deletecollection.yaml")
	require.NoError(t, err)

	var event auditv1.Event
	require.NoError(t, yaml.Unmarshal(raw, &event))

	assert.Equal(t, AuditEventQualityCollection, classifyAuditEventQuality(AuditSourceOfficial, &event))
	require.NotNil(t, event.ResponseObject)
	assert.Contains(t, string(event.ResponseObject.Raw), "ConfigMapList")
}

func TestRedisAuditEventJoiner_CollectionEventEmitsAsIsWithListBody(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoiner(t, mr)

	raw, err := os.ReadFile("testdata/audit-events/config-deletecollection.yaml")
	require.NoError(t, err)
	var event auditv1.Event
	require.NoError(t, yaml.Unmarshal(raw, &event))

	decision, err := decide(context.Background(), t, joiner, AuditSourceOfficial, &event)
	require.NoError(t, err)
	require.Equal(t, AuditJoinActionEmit, decision.Action)
	assert.Equal(t, AuditJoinResultAsIs, decision.Result)
	require.NotNil(t, decision.Event.ResponseObject)
	assert.Contains(t, string(decision.Event.ResponseObject.Raw), "ConfigMapList")
}

func TestRedisAuditEventJoiner_ReleaseDecisionAllowsReclaim(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoiner(t, mr)
	ctx := context.Background()

	event := testAuditEvent("audit-7", "wardle.example.com", true)
	decision, err := decide(ctx, t, joiner, AuditSourceOfficial, &event)
	require.NoError(t, err)
	require.Equal(t, AuditJoinActionEmit, decision.Action)

	duplicate, err := decide(ctx, t, joiner, AuditSourceOfficial, &event)
	require.NoError(t, err)
	require.Equal(t, AuditJoinActionDrop, duplicate.Action)

	require.NoError(t, joiner.ReleaseDecision(ctx, decision.AuditID))
	reclaimed, err := decide(ctx, t, joiner, AuditSourceOfficial, &event)
	require.NoError(t, err)
	assert.Equal(t, AuditJoinActionEmit, reclaimed.Action)
}

func TestRedisAuditEventJoiner_CommitDecisionStoresEmittedState(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoiner(t, mr)
	ctx := context.Background()

	event := testAuditEvent("audit-8", "wardle.example.com", true)
	decision, err := decide(ctx, t, joiner, AuditSourceOfficial, &event)
	require.NoError(t, err)
	require.NoError(t, joiner.CommitDecision(ctx, decision.AuditID, decision.Result))

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	raw, err := client.Get(ctx, decisionKey("audit-8")).Bytes()
	require.NoError(t, err)

	var envelope auditDecisionEnvelope
	require.NoError(t, json.Unmarshal(raw, &envelope))
	assert.Equal(t, "emitted", envelope.State)
	assert.Equal(t, AuditJoinResultAsIs, envelope.Result)
}

func TestRedisAuditEventJoiner_AdditionalAfterCommittedDecisionIsBodyLate(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoiner(t, mr)
	ctx := context.Background()

	official := testAuditEvent("audit-late-1", "wardle.example.com", true)
	decision, err := decide(ctx, t, joiner, AuditSourceOfficial, &official)
	require.NoError(t, err)
	require.Equal(t, AuditJoinActionEmit, decision.Action)
	require.NoError(t, joiner.CommitDecision(ctx, decision.AuditID, decision.Result))

	additional := testAuditEvent("audit-late-1", "wardle.example.com", true)
	late, err := decide(ctx, t, joiner, AuditSourceAdditional, &additional)
	require.NoError(t, err)
	assert.Equal(t, AuditJoinActionDrop, late.Action)
	assert.False(t, mr.Exists(bodyKey("audit-late-1")), "late additional must not park")
}

func TestRedisAuditEventJoiner_AdditionalDuringClaimedDecisionDropsAsInFlight(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoiner(t, mr)
	ctx := context.Background()

	// Claim but do not commit; mimics the handler having claimed and being mid-enqueue.
	official := testAuditEvent("audit-inflight-1", "wardle.example.com", true)
	decision, err := decide(ctx, t, joiner, AuditSourceOfficial, &official)
	require.NoError(t, err)
	require.Equal(t, AuditJoinActionEmit, decision.Action)

	additional := testAuditEvent("audit-inflight-1", "wardle.example.com", true)
	mid, err := decide(ctx, t, joiner, AuditSourceAdditional, &additional)
	require.NoError(t, err)
	assert.Equal(t, AuditJoinActionDrop, mid.Action)
	assert.False(t, mr.Exists(bodyKey("audit-inflight-1")), "in-flight sibling must not park")
}

func newTestJoiner(
	t *testing.T,
	mr *miniredis.Miniredis,
) *RedisAuditEventJoiner {
	t.Helper()
	return newTestJoinerWithOfficialBodyWait(t, mr, 0)
}

func newTestJoinerWithOfficialBodyWait(
	t *testing.T,
	mr *miniredis.Miniredis,
	officialBodyWait time.Duration,
) *RedisAuditEventJoiner {
	t.Helper()
	joiner, err := NewRedisAuditEventJoiner(RedisAuditJoinerConfig{
		Addr:             mr.Addr(),
		BodyTTL:          time.Minute,
		DecisionTTL:      time.Hour,
		OfficialBodyWait: officialBodyWait,
	})
	require.NoError(t, err)
	return joiner
}

func testAuditEvent(auditID, group string, withBody bool) auditv1.Event {
	event := auditv1.Event{
		AuditID:    types.UID(auditID),
		Stage:      auditv1.StageResponseComplete,
		Verb:       "create",
		RequestURI: "/apis/" + group + "/v1/namespaces/team-a/flunders",
		ObjectRef: &auditv1.ObjectReference{
			APIGroup:   group,
			APIVersion: "v1",
			Resource:   "flunders",
		},
	}
	event.User.Username = "test-user"
	if withBody {
		event.RequestObject = &runtime.Unknown{Raw: []byte(`{"kind":"Flunder","metadata":{"name":"flunder-a"}}`)}
		event.ResponseObject = &runtime.Unknown{Raw: []byte(`{"kind":"Flunder","metadata":{"name":"flunder-a"}}`)}
	}
	return event
}
