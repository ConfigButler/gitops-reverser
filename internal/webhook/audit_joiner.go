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
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// auditJoinerFirsts holds one-shot startup-milestone log gates; see
// audit_handler.go for rationale.
type auditJoinerFirsts struct {
	shallowDropped       sync.Once
	malformedAdditional  sync.Once
	additionalBodyParked sync.Once
	mergedEmit           sync.Once
	officialWaitMerged   sync.Once
}

const (
	// AuditSourceOfficial identifies events received from the kube-apiserver audit webhook.
	AuditSourceOfficial AuditSource = "official"
	// AuditSourceAdditional identifies events received from the supplementary audit webhook.
	AuditSourceAdditional AuditSource = "additional"

	defaultAuditEventBodyTTL     = 5 * time.Minute
	defaultAuditEventDecisionTTL = time.Hour
	auditJoinBodyPollInterval    = 25 * time.Millisecond
)

// AuditSource is the semantic source role of an audit webhook endpoint.
type AuditSource string

// AuditEventQuality classifies whether an audit event has enough shape to enter
// the canonical stream or should wait for an additional body contribution.
type AuditEventQuality string

const (
	// AuditEventQualityComplete means the event carries a request or response object.
	AuditEventQualityComplete AuditEventQuality = "complete"
	// AuditEventQualityBodyShallowDeletable means a bodyless delete has enough objectRef identity to emit.
	AuditEventQualityBodyShallowDeletable AuditEventQuality = "body_shallow_deletable"
	// AuditEventQualityCollection means a deletecollection event carries the collection response object.
	AuditEventQualityCollection AuditEventQuality = "collection"
	// AuditEventQualityIdentityShallow means the event cannot drive a high-quality Git write without a body.
	AuditEventQualityIdentityShallow AuditEventQuality = "identity_shallow"
	// AuditEventQualityMalformed means an additional body contribution arrived without a usable body.
	AuditEventQualityMalformed AuditEventQuality = "malformed"
)

// AuditJoinAction describes how the handler should proceed after a join decision.
type AuditJoinAction int

const (
	// AuditJoinActionParked acknowledges a parked body contribution without a stream write.
	AuditJoinActionParked AuditJoinAction = iota
	// AuditJoinActionEmit asks the handler to enqueue Event and then commit or release the decision.
	AuditJoinActionEmit
	// AuditJoinActionDrop acknowledges an event that must not reach the canonical stream.
	AuditJoinActionDrop
)

// AuditJoinResult is stored on committed decisions and emitted as a metric label.
type AuditJoinResult string

const (
	// AuditJoinResultAsIs means the event was emitted without a parked body contribution.
	AuditJoinResultAsIs AuditJoinResult = "as_is"
	// AuditJoinResultMerged means an official event was merged with a parked body contribution.
	AuditJoinResultMerged AuditJoinResult = "merged"
)

// AuditJoinDecision is the two-phase decision returned to AuditHandler before enqueueing.
type AuditJoinDecision struct {
	Action  AuditJoinAction
	Event   *auditv1.Event
	AuditID string
	Result  AuditJoinResult
	Source  AuditSource
}

// AuditEventJoiner decides whether incoming audit events should park, emit, or drop.
// Callers classify quality once and pass it in; the joiner does not re-classify.
type AuditEventJoiner interface {
	Decide(
		ctx context.Context,
		source AuditSource,
		event *auditv1.Event,
		quality AuditEventQuality,
	) (AuditJoinDecision, error)
	CommitDecision(ctx context.Context, auditID string, result AuditJoinResult) error
	ReleaseDecision(ctx context.Context, auditID string) error
}

// AuditBodyEnvelope is the parked contribution shape stored under audit:body:v1:<auditID>.
// Only additional-source bodies are ever parked under the simplified joiner.
type AuditBodyEnvelope struct {
	Version           int                      `json:"v"`
	AuditID           string                   `json:"auditID"`
	ReceivedAt        metav1.Time              `json:"receivedAt"`
	RequestObject     *runtime.RawExtension    `json:"requestObject,omitempty"`
	ResponseObject    *runtime.RawExtension    `json:"responseObject,omitempty"`
	ObjectRef         *auditv1.ObjectReference `json:"objectRef,omitempty"`
	Annotations       map[string]string        `json:"annotations,omitempty"`
	HasRequestObject  bool                     `json:"hasRequestObject"`
	HasResponseObject bool                     `json:"hasResponseObject"`
}

type auditDecisionEnvelope struct {
	Version   int             `json:"v"`
	State     string          `json:"state"`
	ClaimedAt metav1.Time     `json:"claimedAt"`
	EmittedAt *metav1.Time    `json:"emittedAt,omitempty"`
	Result    AuditJoinResult `json:"result,omitempty"`
}

// RedisAuditJoinerConfig configures Redis-backed audit body parking and decision tracking.
type RedisAuditJoinerConfig struct {
	Addr             string
	Username         string
	AuthValue        string
	DB               int
	TLSEnabled       bool
	BodyTTL          time.Duration
	DecisionTTL      time.Duration
	OfficialBodyWait time.Duration
}

// RedisAuditEventJoiner implements audit body parking with Redis/Valkey keys.
type RedisAuditEventJoiner struct {
	client           *redis.Client
	bodyTTL          time.Duration
	decisionTTL      time.Duration
	officialBodyWait time.Duration
	now              func() time.Time
	logger           logr.Logger
	firsts           auditJoinerFirsts
}

// NewRedisAuditEventJoiner creates a Redis-backed AuditEventJoiner.
func NewRedisAuditEventJoiner(cfg RedisAuditJoinerConfig) (*RedisAuditEventJoiner, error) {
	if strings.TrimSpace(cfg.Addr) == "" {
		return nil, errors.New("redis address is required")
	}
	bodyTTL := cfg.BodyTTL
	if bodyTTL <= 0 {
		bodyTTL = defaultAuditEventBodyTTL
	}
	decisionTTL := cfg.DecisionTTL
	if decisionTTL <= 0 {
		decisionTTL = defaultAuditEventDecisionTTL
	}

	options := &redis.Options{
		Addr:     cfg.Addr,
		Username: cfg.Username,
		Password: cfg.AuthValue,
		DB:       cfg.DB,
	}
	if cfg.TLSEnabled {
		options.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	return &RedisAuditEventJoiner{
		client:           redis.NewClient(options),
		bodyTTL:          bodyTTL,
		decisionTTL:      decisionTTL,
		officialBodyWait: cfg.OfficialBodyWait,
		now:              time.Now,
		logger:           logf.Log.WithName("audit-joiner"),
	}, nil
}

// Decide examines an event and, for emitted events, claims the audit ID before enqueueing.
// Officials never park. An identity-shallow official with no matching parked body waits briefly
// for an additional body contribution, then drops with audit_shallow_dropped_total if none arrives.
func (j *RedisAuditEventJoiner) Decide(
	ctx context.Context,
	source AuditSource,
	event *auditv1.Event,
	quality AuditEventQuality,
) (AuditJoinDecision, error) {
	if event == nil {
		return AuditJoinDecision{}, errors.New("audit event is nil")
	}
	auditID := string(event.AuditID)
	if strings.TrimSpace(auditID) == "" {
		return AuditJoinDecision{}, errors.New("auditID cannot be empty")
	}

	if source == AuditSourceAdditional {
		return j.handleAdditional(ctx, event, quality)
	}

	return j.handleOfficial(ctx, event, quality)
}

// CommitDecision promotes a claimed decision to emitted after the canonical stream write succeeds.
func (j *RedisAuditEventJoiner) CommitDecision(
	ctx context.Context,
	auditID string,
	result AuditJoinResult,
) error {
	now := metav1.NewTime(j.now().UTC())
	envelope := auditDecisionEnvelope{
		Version:   1,
		State:     "emitted",
		ClaimedAt: now,
		EmittedAt: &now,
		Result:    result,
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("failed to marshal audit decision for %q: %w", auditID, err)
	}
	if err := j.client.Set(ctx, decisionKey(auditID), payload, j.decisionTTL).Err(); err != nil {
		return fmt.Errorf("failed to commit audit decision for %q: %w", auditID, err)
	}
	return nil
}

// ReleaseDecision deletes a claimed decision so a retry or sibling event can claim it later.
func (j *RedisAuditEventJoiner) ReleaseDecision(ctx context.Context, auditID string) error {
	if err := j.client.Del(ctx, decisionKey(auditID)).Err(); err != nil {
		return fmt.Errorf("failed to release audit decision for %q: %w", auditID, err)
	}
	return nil
}

func (j *RedisAuditEventJoiner) handleOfficial(
	ctx context.Context,
	event *auditv1.Event,
	quality AuditEventQuality,
) (AuditJoinDecision, error) {
	auditID := string(event.AuditID)
	officialArrival := j.now()

	if officialCanEmitAsIs(quality) {
		return j.emitOfficialAsIs(ctx, auditID, event)
	}

	envelope, hasBody, err := j.peekBody(ctx, auditID)
	if err != nil {
		return AuditJoinDecision{}, err
	}
	bodyParkedBeforeOfficial := hasBody

	if !hasBody {
		envelope, hasBody, err = j.waitForBody(ctx, auditID, event)
		if err != nil {
			return AuditJoinDecision{}, err
		}
	}

	if !hasBody {
		return j.dropShallowOfficial(ctx, auditID, event, quality), nil
	}

	return j.emitMergedOfficial(ctx, auditID, event, envelope, bodyParkedBeforeOfficial, officialArrival)
}

func officialCanEmitAsIs(quality AuditEventQuality) bool {
	return quality == AuditEventQualityComplete ||
		quality == AuditEventQualityCollection ||
		quality == AuditEventQualityBodyShallowDeletable
}

func (j *RedisAuditEventJoiner) dropShallowOfficial(
	ctx context.Context,
	auditID string,
	event *auditv1.Event,
	quality AuditEventQuality,
) AuditJoinDecision {
	addShallowDroppedMetric(ctx, event)
	joinerLog := j.logger
	j.firsts.shallowDropped.Do(func() {
		joinerLog.Info(
			"First shallow audit event dropped — no request/response body received. "+
				"Install apiservice-audit-proxy or update kube-apiserver audit policy "+
				"to include bodies. Further drops will log at V(1) only.",
			"auditID", auditID,
			"gvr", auditEventGVR(event),
			"verb", event.Verb,
		)
	})
	joinerLog.V(1).Info("audit shallow event dropped",
		"auditID", auditID,
		"gvr", auditEventGVR(event),
		"verb", event.Verb,
		"source", string(AuditSourceOfficial),
		"quality", quality,
		"hasRequestObject", hasRuntimeUnknownBody(event.RequestObject),
		"hasResponseObject", hasRuntimeUnknownBody(event.ResponseObject),
	)
	return AuditJoinDecision{Action: AuditJoinActionDrop}
}

func (j *RedisAuditEventJoiner) emitMergedOfficial(
	ctx context.Context,
	auditID string,
	event *auditv1.Event,
	envelope AuditBodyEnvelope,
	bodyParkedBeforeOfficial bool,
	officialArrival time.Time,
) (AuditJoinDecision, error) {
	claimed, err := j.claimDecision(ctx, auditID)
	if err != nil {
		return AuditJoinDecision{}, err
	}
	if !claimed {
		addDuplicateMetric(ctx, "decision_exists")
		return AuditJoinDecision{Action: AuditJoinActionDrop}, nil
	}

	if err := j.deleteBody(ctx, auditID); err != nil {
		j.logger.Error(err, "Failed to delete merged parked audit body", "auditID", auditID)
	}
	if bodyParkedBeforeOfficial {
		// The additional body was already parked when the official arrived: the skew is
		// the proxy's lead time. waitForBody records the official-first cases itself.
		observeJoinSkew(ctx, joinArrivalBodyFirst, joinOutcomeMerged,
			officialArrival.Sub(envelope.ReceivedAt.Time).Seconds())
	}
	j.firsts.mergedEmit.Do(func() {
		j.logger.Info(
			"First shallow-event conversion succeeded: official event merged with parked additional body",
			"auditID", auditID,
			"gvr", auditEventGVR(event),
			"verb", event.Verb,
		)
	})

	return AuditJoinDecision{
		Action:  AuditJoinActionEmit,
		Event:   mergeParkedBody(event, envelope),
		AuditID: auditID,
		Result:  AuditJoinResultMerged,
		Source:  AuditSourceOfficial,
	}, nil
}

func (j *RedisAuditEventJoiner) emitOfficialAsIs(
	ctx context.Context,
	auditID string,
	event *auditv1.Event,
) (AuditJoinDecision, error) {
	claimed, err := j.claimDecision(ctx, auditID)
	if err != nil {
		return AuditJoinDecision{}, err
	}
	if !claimed {
		addDuplicateMetric(ctx, "decision_exists")
		return AuditJoinDecision{Action: AuditJoinActionDrop}, nil
	}
	return AuditJoinDecision{
		Action:  AuditJoinActionEmit,
		Event:   event,
		AuditID: auditID,
		Result:  AuditJoinResultAsIs,
		Source:  AuditSourceOfficial,
	}, nil
}

func (j *RedisAuditEventJoiner) waitForBody(
	ctx context.Context,
	auditID string,
	event *auditv1.Event,
) (AuditBodyEnvelope, bool, error) {
	if j.officialBodyWait <= 0 {
		return AuditBodyEnvelope{}, false, nil
	}

	// The body wait is an inherently real-time operation: the deadline and the
	// skew samples must be measured from one wall clock, never from the
	// injectable j.now() (which tests freeze). time.After owns the deadline so
	// there is no clock comparison left to get wrong.
	waitStart := time.Now()
	ticker := time.NewTicker(auditJoinBodyPollInterval)
	defer ticker.Stop()
	timeout := time.After(j.officialBodyWait)

	j.logger.V(1).Info("waiting briefly for additional audit body",
		"auditID", auditID,
		"gvr", auditEventGVR(event),
		"verb", event.Verb,
		"wait", j.officialBodyWait)

	for {
		select {
		case <-ctx.Done():
			return AuditBodyEnvelope{}, false, ctx.Err()
		case <-timeout:
			observeJoinSkew(ctx, joinArrivalOfficialFirst, joinOutcomeTimedOut,
				time.Since(waitStart).Seconds())
			// Logged on every occurrence (deliberately not gated behind a
			// sync.Once): a recurring timeout means the additional-body proxy
			// is missing or lagging, and operators need that signal to persist,
			// not just appear once at startup.
			j.logger.Info(
				"WARNING: official shallow audit event timed out waiting for additional body; "+
					"the official event will be dropped. Install or repair apiservice-audit-proxy "+
					"so request/response bodies arrive within the wait budget.",
				"auditID", auditID,
				"gvr", auditEventGVR(event),
				"verb", event.Verb,
				"wait", j.officialBodyWait)
			return AuditBodyEnvelope{}, false, nil
		case <-ticker.C:
			envelope, hasBody, err := j.peekBody(ctx, auditID)
			if err != nil {
				return AuditBodyEnvelope{}, false, err
			}
			if hasBody {
				observeJoinSkew(ctx, joinArrivalOfficialFirst, joinOutcomeMerged,
					time.Since(waitStart).Seconds())
				j.firsts.officialWaitMerged.Do(func() {
					j.logger.Info("Official shallow audit event found additional body after waiting",
						"auditID", auditID,
						"gvr", auditEventGVR(event),
						"verb", event.Verb,
						"wait", j.officialBodyWait)
				})
				return envelope, true, nil
			}
		}
	}
}

func (j *RedisAuditEventJoiner) handleAdditional(
	ctx context.Context,
	event *auditv1.Event,
	quality AuditEventQuality,
) (AuditJoinDecision, error) {
	auditID := string(event.AuditID)
	joinerLog := j.logger
	if quality == AuditEventQualityMalformed {
		j.firsts.malformedAdditional.Do(func() {
			joinerLog.Info(
				"First malformed additional audit event dropped (no request/response body). "+
					"Further drops will log at V(1) only.",
				"auditID", auditID,
				"gvr", auditEventGVR(event),
				"verb", event.Verb,
			)
		})
		joinerLog.V(1).Info(
			"Dropped additional audit event without request or response body",
			"auditID", auditID,
			"gvr", auditEventGVR(event),
			"verb", event.Verb,
			"quality", quality,
		)
		return AuditJoinDecision{Action: AuditJoinActionDrop}, nil
	}

	state, exists, err := j.peekDecisionState(ctx, auditID)
	if err != nil {
		return AuditJoinDecision{}, err
	}
	if exists {
		if state == "emitted" {
			addBodyLateMetric(ctx, event)
		} else {
			addDuplicateMetric(ctx, "in_flight_claim")
		}
		return AuditJoinDecision{Action: AuditJoinActionDrop}, nil
	}

	if err := j.parkBody(ctx, event); err != nil {
		return AuditJoinDecision{}, err
	}
	addParkedMetric(ctx, "additional_body")
	j.firsts.additionalBodyParked.Do(func() {
		joinerLog.Info("First additional audit body parked (awaiting matching official event)",
			"auditID", auditID,
			"gvr", auditEventGVR(event),
			"verb", event.Verb)
	})
	return AuditJoinDecision{Action: AuditJoinActionParked}, nil
}

func (j *RedisAuditEventJoiner) claimDecision(ctx context.Context, auditID string) (bool, error) {
	now := metav1.NewTime(j.now().UTC())
	envelope := auditDecisionEnvelope{
		Version:   1,
		State:     "claimed",
		ClaimedAt: now,
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return false, fmt.Errorf("failed to marshal audit decision claim for %q: %w", auditID, err)
	}
	_, err = j.client.SetArgs(ctx, decisionKey(auditID), payload, redis.SetArgs{
		Mode: "NX",
		TTL:  j.decisionTTL,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to claim audit decision for %q: %w", auditID, err)
	}
	return true, nil
}

func (j *RedisAuditEventJoiner) peekDecisionState(ctx context.Context, auditID string) (string, bool, error) {
	raw, err := j.client.Get(ctx, decisionKey(auditID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("failed to read audit decision for %q: %w", auditID, err)
	}
	var envelope auditDecisionEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", false, fmt.Errorf("failed to decode audit decision for %q: %w", auditID, err)
	}
	return envelope.State, true, nil
}

func (j *RedisAuditEventJoiner) parkBody(ctx context.Context, event *auditv1.Event) error {
	envelope := bodyEnvelopeFromEvent(event, j.now().UTC())
	payload, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("failed to marshal parked audit body for %q: %w", event.AuditID, err)
	}
	if err := j.client.Set(ctx, bodyKey(string(event.AuditID)), payload, j.bodyTTL).Err(); err != nil {
		return fmt.Errorf("failed to park audit body for %q: %w", event.AuditID, err)
	}
	return nil
}

func (j *RedisAuditEventJoiner) peekBody(ctx context.Context, auditID string) (AuditBodyEnvelope, bool, error) {
	raw, err := j.client.Get(ctx, bodyKey(auditID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return AuditBodyEnvelope{}, false, nil
	}
	if err != nil {
		return AuditBodyEnvelope{}, false, fmt.Errorf("failed to read parked audit body for %q: %w", auditID, err)
	}
	var envelope AuditBodyEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return AuditBodyEnvelope{}, false, fmt.Errorf("failed to decode parked audit body for %q: %w", auditID, err)
	}
	return envelope, true, nil
}

func (j *RedisAuditEventJoiner) deleteBody(ctx context.Context, auditID string) error {
	return j.client.Del(ctx, bodyKey(auditID)).Err()
}

func bodyEnvelopeFromEvent(event *auditv1.Event, receivedAt time.Time) AuditBodyEnvelope {
	return AuditBodyEnvelope{
		Version:           1,
		AuditID:           string(event.AuditID),
		ReceivedAt:        metav1.NewTime(receivedAt),
		RequestObject:     rawExtensionFromUnknown(event.RequestObject),
		ResponseObject:    rawExtensionFromUnknown(event.ResponseObject),
		ObjectRef:         event.ObjectRef.DeepCopy(),
		Annotations:       copyProxyAnnotations(event.Annotations),
		HasRequestObject:  hasRuntimeUnknownBody(event.RequestObject),
		HasResponseObject: hasRuntimeUnknownBody(event.ResponseObject),
	}
}

func mergeParkedBody(event *auditv1.Event, envelope AuditBodyEnvelope) *auditv1.Event {
	merged := event.DeepCopy()
	mergeParkedObjects(merged, envelope)
	mergeParkedObjectRef(merged, envelope)
	mergeParkedAnnotations(merged, envelope)
	return merged
}

func mergeParkedObjects(merged *auditv1.Event, envelope AuditBodyEnvelope) {
	if merged.Verb == "delete" || merged.Verb == "deletecollection" {
		return
	}
	// Official audit is authority for bodies; parked contribution only fills gaps.
	if !hasRuntimeUnknownBody(merged.RequestObject) &&
		envelope.RequestObject != nil && len(envelope.RequestObject.Raw) > 0 {
		merged.RequestObject = &runtime.Unknown{Raw: append([]byte(nil), envelope.RequestObject.Raw...)}
	}
	if !hasRuntimeUnknownBody(merged.ResponseObject) &&
		envelope.ResponseObject != nil && len(envelope.ResponseObject.Raw) > 0 {
		merged.ResponseObject = &runtime.Unknown{Raw: append([]byte(nil), envelope.ResponseObject.Raw...)}
	}
}

func mergeParkedObjectRef(merged *auditv1.Event, envelope AuditBodyEnvelope) {
	if merged.ObjectRef != nil && envelope.ObjectRef != nil {
		if envelope.ObjectRef.Name != "" {
			merged.ObjectRef.Name = envelope.ObjectRef.Name
		}
		if envelope.ObjectRef.Namespace != "" {
			merged.ObjectRef.Namespace = envelope.ObjectRef.Namespace
		}
		if envelope.ObjectRef.UID != "" {
			merged.ObjectRef.UID = envelope.ObjectRef.UID
		}
		if envelope.ObjectRef.ResourceVersion != "" {
			merged.ObjectRef.ResourceVersion = envelope.ObjectRef.ResourceVersion
		}
	}
}

func mergeParkedAnnotations(merged *auditv1.Event, envelope AuditBodyEnvelope) {
	if len(envelope.Annotations) > 0 {
		if merged.Annotations == nil {
			merged.Annotations = map[string]string{}
		}
		for key, value := range envelope.Annotations {
			merged.Annotations[key] = value
		}
	}
}

func rawExtensionFromUnknown(object *runtime.Unknown) *runtime.RawExtension {
	if !hasRuntimeUnknownBody(object) {
		return nil
	}
	return &runtime.RawExtension{Raw: append([]byte(nil), object.Raw...)}
}

func copyProxyAnnotations(annotations map[string]string) map[string]string {
	if len(annotations) == 0 {
		return nil
	}
	copied := map[string]string{}
	for key, value := range annotations {
		if strings.HasPrefix(key, "audit.k8s.io/proxy.") {
			copied[key] = value
		}
	}
	if len(copied) == 0 {
		return nil
	}
	return copied
}

func bodyKey(auditID string) string {
	return "audit:body:v1:" + auditID
}

func decisionKey(auditID string) string {
	return "audit:decision:v1:" + auditID
}

func classifyAuditEventQuality(source AuditSource, event *auditv1.Event) AuditEventQuality {
	if source == AuditSourceAdditional && !hasAuditV1ObjectBody(event) {
		return AuditEventQualityMalformed
	}
	if event == nil {
		return AuditEventQualityIdentityShallow
	}
	if event.Verb == "deletecollection" && hasAuditV1ObjectBody(event) &&
		event.ObjectRef != nil && event.ObjectRef.Resource != "" {
		return AuditEventQualityCollection
	}
	if hasAuditV1ObjectBody(event) {
		return AuditEventQualityComplete
	}
	if allowsBodylessAuditV1Delete(event) {
		return AuditEventQualityBodyShallowDeletable
	}
	return AuditEventQualityIdentityShallow
}

func allowsBodylessAuditV1Delete(event *auditv1.Event) bool {
	return event != nil &&
		event.Verb == "delete" &&
		event.ObjectRef != nil &&
		event.ObjectRef.Resource != "" &&
		event.ObjectRef.Name != ""
}

func auditEventGVR(event *auditv1.Event) string {
	if event == nil || event.ObjectRef == nil || event.ObjectRef.APIVersion == "" {
		return "unknown/unknown/unknown"
	}
	group, version, found := strings.Cut(event.ObjectRef.APIVersion, "/")
	if !found {
		version = event.ObjectRef.APIVersion
		group = event.ObjectRef.APIGroup
	}
	resource := event.ObjectRef.Resource
	if resource == "" {
		resource = "unknown"
	}
	if group == "" {
		return fmt.Sprintf("/%s/%s", version, resource)
	}
	return fmt.Sprintf("%s/%s/%s", group, version, resource)
}

func addQualityMetric(ctx context.Context, source AuditSource, event *auditv1.Event, quality AuditEventQuality) {
	if telemetry.AuditEventQualityTotal != nil {
		telemetry.AuditEventQualityTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("source", string(source)),
			attribute.String("quality", string(quality)),
			attribute.String("gvr", auditEventGVR(event)),
			attribute.String("action", eventVerb(event)),
		))
	}
}

func eventVerb(event *auditv1.Event) string {
	if event == nil {
		return ""
	}
	return event.Verb
}

func addParkedMetric(ctx context.Context, parkedKind string) {
	if telemetry.AuditJoinParkedTotal != nil {
		telemetry.AuditJoinParkedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("parked_kind", parkedKind)))
	}
}

func addEmittedMetric(ctx context.Context, source AuditSource, result AuditJoinResult) {
	if telemetry.AuditJoinEmittedTotal != nil {
		telemetry.AuditJoinEmittedTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("source", string(source)),
			attribute.String("result", string(result)),
		))
	}
}

func addDuplicateMetric(ctx context.Context, reason string) {
	if telemetry.AuditJoinDuplicateDroppedTotal != nil {
		telemetry.AuditJoinDuplicateDroppedTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("reason", reason),
		))
	}
}

func addShallowDroppedMetric(ctx context.Context, event *auditv1.Event) {
	if telemetry.AuditShallowDroppedTotal != nil {
		telemetry.AuditShallowDroppedTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("gvr", auditEventGVR(event)),
			attribute.String("action", eventVerb(event)),
		))
	}
}

func addBodyLateMetric(ctx context.Context, event *auditv1.Event) {
	if telemetry.AuditJoinBodyLateTotal != nil {
		telemetry.AuditJoinBodyLateTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("gvr", auditEventGVR(event)),
			attribute.String("action", eventVerb(event)),
		))
	}
}

const (
	// joinArrivalBodyFirst marks a join where the additional body was parked before the
	// official event arrived (the common, healthy case).
	joinArrivalBodyFirst = "body_first"
	// joinArrivalOfficialFirst marks a join where the official event arrived first and had
	// to wait on the canonical gate for the additional body.
	joinArrivalOfficialFirst = "official_first"

	// joinOutcomeMerged marks a skew sample where the official event was merged with a body.
	joinOutcomeMerged = "merged"
	// joinOutcomeTimedOut marks a skew sample where the wait expired before a body arrived.
	joinOutcomeTimedOut = "timed_out"
)

// observeJoinSkew records the arrival skew between an official audit event and its matching
// additional body. For body_first samples this is the proxy's lead time; for official_first
// samples it is how long the official event waited on the canonical gate. Negative samples
// (clock jitter) are clamped to zero so they land in the first bucket.
func observeJoinSkew(ctx context.Context, arrival, outcome string, seconds float64) {
	if telemetry.AuditJoinSkewSeconds == nil {
		return
	}
	if seconds < 0 {
		seconds = 0
	}
	telemetry.AuditJoinSkewSeconds.Record(ctx, seconds, metric.WithAttributes(
		attribute.String("arrival", arrival),
		attribute.String("outcome", outcome),
	))
}

// observeOfficialGateWait records how long an official audit event waited to acquire the
// in-pod canonical ordering gate before processing.
func observeOfficialGateWait(ctx context.Context, seconds float64) {
	if telemetry.AuditOfficialGateWaitSeconds == nil {
		return
	}
	if seconds < 0 {
		seconds = 0
	}
	telemetry.AuditOfficialGateWaitSeconds.Record(ctx, seconds)
}
