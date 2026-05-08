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

	// AuditJoinModeWaitOfficial parks additional bodies until the official event arrives.
	AuditJoinModeWaitOfficial AuditJoinMode = "wait-official"
	// AuditJoinModeFirst emits whichever usable event claims the audit ID first.
	AuditJoinModeFirst AuditJoinMode = "first"

	auditBodySourceProxy = "apiservice-audit-proxy"

	defaultAuditEventBodyTTL     = 5 * time.Minute
	defaultAuditEventDecisionTTL = time.Hour
)

// AuditSource is the semantic source role of an audit webhook endpoint.
type AuditSource string

// AuditJoinMode controls whether the joiner waits for official events or emits the first arrival.
type AuditJoinMode string

// AuditJoinAction describes how the handler should proceed after a join decision.
type AuditJoinAction int

const (
	// AuditJoinActionParked acknowledges a parked body contribution without a stream write.
	AuditJoinActionParked AuditJoinAction = iota
	// AuditJoinActionEmit asks the handler to enqueue Event and then commit or release the decision.
	AuditJoinActionEmit
	// AuditJoinActionDropDuplicate acknowledges an event that should not be emitted.
	AuditJoinActionDropDuplicate
)

// AuditJoinResult is stored on committed decisions and emitted as a metric label.
type AuditJoinResult string

const (
	// AuditJoinResultAsIs means the event was emitted without a parked body contribution.
	AuditJoinResultAsIs AuditJoinResult = "as_is"
	// AuditJoinResultMerged means an official event was merged with a parked body contribution.
	AuditJoinResultMerged AuditJoinResult = "merged"
	// AuditJoinResultFirst means the first arrival won in first mode.
	AuditJoinResultFirst AuditJoinResult = "first"
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
	Mode    AuditJoinMode
}

// AuditEventJoiner decides whether incoming audit events should park, emit, or drop.
type AuditEventJoiner interface {
	Decide(ctx context.Context, source AuditSource, event *auditv1.Event) (AuditJoinDecision, error)
	CommitDecision(ctx context.Context, auditID string, result AuditJoinResult) error
	ReleaseDecision(ctx context.Context, auditID string) error
}

// AuditBodyEnvelope is the parked contribution shape stored under audit:body:v1:<auditID>.
type AuditBodyEnvelope struct {
	Version           int                      `json:"v"`
	AuditID           string                   `json:"auditID"`
	Source            string                   `json:"source"`
	ReceivedAt        metav1.Time              `json:"receivedAt"`
	Event             *auditv1.Event           `json:"event,omitempty"`
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
	Mode      AuditJoinMode   `json:"mode"`
	ClaimedAt metav1.Time     `json:"claimedAt"`
	EmittedAt *metav1.Time    `json:"emittedAt,omitempty"`
	Result    AuditJoinResult `json:"result,omitempty"`
}

// RedisAuditJoinerConfig configures Redis-backed audit body parking and decision tracking.
type RedisAuditJoinerConfig struct {
	Addr                 string
	Username             string
	AuthValue            string
	DB                   int
	TLSEnabled           bool
	Mode                 AuditJoinMode
	BodyTTL              time.Duration
	DecisionTTL          time.Duration
	BodyParkingAPIGroups []string
	AdditionalOnly       bool
}

// RedisAuditEventJoiner implements audit body parking with Redis/Valkey keys.
type RedisAuditEventJoiner struct {
	client         *redis.Client
	mode           AuditJoinMode
	bodyTTL        time.Duration
	decisionTTL    time.Duration
	allowGroups    map[string]struct{}
	additionalOnly bool
	now            func() time.Time
}

// NewRedisAuditEventJoiner creates a Redis-backed AuditEventJoiner.
func NewRedisAuditEventJoiner(cfg RedisAuditJoinerConfig) (*RedisAuditEventJoiner, error) {
	if strings.TrimSpace(cfg.Addr) == "" {
		return nil, errors.New("redis address is required")
	}
	mode := cfg.Mode
	if mode == "" {
		mode = AuditJoinModeWaitOfficial
	}
	if mode != AuditJoinModeWaitOfficial && mode != AuditJoinModeFirst {
		return nil, fmt.Errorf("invalid audit event join mode %q", mode)
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
		mode:           mode,
		bodyTTL:        bodyTTL,
		decisionTTL:    decisionTTL,
		allowGroups:    buildAPIGroupSet(cfg.BodyParkingAPIGroups),
		additionalOnly: cfg.AdditionalOnly,
		now:            time.Now,
	}, nil
}

// Decide examines an event and, for emitted events, claims the audit ID before enqueueing.
func (j *RedisAuditEventJoiner) Decide(
	ctx context.Context,
	source AuditSource,
	event *auditv1.Event,
) (AuditJoinDecision, error) {
	if event == nil {
		return AuditJoinDecision{}, errors.New("audit event is nil")
	}
	auditID := string(event.AuditID)
	if strings.TrimSpace(auditID) == "" {
		return AuditJoinDecision{}, errors.New("auditID cannot be empty")
	}

	group := auditEventAPIGroup(event)
	allowlisted := j.isAllowlisted(group)
	if source == AuditSourceAdditional && !allowlisted && !j.additionalOnly {
		addUnexpectedMetric(ctx, group)
		logf.Log.WithName("audit-joiner").Info(
			"Dropped unexpected additional audit event for non-allowlisted API group",
			"auditID", auditID,
			"apiGroup", group,
			"mode", j.mode,
		)
		return AuditJoinDecision{Action: AuditJoinActionDropDuplicate}, nil
	}

	if j.additionalOnly {
		return j.claimForEmit(ctx, source, event, AuditJoinResultAdditionalOnly)
	}
	if j.mode == AuditJoinModeFirst {
		return j.claimForEmit(ctx, source, event, AuditJoinResultFirst)
	}

	if source == AuditSourceAdditional {
		return j.handleAdditionalWait(ctx, event, group)
	}

	return j.claimOfficialWait(ctx, event, allowlisted)
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
		Mode:      j.mode,
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
		addDuplicateMetric(ctx, j.mode)
		return AuditJoinDecision{Action: AuditJoinActionDropDuplicate}, nil
	}
	return AuditJoinDecision{
		Action:  AuditJoinActionEmit,
		Event:   event,
		AuditID: auditID,
		Result:  result,
		Source:  source,
		Mode:    j.mode,
	}, nil
}

func (j *RedisAuditEventJoiner) claimOfficialWait(
	ctx context.Context,
	event *auditv1.Event,
	allowlisted bool,
) (AuditJoinDecision, error) {
	auditID := string(event.AuditID)

	envelope, found, err := j.peekBody(ctx, auditID)
	if err != nil {
		return AuditJoinDecision{}, err
	}
	if found && envelope.Source == string(AuditSourceOfficial) {
		addDuplicateMetric(ctx, j.mode)
		return AuditJoinDecision{Action: AuditJoinActionDropDuplicate}, nil
	}
	if !found && shouldParkOfficialUntilAdditional(event, allowlisted) {
		if err := j.parkBody(ctx, AuditSourceOfficial, event); err != nil {
			return AuditJoinDecision{}, err
		}
		addParkedMetric(ctx)
		logf.Log.WithName("audit-joiner").Info(
			"Parked shallow official audit event until additional body arrives",
			"auditID", auditID,
			"apiGroup", auditEventAPIGroup(event),
			"mode", j.mode,
		)
		return AuditJoinDecision{Action: AuditJoinActionParked}, nil
	}

	claimed, err := j.claimDecision(ctx, auditID)
	if err != nil {
		return AuditJoinDecision{}, err
	}
	if !claimed {
		addDuplicateMetric(ctx, j.mode)
		return AuditJoinDecision{Action: AuditJoinActionDropDuplicate}, nil
	}

	result := AuditJoinResultAsIs
	emitEvent := event
	if found {
		if err := j.deleteBody(ctx, auditID); err != nil {
			logf.Log.WithName("audit-joiner").
				Error(err, "Failed to delete merged parked audit body", "auditID", auditID)
		}
		emitEvent = mergeParkedBody(event, envelope)
		result = AuditJoinResultMerged
	} else if allowlisted {
		addBodyMissMetric(ctx, j.mode)
	}

	return AuditJoinDecision{
		Action:  AuditJoinActionEmit,
		Event:   emitEvent,
		AuditID: auditID,
		Result:  result,
		Source:  AuditSourceOfficial,
		Mode:    j.mode,
	}, nil
}

func (j *RedisAuditEventJoiner) handleAdditionalWait(
	ctx context.Context,
	event *auditv1.Event,
	group string,
) (AuditJoinDecision, error) {
	auditID := string(event.AuditID)
	if !hasAuditV1ObjectBody(event) {
		logf.Log.WithName("audit-joiner").Info(
			"Dropped additional audit event without request or response body",
			"auditID", auditID,
			"apiGroup", group,
			"mode", j.mode,
		)
		return AuditJoinDecision{Action: AuditJoinActionDropDuplicate}, nil
	}

	envelope, found, err := j.peekBody(ctx, auditID)
	if err != nil {
		return AuditJoinDecision{}, err
	}
	if found && envelope.Source == string(AuditSourceOfficial) {
		return j.claimAdditionalWithOfficial(ctx, event, envelope)
	}

	if err := j.parkBody(ctx, AuditSourceAdditional, event); err != nil {
		return AuditJoinDecision{}, err
	}
	addParkedMetric(ctx)
	return AuditJoinDecision{Action: AuditJoinActionParked}, nil
}

func (j *RedisAuditEventJoiner) claimAdditionalWithOfficial(
	ctx context.Context,
	event *auditv1.Event,
	envelope AuditBodyEnvelope,
) (AuditJoinDecision, error) {
	auditID := string(event.AuditID)
	claimed, err := j.claimDecision(ctx, auditID)
	if err != nil {
		return AuditJoinDecision{}, err
	}
	if !claimed {
		addDuplicateMetric(ctx, j.mode)
		return AuditJoinDecision{Action: AuditJoinActionDropDuplicate}, nil
	}
	if err := j.deleteBody(ctx, auditID); err != nil {
		logf.Log.WithName("audit-joiner").Error(err, "Failed to delete merged parked audit body", "auditID", auditID)
	}

	official := envelope.Event
	if official == nil {
		official = event
	}
	additionalEnvelope := bodyEnvelopeFromEvent(event, j.now().UTC(), AuditSourceAdditional)
	return AuditJoinDecision{
		Action:  AuditJoinActionEmit,
		Event:   mergeParkedBody(official, additionalEnvelope),
		AuditID: auditID,
		Result:  AuditJoinResultMerged,
		Source:  AuditSourceOfficial,
		Mode:    j.mode,
	}, nil
}

func (j *RedisAuditEventJoiner) claimDecision(ctx context.Context, auditID string) (bool, error) {
	now := metav1.NewTime(j.now().UTC())
	envelope := auditDecisionEnvelope{
		Version:   1,
		State:     "claimed",
		Mode:      j.mode,
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

func (j *RedisAuditEventJoiner) parkBody(ctx context.Context, source AuditSource, event *auditv1.Event) error {
	envelope := bodyEnvelopeFromEvent(event, j.now().UTC(), source)
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

func bodyEnvelopeFromEvent(event *auditv1.Event, receivedAt time.Time, source AuditSource) AuditBodyEnvelope {
	envelope := AuditBodyEnvelope{
		Version:           1,
		AuditID:           string(event.AuditID),
		Source:            string(source),
		ReceivedAt:        metav1.NewTime(receivedAt),
		RequestObject:     rawExtensionFromUnknown(event.RequestObject),
		ResponseObject:    rawExtensionFromUnknown(event.ResponseObject),
		ObjectRef:         event.ObjectRef.DeepCopy(),
		Annotations:       copyProxyAnnotations(event.Annotations),
		HasRequestObject:  hasRuntimeUnknownBody(event.RequestObject),
		HasResponseObject: hasRuntimeUnknownBody(event.ResponseObject),
	}
	if source == AuditSourceOfficial {
		envelope.Event = event.DeepCopy()
	}
	if source == AuditSourceAdditional {
		envelope.Source = auditBodySourceProxy
	}
	return envelope
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
	if envelope.RequestObject != nil && len(envelope.RequestObject.Raw) > 0 {
		merged.RequestObject = &runtime.Unknown{Raw: append([]byte(nil), envelope.RequestObject.Raw...)}
	}
	if envelope.ResponseObject != nil && len(envelope.ResponseObject.Raw) > 0 {
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

func shouldParkOfficialUntilAdditional(event *auditv1.Event, allowlisted bool) bool {
	if !allowlisted || hasAuditV1ObjectBody(event) {
		return false
	}
	if event.ObjectRef == nil {
		return true
	}
	return event.ObjectRef.Name == ""
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

func (j *RedisAuditEventJoiner) isAllowlisted(group string) bool {
	_, ok := j.allowGroups[strings.TrimSpace(group)]
	return ok
}

func buildAPIGroupSet(groups []string) map[string]struct{} {
	result := map[string]struct{}{}
	for _, group := range groups {
		group = strings.TrimSpace(group)
		if group != "" {
			result[group] = struct{}{}
		}
	}
	return result
}

func auditEventAPIGroup(event *auditv1.Event) string {
	if event == nil || event.ObjectRef == nil {
		return ""
	}
	return event.ObjectRef.APIGroup
}

func bodyKey(auditID string) string {
	return "audit:body:v1:" + auditID
}

func decisionKey(auditID string) string {
	return "audit:decision:v1:" + auditID
}

func addParkedMetric(ctx context.Context) {
	if telemetry.AuditJoinParkedTotal != nil {
		telemetry.AuditJoinParkedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("source", "body")))
	}
}

func addEmittedMetric(ctx context.Context, source AuditSource, mode AuditJoinMode, result AuditJoinResult) {
	if telemetry.AuditJoinEmittedTotal != nil {
		telemetry.AuditJoinEmittedTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("source", string(source)),
			attribute.String("mode", string(mode)),
			attribute.String("result", string(result)),
		))
	}
}

func addDuplicateMetric(ctx context.Context, mode AuditJoinMode) {
	if telemetry.AuditJoinDuplicateDroppedTotal != nil {
		telemetry.AuditJoinDuplicateDroppedTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("mode", string(mode)),
		))
	}
}

func addBodyMissMetric(ctx context.Context, mode AuditJoinMode) {
	if telemetry.AuditJoinBodyMissTotal != nil {
		telemetry.AuditJoinBodyMissTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("mode", string(mode))))
	}
}

func addUnexpectedMetric(ctx context.Context, group string) {
	if telemetry.AuditJoinBodyUnexpectedTotal != nil {
		if strings.TrimSpace(group) == "" {
			group = "unknown"
		}
		telemetry.AuditJoinBodyUnexpectedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("group", group)))
	}
}
