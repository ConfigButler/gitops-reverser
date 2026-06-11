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
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-logr/logr/funcr"
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
	assert.False(t, mr.Exists(bodyKey("audit-2")), "the merged parked body is deleted eagerly")
}

func TestRedisAuditEventJoiner_CompleteOfficialDoesNotPeekOrMergeParkedBody(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoiner(t, mr)
	ctx := context.Background()

	additional := testAuditEvent("audit-complete-1", "wardle.example.com", true)
	additional.ObjectRef.Name = "from-additional"
	_, err := decide(ctx, t, joiner, AuditSourceAdditional, &additional)
	require.NoError(t, err)

	official := testAuditEvent("audit-complete-1", "wardle.example.com", true)
	official.ObjectRef.Name = "from-official"
	decision, err := decide(ctx, t, joiner, AuditSourceOfficial, &official)
	require.NoError(t, err)

	require.Equal(t, AuditJoinActionEmit, decision.Action)
	require.Equal(t, AuditJoinResultAsIs, decision.Result)
	assert.Equal(t, "from-official", decision.Event.ObjectRef.Name)
	assert.True(t, mr.Exists(bodyKey("audit-complete-1")),
		"complete official events should not pay Redis body lookup/delete; orphan bodies expire by TTL")
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
	assert.Equal(t, AuditJoinResultAsIs, decision.Result)
	assert.Nil(t, decision.Event.RequestObject)
	assert.Nil(t, decision.Event.ResponseObject)
	assert.True(t, mr.Exists(bodyKey("audit-delete-1")),
		"bodyless single deletes should not pay Redis body lookup/delete")
}

func TestRedisAuditEventJoiner_ShallowOfficialWithoutParkedBodyDropsImmediately(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoiner(t, mr)

	official := testAuditEvent("audit-shallow-1", "wardle.example.com", false)
	decision, err := decide(context.Background(), t, joiner, AuditSourceOfficial, &official)
	require.NoError(t, err)

	assert.Equal(t, AuditJoinActionDrop, decision.Action)
	assert.False(t, mr.Exists(bodyKey("audit-shallow-1")), "shallow official must not park")
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
}

// TestRedisAuditEventJoiner_WaitForBodyHonorsInjectedClock demonstrates PR #149
// review issue 4: waitForBody computes its deadline from the injectable j.now()
// but is driven by a real time.Ticker. When j.now() is frozen (or skewed), the
// deadline check `!j.now().Before(deadline)` never becomes true, so the wait
// loop spins until the context is cancelled instead of timing out cleanly.
func TestRedisAuditEventJoiner_WaitForBodyHonorsInjectedClock(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoinerWithOfficialBodyWait(t, mr, 100*time.Millisecond)

	// Freeze the joiner clock: every j.now() call returns the same instant.
	frozen := time.Now()
	joiner.now = func() time.Time { return frozen }

	official := testAuditEvent("audit-frozen-clock-1", "wardle.example.com", false)

	// The 1s context is only a safety net so the spin does not hang the suite
	// forever; correct behaviour must not depend on it firing.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	start := time.Now()
	decision, err := decide(ctx, t, joiner, AuditSourceOfficial, &official)
	elapsed := time.Since(start)

	require.NoError(t, err, "wait must time out on its own deadline, not via context cancellation")
	assert.Equal(t, AuditJoinActionDrop, decision.Action)
	assert.Less(t, elapsed, 750*time.Millisecond,
		"a 100ms body wait must not run until the 1s context deadline")
}

// TestRedisAuditEventJoiner_TimeoutLogsEveryOccurrenceAtInfo verifies a body
// wait timeout logs at default verbosity on every occurrence: a recurring
// timeout means the additional-body proxy is missing or lagging, and operators
// need that signal to persist rather than appear once at startup.
func TestRedisAuditEventJoiner_TimeoutLogsEveryOccurrenceAtInfo(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoinerWithOfficialBodyWait(t, mr, 30*time.Millisecond)

	var timeoutLogs atomic.Int64
	joiner.logger = funcr.New(func(_, args string) {
		if strings.Contains(args, "timed out waiting for") {
			timeoutLogs.Add(1)
		}
	}, funcr.Options{})

	ctx := context.Background()
	for _, auditID := range []string{"audit-timeout-log-1", "audit-timeout-log-2"} {
		official := testAuditEvent(auditID, "wardle.example.com", false)
		decision, err := decide(ctx, t, joiner, AuditSourceOfficial, &official)
		require.NoError(t, err)
		require.Equal(t, AuditJoinActionDrop, decision.Action)
	}

	assert.Equal(t, int64(2), timeoutLogs.Load(),
		"every body-wait timeout must log at default verbosity so a recurring failure stays visible")
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
}

func TestRedisAuditEventJoiner_DropsAdditionalWithoutBodyAsMalformed(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoiner(t, mr)

	event := testAuditEvent("audit-malformed", "wardle.example.com", false)
	decision, err := decide(context.Background(), t, joiner, AuditSourceAdditional, &event)
	require.NoError(t, err)

	assert.Equal(t, AuditJoinActionDrop, decision.Action)
	assert.False(t, mr.Exists(bodyKey("audit-malformed")))
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

// The decision-key dedupe is retired (C-C §4.1): a duplicate official delivery
// re-emits, lands in the per-type stream at the same resourceVersion, and is
// absorbed by the idempotent splice fold and the writer's no-op detection.
func TestRedisAuditEventJoiner_DuplicateOfficialReEmits(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoiner(t, mr)
	ctx := context.Background()

	event := testAuditEvent("audit-7", "wardle.example.com", true)
	decision, err := decide(ctx, t, joiner, AuditSourceOfficial, &event)
	require.NoError(t, err)
	require.Equal(t, AuditJoinActionEmit, decision.Action)

	duplicate, err := decide(ctx, t, joiner, AuditSourceOfficial, &event)
	require.NoError(t, err)
	assert.Equal(t, AuditJoinActionEmit, duplicate.Action,
		"a webhook retry must re-emit; the RV-keyed stream absorbs the duplicate")
}

// An additional body whose official has already been processed simply parks
// and ages out by TTL — there is no decision state left to consult.
func TestRedisAuditEventJoiner_AdditionalAfterOfficialEmitParksAndExpires(t *testing.T) {
	mr := miniredis.RunT(t)
	joiner := newTestJoiner(t, mr)
	ctx := context.Background()

	official := testAuditEvent("audit-late-1", "wardle.example.com", true)
	decision, err := decide(ctx, t, joiner, AuditSourceOfficial, &official)
	require.NoError(t, err)
	require.Equal(t, AuditJoinActionEmit, decision.Action)

	additional := testAuditEvent("audit-late-1", "wardle.example.com", true)
	late, err := decide(ctx, t, joiner, AuditSourceAdditional, &additional)
	require.NoError(t, err)
	assert.Equal(t, AuditJoinActionParked, late.Action)
	assert.True(t, mr.Exists(bodyKey("audit-late-1")), "the orphan body parks and expires by TTL")
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
