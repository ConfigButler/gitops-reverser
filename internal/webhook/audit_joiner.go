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
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

const (
	// AuditSourceOfficial identifies events received from the kube-apiserver audit webhook.
	AuditSourceOfficial AuditSource = "official"
	// AuditSourceAdditional identifies events received from the supplementary audit webhook.
	AuditSourceAdditional AuditSource = "additional"

	defaultAuditEventBodyTTL     = 5 * time.Minute
	defaultAuditEventDecisionTTL = time.Hour
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
	// AuditJoinResultAdditionalOnly means the additional stream is configured as canonical.
	AuditJoinResultAdditionalOnly AuditJoinResult = "additional_only"
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
	Addr           string
	Username       string
	AuthValue      string
	DB             int
	TLSEnabled     bool
	BodyTTL        time.Duration
	DecisionTTL    time.Duration
	AdditionalOnly bool
}

// RedisAuditEventJoiner implements audit body parking with Redis/Valkey keys.
type RedisAuditEventJoiner struct {
	client         *redis.Client
	bodyTTL        time.Duration
	decisionTTL    time.Duration
	additionalOnly bool
	now            func() time.Time
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
		client:         redis.NewClient(options),
		bodyTTL:        bodyTTL,
		decisionTTL:    decisionTTL,
		additionalOnly: cfg.AdditionalOnly,
		now:            time.Now,
	}, nil
}

// Decide examines an event and, for emitted events, claims the audit ID before enqueueing.
// Officials never park: an identity-shallow official with no matching parked body drops
// synchronously with audit_shallow_dropped_total + a WARN log.
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

	if j.additionalOnly {
		return j.claimForEmit(ctx, source, event, AuditJoinResultAdditionalOnly)
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

func (j *RedisAuditEventJoiner) claimForEmit(
	ctx context.Context,
	source AuditSource,
	event *auditv1.Event,
	result AuditJoinResult,
) (AuditJoinDecision, error) {
	auditID := string(event.AuditID)
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
		Result:  result,
		Source:  source,
	}, nil
}

func (j *RedisAuditEventJoiner) handleOfficial(
	ctx context.Context,
	event *auditv1.Event,
	quality AuditEventQuality,
) (AuditJoinDecision, error) {
	auditID := string(event.AuditID)

	envelope, hasBody, err := j.peekBody(ctx, auditID)
	if err != nil {
		return AuditJoinDecision{}, err
	}

	if !hasBody && quality == AuditEventQualityIdentityShallow {
		addShallowDroppedMetric(ctx, event)
		logf.Log.WithName("audit-joiner").Info(
			"audit shallow event dropped: install apiservice-audit-proxy or update kube-apiserver "+
				"audit policy to include request/response bodies",
			"auditID", auditID,
			"gvr", auditEventGVR(event),
			"verb", event.Verb,
			"source", string(AuditSourceOfficial),
			"quality", quality,
			"hasRequestObject", hasRuntimeUnknownBody(event.RequestObject),
			"hasResponseObject", hasRuntimeUnknownBody(event.ResponseObject),
		)
		return AuditJoinDecision{Action: AuditJoinActionDrop}, nil
	}

	claimed, err := j.claimDecision(ctx, auditID)
	if err != nil {
		return AuditJoinDecision{}, err
	}
	if !claimed {
		addDuplicateMetric(ctx, "decision_exists")
		return AuditJoinDecision{Action: AuditJoinActionDrop}, nil
	}

	if hasBody {
		if err := j.deleteBody(ctx, auditID); err != nil {
			logf.Log.WithName("audit-joiner").
				Error(err, "Failed to delete merged parked audit body", "auditID", auditID)
		}
		return AuditJoinDecision{
			Action:  AuditJoinActionEmit,
			Event:   mergeParkedBody(event, envelope),
			AuditID: auditID,
			Result:  AuditJoinResultMerged,
			Source:  AuditSourceOfficial,
		}, nil
	}

	return AuditJoinDecision{
		Action:  AuditJoinActionEmit,
		Event:   event,
		AuditID: auditID,
		Result:  AuditJoinResultAsIs,
		Source:  AuditSourceOfficial,
	}, nil
}

func (j *RedisAuditEventJoiner) handleAdditional(
	ctx context.Context,
	event *auditv1.Event,
	quality AuditEventQuality,
) (AuditJoinDecision, error) {
	auditID := string(event.AuditID)
	if quality == AuditEventQualityMalformed {
		logf.Log.WithName("audit-joiner").Info(
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
