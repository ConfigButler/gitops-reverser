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
	"time"

	"github.com/go-logr/logr"
	"github.com/redis/go-redis/v9"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
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

// AuditEventRouter is the subset of watch.EventRouter used by the consumer.
// watch.EventRouter satisfies this interface without modification.
type AuditEventRouter interface {
	RouteToGitTargetEventStream(event git.Event, gitDest itypes.ResourceReference) error
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
}

// NewAuditConsumer creates a new AuditConsumer. It does not start consuming;
// call Start to begin the consume loop.
func NewAuditConsumer(
	cfg AuditConsumerConfig,
	ruleStore *rulestore.RuleStore,
	eventRouter AuditEventRouter,
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
	c.log.Info("Consumer group ready", "stream", c.stream, "group", c.group)
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
	op, ok := verbToOperation(auditEvent.Verb)
	if !ok {
		c.ackMessage(ctx, msg.ID)
		return
	}

	if auditEvent.ObjectRef == nil {
		c.ackMessage(ctx, msg.ID)
		return
	}

	clusterID := stringField(msg.Values, "cluster_id")
	if err := c.routeAuditEvent(ctx, log, auditEvent, op, clusterID); err != nil {
		log.Error(err, "Failed to route audit event; ACKing to avoid poison-pill")
	}
	c.ackMessage(ctx, msg.ID)
}

// routeAuditEvent performs rule matching, object extraction, and routing for one audit event.
func (c *AuditConsumer) routeAuditEvent(
	_ context.Context,
	log logr.Logger,
	auditEvent auditv1.Event,
	op configv1alpha1.OperationType,
	clusterID string,
) error {
	ref := auditEvent.ObjectRef
	apiGroup, apiVersion := objectRefGroupVersion(ref)
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

	if len(wrRules) == 0 && len(cwrRules) == 0 {
		return nil
	}

	fullAPIVersion := apiVersion
	if apiGroup != "" {
		fullAPIVersion = apiGroup + "/" + apiVersion
	}

	sanitized, err := extractObject(auditEvent, op, fullAPIVersion, ref.Resource, namespace, name)
	if err != nil {
		return fmt.Errorf("extracting object for %s/%s: %w", namespace, name, err)
	}

	id := itypes.NewResourceIdentifier(apiGroup, apiVersion, resourcePlural, namespace, name)
	userInfo := resolveUserInfo(auditEvent)

	routed := c.routeToMatchedRules(log, sanitized, id, op, userInfo, clusterID, wrRules, cwrRules)

	log.V(1).Info("Processed audit stream entry",
		"resource", resourcePlural, "namespace", namespace, "name", name,
		"operation", op, "user", userInfo.Username, "clusterID", clusterID,
		"routed", routed)
	return nil
}

// routeToMatchedRules dispatches git.Events to all matched WatchRule and ClusterWatchRule targets.
// It returns the number of successfully routed events.
func (c *AuditConsumer) routeToMatchedRules(
	log logr.Logger,
	sanitized *unstructured.Unstructured,
	id itypes.ResourceIdentifier,
	op configv1alpha1.OperationType,
	userInfo git.UserInfo,
	clusterID string,
	wrRules []rulestore.CompiledRule,
	cwrRules []rulestore.CompiledClusterRule,
) int {
	routed := 0
	for _, rule := range wrRules {
		ev := buildGitEvent(sanitized, id, op, userInfo, rule.Path, rule.GitTargetRef, rule.GitTargetNamespace)
		gitDest := itypes.NewResourceReference(rule.GitTargetRef, rule.GitTargetNamespace)
		if err := c.eventRouter.RouteToGitTargetEventStream(ev, gitDest); err != nil {
			log.V(1).Info("Failed to route audit event via WatchRule", "error", err,
				"gitTarget", gitDest.String(), "clusterID", clusterID)
			continue
		}
		routed++
	}
	for _, rule := range cwrRules {
		ev := buildGitEvent(sanitized, id, op, userInfo, rule.Path, rule.GitTargetRef, rule.GitTargetNamespace)
		gitDest := itypes.NewResourceReference(rule.GitTargetRef, rule.GitTargetNamespace)
		if err := c.eventRouter.RouteToGitTargetEventStream(ev, gitDest); err != nil {
			log.V(1).Info("Failed to route audit event via ClusterWatchRule", "error", err,
				"gitTarget", gitDest.String(), "clusterID", clusterID)
			continue
		}
		routed++
	}
	return routed
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

// resolveUserInfo extracts the effective username from an audit event, preferring
// the impersonated user when present.
func resolveUserInfo(event auditv1.Event) git.UserInfo {
	username := event.User.Username
	if event.ImpersonatedUser != nil && event.ImpersonatedUser.Username != "" {
		username = event.ImpersonatedUser.Username
	}
	return git.UserInfo{Username: username}
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
	var raw []byte

	if op == configv1alpha1.OperationDelete {
		if event.RequestObject != nil && len(event.RequestObject.Raw) > 0 {
			raw = event.RequestObject.Raw
		}
	} else {
		if event.ResponseObject != nil && len(event.ResponseObject.Raw) > 0 {
			raw = event.ResponseObject.Raw
		}
	}

	if len(raw) == 0 {
		// Fall back to a minimal stub so downstream pipeline always has an object.
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

	return sanitize.Sanitize(obj), nil
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

// verbToOperation maps a Kubernetes audit verb to a configv1alpha1.OperationType.
// Returns (op, true) for mutating verbs, ("", false) for read-only or unknown verbs.
func verbToOperation(verb string) (configv1alpha1.OperationType, bool) {
	switch strings.ToLower(verb) {
	case "create":
		return configv1alpha1.OperationCreate, true
	case "update", "patch":
		return configv1alpha1.OperationUpdate, true
	case "delete", "deletecollection":
		return configv1alpha1.OperationDelete, true
	default:
		return "", false
	}
}

// splitAPIVersion splits a Kubernetes apiVersion string (e.g. "apps/v1" or "v1")
// into (group, version). Core resources have an empty group.
func splitAPIVersion(apiVersion string) (string, string) {
	group, version, found := strings.Cut(apiVersion, "/")
	if !found {
		return "", apiVersion
	}
	return group, version
}

func objectRefGroupVersion(ref *auditv1.ObjectReference) (string, string) {
	if ref == nil {
		return "", ""
	}

	group, version := splitAPIVersion(ref.APIVersion)
	if group != "" {
		return group, version
	}

	return ref.APIGroup, version
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
