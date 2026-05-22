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
	authnv1 "k8s.io/api/authentication/v1"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/auditutil"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

const (
	// defaultConsumerGroup is the consumer group name used for the audit stream.
	defaultConsumerGroup = "gitopsreverser-consumer"

	// consumerReadCount is the maximum number of messages fetched per XREADGROUP call.
	consumerReadCount = 50

	// consumerBlockDuration is how long XREADGROUP blocks waiting for new messages.
	consumerBlockDuration = 2 * time.Second

	// autoClaimMinIdle is the minimum idle time before a pending entry is reclaimed.
	autoClaimMinIdle = 60 * time.Second

	// autoClaimInterval controls how often XAUTOCLAIM runs.
	autoClaimInterval = 30 * time.Second

	// consumerRetryDelay controls how long the consumer waits before retrying Redis setup or reads.
	consumerRetryDelay = time.Second
)

var errAuditEventObjectMissing = errors.New("audit event has no requestObject or responseObject")

// errAuditEventObjectIsStatus marks an audit event whose extracted body is a
// metav1.Status error response (apiVersion: v1, kind: Status) rather than a
// real resource. The API server emits such a body when a request fails — most
// commonly a 409 Conflict from an optimistic-concurrency clash. The ingress
// gate in internal/webhook drops failed requests by responseStatus.code, so a
// well-formed pipeline never reaches here; this is a defense-in-depth guard
// against an event that lost its responseStatus (e.g. an additional-source
// proxy body) so the Status is never written to Git as if it were the resource.
var errAuditEventObjectIsStatus = errors.New("audit event object is a metav1.Status error body")

// AuditEventRouter is the subset of watch.EventRouter used by the consumer.
// watch.EventRouter satisfies this interface without modification.
type AuditEventRouter interface {
	RouteToGitTargetEventStream(event git.Event, gitDest itypes.ResourceReference) error
	FinalizeGitTargetWindow(
		ctx context.Context,
		author, gitTargetName, gitTargetNamespace, message string,
	) (git.FinalizeResult, error)
}

// AuditConsumerConfig configures the Redis stream consumer.
type AuditConsumerConfig struct {
	Addr       string
	Username   string
	AuthValue  string
	DB         int
	Stream     string
	TLSEnabled bool

	// Group is the Redis consumer group name. Defaults to defaultConsumerGroup.
	Group string

	// ConsumerID uniquely identifies this consumer replica (e.g. pod name).
	ConsumerID string
}

// AuditConsumer reads audit events from a Redis stream and routes them into
// the existing git write pipeline via EventRouter.
type AuditConsumer struct {
	client      *redis.Client
	stream      string
	group       string
	consumerID  string
	ruleStore   *rulestore.RuleStore
	eventRouter AuditEventRouter
	log         logr.Logger

	// kubeClient writes CommitRequest status subresources. apiReader performs
	// uncached reads of CommitRequest objects so a freshly-created object is
	// visible even before the controller-runtime cache has synced it. Both may
	// be nil, in which case CommitRequest handling is disabled.
	kubeClient client.Client
	apiReader  client.Reader

	// One-shot log gates for startup-milestone visibility.
	firstGroupReady     sync.Once
	firstMessage        sync.Once
	firstRouted         sync.Once
	firstShallowDropped sync.Once
	firstStatusDropped  sync.Once
}

// NewAuditConsumer creates a new AuditConsumer. It does not start consuming;
// call Start to begin the consume loop. kubeClient and apiReader are used for
// CommitRequest handling and may be nil to disable it.
func NewAuditConsumer(
	cfg AuditConsumerConfig,
	ruleStore *rulestore.RuleStore,
	eventRouter AuditEventRouter,
	kubeClient client.Client,
	apiReader client.Reader,
	log logr.Logger,
) (*AuditConsumer, error) {
	if strings.TrimSpace(cfg.Addr) == "" {
		return nil, errors.New("redis address is required")
	}

	stream := strings.TrimSpace(cfg.Stream)
	if stream == "" {
		stream = DefaultRedisAuditStream
	}

	group := strings.TrimSpace(cfg.Group)
	if group == "" {
		group = defaultConsumerGroup
	}

	consumerID := strings.TrimSpace(cfg.ConsumerID)
	if consumerID == "" {
		consumerID = "gitopsreverser-consumer-0"
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

	return &AuditConsumer{
		client:      redis.NewClient(options),
		stream:      stream,
		group:       group,
		consumerID:  consumerID,
		ruleStore:   ruleStore,
		eventRouter: eventRouter,
		log:         log.WithName("audit-consumer"),
		kubeClient:  kubeClient,
		apiReader:   apiReader,
	}, nil
}

// NeedLeaderElection returns true so only the elected leader runs the consumer.
func (c *AuditConsumer) NeedLeaderElection() bool {
	return true
}

// Start implements manager.Runnable. It bootstraps the consumer group and
// enters the read loop until ctx is cancelled.
func (c *AuditConsumer) Start(ctx context.Context) error {
	c.log.Info("Starting audit stream consumer",
		"stream", c.stream,
		"group", c.group,
		"consumer", c.consumerID)

	for {
		if err := c.ensureConsumerGroup(ctx); err != nil {
			c.log.Error(err, "Failed to ensure consumer group, retrying")
			select {
			case <-ctx.Done():
				c.log.Info("Audit stream consumer stopping before consumer group became ready")
				return nil
			case <-time.After(consumerRetryDelay):
			}
			continue
		}
		break
	}

	autoClaimTicker := time.NewTicker(autoClaimInterval)
	defer autoClaimTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.log.Info("Audit stream consumer stopping")
			return nil
		case <-autoClaimTicker.C:
			c.runAutoClaimCycle(ctx)
		default:
			if err := c.readAndProcessBatch(ctx); err != nil {
				// Log but do not exit — transient Redis errors should not crash the process.
				c.log.Error(err, "Error reading from audit stream, retrying")
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(consumerRetryDelay):
				}
			}
		}
	}
}

// ensureConsumerGroup creates the consumer group if it does not already exist.
// MKSTREAM also creates the stream itself when missing.
func (c *AuditConsumer) ensureConsumerGroup(ctx context.Context) error {
	err := c.client.XGroupCreateMkStream(ctx, c.stream, c.group, "$").Err()
	if err != nil && !isAlreadyExistsErr(err) {
		return err
	}
	c.firstGroupReady.Do(func() {
		c.log.Info("Consumer group ready", "stream", c.stream, "group", c.group)
	})
	return nil
}

// readAndProcessBatch fetches up to consumerReadCount messages from the stream
// and processes each one, ACKing only after successful routing.
func (c *AuditConsumer) readAndProcessBatch(ctx context.Context) error {
	streams, err := c.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    c.group,
		Consumer: c.consumerID,
		Streams:  []string{c.stream, ">"},
		Count:    consumerReadCount,
		Block:    consumerBlockDuration,
	}).Result()

	if errors.Is(err, redis.Nil) {
		// No new messages within the block window; this is normal.
		return nil
	}
	if err != nil {
		if isNoGroupErr(err) {
			if ensureErr := c.ensureConsumerGroup(ctx); ensureErr != nil {
				return fmt.Errorf("XREADGROUP failed with NOGROUP and group recreation failed: %w", ensureErr)
			}
			return nil
		}
		return fmt.Errorf("XREADGROUP failed: %w", err)
	}

	for _, stream := range streams {
		for _, msg := range stream.Messages {
			c.processMessage(ctx, msg)
		}
	}
	return nil
}

// runAutoClaimCycle reclaims pending entries that have been idle longer than
// autoClaimMinIdle, then processes them.
func (c *AuditConsumer) runAutoClaimCycle(ctx context.Context) {
	start := "0-0"
	for {
		messages, nextStart, err := c.client.XAutoClaim(ctx, &redis.XAutoClaimArgs{
			Stream:   c.stream,
			Group:    c.group,
			Consumer: c.consumerID,
			MinIdle:  autoClaimMinIdle,
			Start:    start,
			Count:    consumerReadCount,
		}).Result()

		if err != nil {
			if isNoGroupErr(err) {
				if ensureErr := c.ensureConsumerGroup(ctx); ensureErr != nil {
					c.log.Error(ensureErr, "XAUTOCLAIM failed with NOGROUP and group recreation failed")
					return
				}
				return
			}
			c.log.Error(err, "XAUTOCLAIM failed")
			return
		}

		for _, msg := range messages {
			c.processMessage(ctx, msg)
		}

		// "0-0" is returned when there are no more pending entries to scan.
		if nextStart == "0-0" || len(messages) == 0 {
			return
		}
		start = nextStart
	}
}

// processMessage handles a single stream entry: parses the audit event,
// matches rules, builds git.Event(s), routes them, and ACKs.
func (c *AuditConsumer) processMessage(ctx context.Context, msg redis.XMessage) {
	c.firstMessage.Do(func() {
		c.log.Info("First audit message consumed from stream", "msgID", msg.ID, "stream", c.stream)
	})
	log := c.log.WithValues("msgID", msg.ID)

	auditEvent, err := parseAuditEvent(msg.Values)
	if err != nil {
		log.Error(err, "Failed to parse audit event; skipping and ACKing to avoid poison-pill")
		c.ackMessage(ctx, msg.ID)
		return
	}

	// Only process ResponseComplete entries to avoid duplicates across stages.
	if auditEvent.Stage != auditv1.StageResponseComplete {
		c.ackMessage(ctx, msg.ID)
		return
	}

	// Only process mutating verbs.
	op, ok := auditutil.VerbToOperation(auditEvent.Verb)
	if !ok {
		c.ackMessage(ctx, msg.ID)
		return
	}

	if auditEvent.ObjectRef == nil {
		c.ackMessage(ctx, msg.ID)
		return
	}

	// CommitRequest create events drive the "finalize the open window now"
	// path. They are handled before rule matching and never flow into the
	// resource-write pipeline.
	if c.isCommitRequestCreate(auditEvent) {
		c.handleCommitRequest(ctx, log, auditEvent)
		c.ackMessage(ctx, msg.ID)
		return
	}

	if err := c.routeAuditEvent(ctx, log, auditEvent, op); err != nil {
		log.Error(err, "Failed to route audit event; ACKing to avoid poison-pill")
	}
	c.ackMessage(ctx, msg.ID)
}

// routeAuditEvent performs rule matching, object extraction, and routing for one audit event.
func (c *AuditConsumer) routeAuditEvent(
	ctx context.Context,
	log logr.Logger,
	auditEvent auditv1.Event,
	op configv1alpha1.OperationType,
) error {
	ref := auditEvent.ObjectRef
	apiGroup, apiVersion := auditutil.ObjectRefGroupVersion(ref)
	resourcePlural := ref.Resource
	namespace := ref.Namespace
	name := ref.Name
	isClusterScoped := namespace == ""
	op = effectiveAuditOperation(auditEvent, op)

	var matchObj *unstructured.Unstructured
	if !isClusterScoped {
		matchObj = &unstructured.Unstructured{}
		matchObj.SetNamespace(namespace)
		matchObj.SetName(name)
	}

	wrRules := c.ruleStore.GetMatchingRules(matchObj, resourcePlural, op, apiGroup, apiVersion, isClusterScoped)
	cwrRules := c.ruleStore.GetMatchingClusterRules(resourcePlural, op, apiGroup, apiVersion, isClusterScoped, nil)

	gvr := pipelineGVR{group: apiGroup, version: apiVersion, resource: resourcePlural}

	if len(wrRules) == 0 && len(cwrRules) == 0 {
		recordPipelineEvent(ctx, gvr, auditEvent.Verb, pipelineOutcomeUnmatched)
		return nil
	}

	fullAPIVersion := apiVersion
	if apiGroup != "" {
		fullAPIVersion = apiGroup + "/" + apiVersion
	}

	sanitized, err := extractObject(auditEvent, op, fullAPIVersion, ref.Resource, namespace, name)
	if err != nil {
		if c.handleExtractObjectError(
			log, auditEvent, err, fullAPIVersion+"/"+ref.Resource, namespace, name,
		) {
			recordPipelineEvent(ctx, gvr, auditEvent.Verb, pipelineOutcomeDroppedNoBody)
			return nil
		}
		return fmt.Errorf("extracting object for %s/%s: %w", namespace, name, err)
	}
	if namespace == "" {
		namespace = sanitized.GetNamespace()
	}
	if name == "" {
		name = sanitized.GetName()
	}

	id := itypes.NewResourceIdentifier(apiGroup, apiVersion, resourcePlural, namespace, name)
	userInfo := resolveUserInfo(auditEvent)

	routed := c.routeToMatchedRules(ctx, log, sanitized, id, op, userInfo, wrRules, cwrRules)

	if routed > 0 {
		recordPipelineEvent(ctx, gvr, auditEvent.Verb, pipelineOutcomeRouted)
	} else {
		recordPipelineEvent(ctx, gvr, auditEvent.Verb, pipelineOutcomeRouteFailed)
	}

	if routed > 0 {
		c.firstRouted.Do(func() {
			c.log.Info("First audit event routed to BranchWorker",
				"resource", resourcePlural,
				"namespace", namespace,
				"name", name,
				"operation", op,
				"routedTargets", routed)
		})
	}

	log.V(1).Info("Processed audit stream entry",
		"resource", resourcePlural, "namespace", namespace, "name", name,
		"operation", op, "user", userInfo.Username,
		"routed", routed)
	return nil
}

// handleExtractObjectError classifies an extractObject failure. For a benign
// drop — a shallow event with no body, or a metav1.Status error body from a
// failed API request — it logs and returns true so the caller acks the event
// without routing it. For any other error it returns false, leaving the caller
// to surface it.
func (c *AuditConsumer) handleExtractObjectError(
	log logr.Logger,
	auditEvent auditv1.Event,
	err error,
	gvr, namespace, name string,
) bool {
	switch {
	case errors.Is(err, errAuditEventObjectMissing):
		c.firstShallowDropped.Do(func() {
			c.log.Info(
				"First audit event dropped before git routing — missing requestObject/responseObject. "+
					"Install apiservice-audit-proxy or update kube-apiserver audit policy to include bodies. "+
					"Further drops will log at V(1) only.",
				"auditID", auditEvent.AuditID,
				"gvr", gvr,
				"verb", auditEvent.Verb,
			)
		})
		log.V(1).Info(
			"audit event dropped before git routing: missing requestObject/responseObject",
			"auditID", auditEvent.AuditID,
			"gvr", gvr,
			"verb", auditEvent.Verb,
			"namespace", namespace,
			"name", name,
			"hasRequestObject", hasAuditObjectRaw(auditEvent.RequestObject),
			"hasResponseObject", hasAuditObjectRaw(auditEvent.ResponseObject),
		)
		return true
	case errors.Is(err, errAuditEventObjectIsStatus):
		c.firstStatusDropped.Do(func() {
			c.log.Info(
				"First audit event dropped before git routing — body is a metav1.Status error "+
					"response, not a resource. This indicates a failed API request (e.g. a 409 "+
					"Conflict) that should have been filtered at ingress. Further drops will log "+
					"at V(1) only.",
				"auditID", auditEvent.AuditID,
				"gvr", gvr,
				"verb", auditEvent.Verb,
			)
		})
		log.V(1).Info(
			"audit event dropped before git routing: body is a metav1.Status error response",
			"auditID", auditEvent.AuditID,
			"gvr", gvr,
			"verb", auditEvent.Verb,
			"namespace", namespace,
			"name", name,
		)
		return true
	default:
		return false
	}
}

// routeToMatchedRules dispatches git.Events to all matched WatchRule and ClusterWatchRule targets.
// It returns the number of successfully routed events.
func (c *AuditConsumer) routeToMatchedRules(
	ctx context.Context,
	log logr.Logger,
	sanitized *unstructured.Unstructured,
	id itypes.ResourceIdentifier,
	op configv1alpha1.OperationType,
	userInfo git.UserInfo,
	wrRules []rulestore.CompiledRule,
	cwrRules []rulestore.CompiledClusterRule,
) int {
	routed := 0
	for _, rule := range wrRules {
		ev := buildGitEvent(sanitized, id, op, userInfo, rule.Path, rule.GitTargetRef, rule.GitTargetNamespace)
		gitDest := itypes.NewResourceReference(rule.GitTargetRef, rule.GitTargetNamespace)
		if err := c.eventRouter.RouteToGitTargetEventStream(ev, gitDest); err != nil {
			log.V(1).Info("Failed to route audit event via WatchRule", "error", err,
				"gitTarget", gitDest.String())
			recordRouteTarget(
				ctx,
				rule.GitTargetNamespace,
				rule.GitTargetRef,
				ruleKindWatchRule,
				pipelineOutcomeRouteFailed,
			)
			continue
		}
		recordRouteTarget(ctx, rule.GitTargetNamespace, rule.GitTargetRef, ruleKindWatchRule, pipelineOutcomeRouted)
		routed++
	}
	for _, rule := range cwrRules {
		ev := buildGitEvent(sanitized, id, op, userInfo, rule.Path, rule.GitTargetRef, rule.GitTargetNamespace)
		gitDest := itypes.NewResourceReference(rule.GitTargetRef, rule.GitTargetNamespace)
		if err := c.eventRouter.RouteToGitTargetEventStream(ev, gitDest); err != nil {
			log.V(1).Info("Failed to route audit event via ClusterWatchRule", "error", err,
				"gitTarget", gitDest.String())
			recordRouteTarget(
				ctx, rule.GitTargetNamespace, rule.GitTargetRef, ruleKindClusterWatchRule, pipelineOutcomeRouteFailed)
			continue
		}
		recordRouteTarget(
			ctx,
			rule.GitTargetNamespace,
			rule.GitTargetRef,
			ruleKindClusterWatchRule,
			pipelineOutcomeRouted,
		)
		routed++
	}
	return routed
}

// pipelineGVR carries the bounded group/version/resource labels for the
// audit pipeline consumer metric.
type pipelineGVR struct {
	group    string
	version  string
	resource string
}

// Audit pipeline consumer-stage outcome and rule-kind label values.
const (
	pipelineOutcomeUnmatched     = "unmatched"
	pipelineOutcomeDroppedNoBody = "dropped_no_body"
	pipelineOutcomeRouted        = "routed"
	pipelineOutcomeRouteFailed   = "route_failed"

	ruleKindWatchRule        = "watchrule"
	ruleKindClusterWatchRule = "clusterwatchrule"
)

// recordPipelineEvent emits one audit_pipeline_events_total sample for a
// canonical audit event that reached the consumer.
func recordPipelineEvent(ctx context.Context, gvr pipelineGVR, verb, outcome string) {
	if telemetry.AuditPipelineEventsTotal == nil {
		return
	}
	resource := gvr.resource
	if resource == "" {
		resource = "unknown"
	}
	telemetry.AuditPipelineEventsTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("group", gvr.group),
		attribute.String("version", gvr.version),
		attribute.String("resource", resource),
		attribute.String("verb", verb),
		attribute.String("outcome", outcome),
	))
}

// recordRouteTarget emits one audit_pipeline_route_targets_total sample for a
// per-GitTarget route attempt.
func recordRouteTarget(ctx context.Context, gitTargetNamespace, gitTarget, ruleKind, outcome string) {
	if telemetry.AuditPipelineRouteTargetsTotal == nil {
		return
	}
	telemetry.AuditPipelineRouteTargetsTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("git_target_namespace", gitTargetNamespace),
		attribute.String("git_target", gitTarget),
		attribute.String("rule_kind", ruleKind),
		attribute.String("outcome", outcome),
	))
}

// buildGitEvent constructs a git.Event for a given rule match.
func buildGitEvent(
	sanitized *unstructured.Unstructured,
	id itypes.ResourceIdentifier,
	op configv1alpha1.OperationType,
	userInfo git.UserInfo,
	path, gitTargetRef, gitTargetNamespace string,
) git.Event {
	return git.Event{
		Object:             sanitized.DeepCopy(),
		Identifier:         id,
		Operation:          string(op),
		UserInfo:           userInfo,
		Path:               path,
		GitTargetName:      gitTargetRef,
		GitTargetNamespace: gitTargetNamespace,
	}
}

const (
	// displayNameExtraKey is the audit-event user.extra key carrying the OIDC
	// "name" claim, when the API server is configured to map it.
	displayNameExtraKey = "configbutler.ai/claims/display-name"
	// emailExtraKey is the audit-event user.extra key carrying the OIDC
	// "email" claim, when the API server is configured to map it.
	emailExtraKey = "configbutler.ai/claims/email"
)

// resolveUserInfo extracts the effective user identity from an audit event,
// preferring the impersonated user when present. When the effective user
// carries the OIDC display-name / email extras, those populate the optional
// UserInfo fields; absent values are left empty so commit authoring falls back
// to the username.
func resolveUserInfo(event auditv1.Event) git.UserInfo {
	user := event.User
	if event.ImpersonatedUser != nil && event.ImpersonatedUser.Username != "" {
		user = *event.ImpersonatedUser
	}

	return git.UserInfo{
		Username:    user.Username,
		DisplayName: firstExtraValue(user.Extra, displayNameExtraKey),
		Email:       firstExtraValue(user.Extra, emailExtraKey),
	}
}

// firstExtraValue returns the first value for key in an audit event's
// user.extra map, or "" when the key is absent or carries no values.
func firstExtraValue(extra map[string]authnv1.ExtraValue, key string) string {
	values := extra[key]
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// ackMessage ACKs a single message, logging any error.
func (c *AuditConsumer) ackMessage(ctx context.Context, msgID string) {
	if err := c.client.XAck(ctx, c.stream, c.group, msgID).Err(); err != nil {
		c.log.Error(err, "Failed to ACK stream message", "msgID", msgID)
	}
}

// parseAuditEvent unmarshals the audit event from the stream entry's payload_json field.
func parseAuditEvent(values map[string]interface{}) (auditv1.Event, error) {
	raw, ok := values["payload_json"]
	if !ok {
		return auditv1.Event{}, errors.New("stream entry missing payload_json field")
	}

	var payload string
	switch v := raw.(type) {
	case string:
		payload = v
	case []byte:
		payload = string(v)
	default:
		return auditv1.Event{}, fmt.Errorf("unexpected payload_json type %T", raw)
	}

	var event auditv1.Event
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return auditv1.Event{}, fmt.Errorf("failed to unmarshal audit event: %w", err)
	}
	return event, nil
}

// extractObject obtains the Kubernetes object from the audit event and sanitizes it.
// For DELETE operations the RequestObject is used; otherwise the ResponseObject.
func extractObject(
	event auditv1.Event,
	op configv1alpha1.OperationType,
	apiVersion, resource, namespace, name string,
) (*unstructured.Unstructured, error) {
	raw := selectAuditObjectRaw(event, op)

	if len(raw) == 0 {
		if !allowsBodylessSingleDelete(event, resource, name) {
			return nil, errAuditEventObjectMissing
		}
		u := &unstructured.Unstructured{}
		u.SetAPIVersion(apiVersion)
		u.SetKind(resource)
		u.SetNamespace(namespace)
		u.SetName(name)
		return u, nil
	}

	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(raw); err != nil {
		return nil, fmt.Errorf("failed to unmarshal object JSON: %w", err)
	}

	if isStatusObject(obj) {
		return nil, errAuditEventObjectIsStatus
	}

	return backfillSanitizedIdentity(sanitize.Sanitize(obj), apiVersion, resource, namespace, name), nil
}

// isStatusObject reports whether obj is a core metav1.Status error response
// (apiVersion: v1, kind: Status) rather than a real Kubernetes resource. The
// API server returns such a body when a request fails — for example the
// "the object has been modified" message of a 409 Conflict — and it must never
// be written to Git as the resource's desired state.
func isStatusObject(obj *unstructured.Unstructured) bool {
	return obj != nil && obj.GetAPIVersion() == "v1" && obj.GetKind() == "Status"
}

func allowsBodylessSingleDelete(event auditv1.Event, resource, name string) bool {
	return strings.EqualFold(event.Verb, "delete") &&
		resource != "" &&
		name != ""
}

func selectAuditObjectRaw(event auditv1.Event, op configv1alpha1.OperationType) []byte {
	if op == configv1alpha1.OperationDelete {
		return firstAuditObjectRaw(event.RequestObject, event.ResponseObject)
	}

	return firstAuditObjectRaw(event.ResponseObject, event.RequestObject)
}

func firstAuditObjectRaw(objects ...*runtime.Unknown) []byte {
	for _, object := range objects {
		if object != nil && len(object.Raw) > 0 {
			return object.Raw
		}
	}

	return nil
}

func hasAuditObjectRaw(object *runtime.Unknown) bool {
	return object != nil && len(object.Raw) > 0
}

func backfillSanitizedIdentity(
	sanitized *unstructured.Unstructured,
	apiVersion, resource, namespace, name string,
) *unstructured.Unstructured {
	if sanitized.GetAPIVersion() == "" {
		sanitized.SetAPIVersion(apiVersion)
	}
	if sanitized.GetKind() == "" {
		sanitized.SetKind(resource)
	}
	if sanitized.GetNamespace() == "" {
		sanitized.SetNamespace(namespace)
	}
	if sanitized.GetName() == "" {
		sanitized.SetName(name)
	}

	return sanitized
}

func effectiveAuditOperation(event auditv1.Event, op configv1alpha1.OperationType) configv1alpha1.OperationType {
	if op == configv1alpha1.OperationDelete {
		return op
	}
	if auditEventObjectMarkedForDeletion(event) {
		return configv1alpha1.OperationDelete
	}
	return op
}

func auditEventObjectMarkedForDeletion(event auditv1.Event) bool {
	return auditObjectMarkedForDeletion(event.ResponseObject) || auditObjectMarkedForDeletion(event.RequestObject)
}

func auditObjectMarkedForDeletion(rawObj *runtime.Unknown) bool {
	if rawObj == nil || len(rawObj.Raw) == 0 {
		return false
	}

	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(rawObj.Raw); err != nil {
		return false
	}

	return !obj.GetDeletionTimestamp().IsZero()
}

// stringField safely reads a string value from a stream entry's Values map.
func stringField(values map[string]interface{}, key string) string {
	v, ok := values[key]
	if !ok {
		return ""
	}
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// isAlreadyExistsErr returns true when the Redis error indicates that the consumer
// group already exists (BUSYGROUP).
func isAlreadyExistsErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "BUSYGROUP")
}

func isNoGroupErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "NOGROUP")
}
