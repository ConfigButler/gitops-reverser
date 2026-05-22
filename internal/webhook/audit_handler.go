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
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/apis/audit"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	"sigs.k8s.io/yaml"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/auditutil"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// auditHandlerFirsts holds one-shot startup-milestone log gates. Per-event
// Info logs are noisy in steady state; these surface the first time the
// pipeline crosses each transition so operators can see startup progress at
// a glance.
type auditHandlerFirsts struct {
	officialRequest   sync.Once
	additionalRequest sync.Once
	officialEmit      sync.Once
	additionalEmit    sync.Once
	impersonatedEvent sync.Once
}

const (
	// DefaultAuditDumpDir is the default directory for audit event dumps.
	DefaultAuditDumpDir = "/tmp/audit-events"
	// DefaultAuditMaxRequestBodyBytes limits incoming audit payload size.
	DefaultAuditMaxRequestBodyBytes = int64(10 * 1024 * 1024)
)

// AuditHandlerConfig contains configuration for the audit handler.
type AuditHandlerConfig struct {
	// DumpDir is the directory where audit events are written for debugging.
	// If empty, defaults to DefaultAuditDumpDir.
	DumpDir string
	// MaxRequestBodyBytes is the maximum accepted HTTP request body size.
	MaxRequestBodyBytes int64
	// Queue enqueues accepted audit events to a durable backend.
	// If nil, queueing is disabled.
	Queue AuditEventQueue
	// Joiner optionally parks additional-source bodies and deduplicates canonical audit events.
	Joiner AuditEventJoiner
	// RelevanceMatcher optionally gates body joins to resources that active rules could watch.
	RelevanceMatcher AuditRelevanceMatcher
}

// AuditEventQueue persists accepted audit events for downstream processing.
type AuditEventQueue interface {
	Enqueue(ctx context.Context, event auditv1.Event) error
}

// AuditRelevanceMatcher checks whether an audit event could match active watch rules.
type AuditRelevanceMatcher interface {
	IsReady() bool
	CouldMatchResource(
		resourcePlural string,
		operation configv1alpha1.OperationType,
		apiGroup string,
		apiVersion string,
	) bool
}

type auditIngressDecision struct {
	Process        bool
	Operation      configv1alpha1.OperationType
	Quality        AuditEventQuality
	GVR            schema.GroupVersionResource
	CouldBeWatched bool
	Reason         string
}

// AuditHandler handles incoming audit events and collects telemetry.
type AuditHandler struct {
	scheme       *runtime.Scheme
	deserializer runtime.Decoder
	config       AuditHandlerConfig
	firsts       auditHandlerFirsts
	canonicalMu  sync.Mutex
}

// NewAuditHandler creates a new audit handler with the given configuration.
// If config.DumpDir is empty, file dumping is disabled.
func NewAuditHandler(config AuditHandlerConfig) (*AuditHandler, error) {
	if config.MaxRequestBodyBytes <= 0 {
		config.MaxRequestBodyBytes = DefaultAuditMaxRequestBodyBytes
	}

	scheme := runtime.NewScheme()
	if err := audit.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to initialize scheme: %w", err)
	}
	if err := auditv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add v1 types to scheme: %w", err)
	}

	codecs := serializer.NewCodecFactory(scheme)
	deserializer := codecs.UniversalDeserializer()

	return &AuditHandler{
		scheme:       scheme,
		deserializer: deserializer,
		config:       config,
	}, nil
}

// ServeHTTP implements http.Handler for audit event processing.
func (h *AuditHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := logf.Log.WithName("audit-handler")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	source, err := auditSourceFromPath(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	reqLog := log.WithValues(
		"source", source,
		"remoteAddr", r.RemoteAddr,
		"path", r.URL.Path,
	)

	eventListV1, err := h.decodeEventList(r)
	if err != nil {
		reqLog.Error(err, "Failed to decode audit event list")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if len(eventListV1.Items) == 0 {
		reqLog.Info("Received empty audit event list", "eventCount", 0, "processingOutcome", "empty")
		w.WriteHeader(http.StatusOK)
		_, err = w.Write([]byte("Empty event list processed"))
		if err != nil {
			reqLog.Error(err, "Failed to write response")
		}
		return
	}

	h.logFirstAuditRequest(reqLog, source, len(eventListV1.Items))

	if err := h.processEvents(ctx, source, eventListV1.Items); err != nil {
		reqLog.Error(err, "Failed to process audit events")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	reqLog.V(1).Info("Processed audit request", "eventCount", len(eventListV1.Items), "processingOutcome", "success")

	w.WriteHeader(http.StatusOK)
	_, err = w.Write([]byte("Audit event processed"))
	if err != nil {
		reqLog.Error(err, "Failed to write response")
	}
}

// decodeEventList reads and decodes the audit event list from the request.
func (h *AuditHandler) decodeEventList(r *http.Request) (*auditv1.EventList, error) {
	limited := io.LimitReader(r.Body, h.config.MaxRequestBodyBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("failed to read body: %w", err)
	}
	defer r.Body.Close()
	if int64(len(body)) > h.config.MaxRequestBodyBytes {
		return nil, fmt.Errorf("request body too large: max %d bytes", h.config.MaxRequestBodyBytes)
	}

	var eventListV1 auditv1.EventList
	_, _, err = h.deserializer.Decode(body, nil, &eventListV1)
	if err != nil {
		return nil, fmt.Errorf("invalid audit event list: %w", err)
	}

	return &eventListV1, nil
}

// processEvents processes a list of audit events.
func (h *AuditHandler) processEvents(ctx context.Context, source AuditSource, events []auditv1.Event) error {
	for _, auditEventV1 := range events {
		if err := h.processEvent(ctx, source, auditEventV1); err != nil {
			return err
		}
	}

	return nil
}

func (h *AuditHandler) processEvent(ctx context.Context, source AuditSource, auditEventV1 auditv1.Event) error {
	log := logf.Log.WithName("audit-handler")

	auditEvent, process, err := h.prepareAuditEvent(ctx, source, auditEventV1)
	if err != nil || !process {
		return err
	}

	// Only ResponseComplete drives the canonical stream. Earlier stages share the same auditID
	// and must not claim the dedupe key; if they did, the later ResponseComplete event for the
	// same auditID would be dropped as a duplicate before reaching Git.
	if auditEventV1.Stage != auditv1.StageResponseComplete {
		return nil
	}

	if source == AuditSourceOfficial {
		gateStart := time.Now()
		h.canonicalMu.Lock()
		defer h.canonicalMu.Unlock()
		observeOfficialGateWait(ctx, time.Since(gateStart).Seconds())
	}

	quality := classifyAuditEventQuality(source, &auditEventV1)
	addQualityMetric(ctx, source, &auditEventV1, quality)
	decision := h.classifyAuditIngress(source, &auditEventV1, quality)
	if !decision.Process {
		logAuditJoinSkip(
			"Dropped audit event before join relevance gate",
			source,
			h.extractGVR(&auditEvent),
			auditEvent.AuditID,
		)
		return nil
	}

	eventToWrite, joinDecision, shouldEmit, err := h.eventForCanonicalStream(
		ctx, source, &auditEventV1, auditEvent, quality,
	)
	if err != nil || !shouldEmit {
		return err
	}
	if err := h.enqueueCanonicalEvent(ctx, eventToWrite, auditEvent, joinDecision); err != nil {
		return err
	}
	if err := h.commitJoinDecision(ctx, joinDecision); err != nil {
		return err
	}

	h.logFirstAuditEmit(log, source, auditEvent)

	log.V(1).Info("Processed audit event",
		"source", source,
		"gvr", h.extractGVR(&auditEvent),
		"action", auditEvent.Verb,
		"auditID", auditEvent.AuditID,
		"user", effectiveAuditUsername(auditEvent),
		"ips", auditEvent.SourceIPs,
		"userAgent", auditEvent.UserAgent)

	return h.writeCanonicalAuditEvent(eventToWrite, auditEvent)
}

// logFirstAuditRequest emits an Info banner the first time we accept an
// audit POST from each source so operators can see ingress is wired up.
func (h *AuditHandler) logFirstAuditRequest(log logr.Logger, source AuditSource, eventCount int) {
	switch source {
	case AuditSourceOfficial:
		h.firsts.officialRequest.Do(func() {
			log.Info("Received first audit request (official)", "eventCount", eventCount)
		})
	case AuditSourceAdditional:
		h.firsts.additionalRequest.Do(func() {
			log.Info("Received first audit request (additional)", "eventCount", eventCount)
		})
	}
}

// logFirstAuditEmit emits an Info banner the first time an event from each
// source is enqueued onto the canonical stream.
func (h *AuditHandler) logFirstAuditEmit(log logr.Logger, source AuditSource, event audit.Event) {
	switch source {
	case AuditSourceOfficial:
		h.firsts.officialEmit.Do(func() {
			log.Info("First audit event enqueued to canonical stream (official)",
				"auditID", event.AuditID,
				"verb", event.Verb)
		})
	case AuditSourceAdditional:
		h.firsts.additionalEmit.Do(func() {
			log.Info("First audit event enqueued to canonical stream (additional)",
				"auditID", event.AuditID,
				"verb", event.Verb)
		})
	}
}

func (h *AuditHandler) prepareAuditEvent(
	ctx context.Context,
	source AuditSource,
	auditEventV1 auditv1.Event,
) (audit.Event, bool, error) {
	var auditEvent audit.Event
	if err := h.scheme.Convert(&auditEventV1, &auditEvent, nil); err != nil {
		return audit.Event{}, false, fmt.Errorf("failed to convert audit event: %w", err)
	}

	process, err := h.checkEvent(&auditEvent)
	if err != nil {
		return audit.Event{}, false, fmt.Errorf("failed to check audit event: %w", err)
	}
	h.recordReceivedMetric(ctx, source, auditEvent)
	return auditEvent, process, nil
}

func (h *AuditHandler) recordReceivedMetric(
	ctx context.Context,
	source AuditSource,
	auditEvent audit.Event,
) {
	user := effectiveAuditUsername(auditEvent)
	if auditEvent.ImpersonatedUser != nil {
		handlerLog := logf.Log.WithName("audit-handler")
		h.firsts.impersonatedEvent.Do(func() {
			handlerLog.Info("First impersonated audit event observed",
				"source", source,
				"authUser", auditEvent.User.Username,
				"impersonatedUser", auditEvent.ImpersonatedUser.Username)
		})
		handlerLog.V(1).Info("Audit event impersonated",
			"source", source,
			"authUser", auditEvent.User.Username,
			"impersonatedUser", auditEvent.ImpersonatedUser,
		)
	}

	telemetry.AuditEventsReceivedTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("source", string(source)),
		attribute.String("gvr", h.extractGVR(&auditEvent)),
		attribute.String("action", auditEvent.Verb),
		attribute.String("user", user),
	))
}

func (h *AuditHandler) eventForCanonicalStream(
	ctx context.Context,
	source AuditSource,
	auditEventV1 *auditv1.Event,
	auditEvent audit.Event,
	quality AuditEventQuality,
) (*auditv1.Event, AuditJoinDecision, bool, error) {
	if h.config.Joiner == nil {
		emit := quality == AuditEventQualityComplete ||
			quality == AuditEventQualityBodyShallowDeletable ||
			quality == AuditEventQualityCollection
		return auditEventV1, AuditJoinDecision{}, emit, nil
	}

	decision, err := h.config.Joiner.Decide(ctx, source, auditEventV1, quality)
	if err != nil {
		return nil, AuditJoinDecision{}, false, fmt.Errorf(
			"failed to decide audit event %q: %w",
			auditEvent.AuditID,
			err,
		)
	}
	switch decision.Action {
	case AuditJoinActionParked:
		logAuditJoinSkip("Parked additional audit body", source, h.extractGVR(&auditEvent), auditEvent.AuditID)
		return nil, decision, false, nil
	case AuditJoinActionDrop:
		logAuditJoinSkip(
			"Dropped audit event before canonical stream enqueue",
			source,
			h.extractGVR(&auditEvent),
			auditEvent.AuditID,
		)
		return nil, decision, false, nil
	case AuditJoinActionEmit:
		if decision.Event == nil {
			return nil, decision, false, fmt.Errorf("joiner emitted nil audit event for %q", auditEvent.AuditID)
		}
		return decision.Event, decision, true, nil
	default:
		return nil, decision, false, fmt.Errorf(
			"unknown audit join action %d for %q",
			decision.Action,
			auditEvent.AuditID,
		)
	}
}

func (h *AuditHandler) enqueueCanonicalEvent(
	ctx context.Context,
	event *auditv1.Event,
	auditEvent audit.Event,
	decision AuditJoinDecision,
) error {
	if h.config.Queue == nil {
		return nil
	}
	if err := h.config.Queue.Enqueue(ctx, *event); err != nil {
		h.releaseJoinDecision(ctx, decision)
		return fmt.Errorf("failed to enqueue audit event %q: %w", auditEvent.AuditID, err)
	}
	return nil
}

func (h *AuditHandler) commitJoinDecision(ctx context.Context, decision AuditJoinDecision) error {
	if h.config.Joiner == nil || decision.Action != AuditJoinActionEmit {
		return nil
	}
	if err := h.config.Joiner.CommitDecision(ctx, decision.AuditID, decision.Result); err != nil {
		return fmt.Errorf("failed to commit audit event decision %q: %w", decision.AuditID, err)
	}
	addEmittedMetric(ctx, decision.Source, decision.Result)
	return nil
}

func (h *AuditHandler) releaseJoinDecision(ctx context.Context, decision AuditJoinDecision) {
	if h.config.Joiner == nil || decision.Action != AuditJoinActionEmit {
		return
	}
	if err := h.config.Joiner.ReleaseDecision(ctx, decision.AuditID); err != nil {
		logf.Log.WithName("audit-handler").Error(err,
			"Failed to release audit join decision after enqueue failure",
			"auditID", decision.AuditID)
	}
}

func (h *AuditHandler) writeCanonicalAuditEvent(eventToWrite *auditv1.Event, fallback audit.Event) error {
	var emitted audit.Event
	if err := h.scheme.Convert(eventToWrite, &emitted, nil); err != nil {
		return fmt.Errorf("failed to convert emitted audit event: %w", err)
	}
	if emitted.AuditID == "" {
		emitted = fallback
	}
	h.writeAuditEventToFile(&emitted)
	return nil
}

func effectiveAuditUsername(event audit.Event) string {
	if event.ImpersonatedUser != nil && event.ImpersonatedUser.Username != "" {
		return event.ImpersonatedUser.Username
	}
	return event.User.Username
}

func logAuditJoinSkip(message string, source AuditSource, gvr string, auditID types.UID) {
	logf.Log.WithName("audit-handler").V(1).Info(message,
		"source", source,
		"gvr", gvr,
		"auditID", auditID)
}

func auditSourceFromPath(path string) (AuditSource, error) {
	switch path {
	case "/audit-webhook":
		return AuditSourceOfficial, nil
	case "/audit-webhook-additional":
		return AuditSourceAdditional, nil
	case "/audit-webhook/", "/audit-webhook-additional/":
		return "", errors.New("audit webhook path must not include a trailing slash")
	default:
		if strings.HasPrefix(path, "/audit-webhook/") || strings.HasPrefix(path, "/audit-webhook-additional/") {
			return "", errors.New("audit webhook path must not include a cluster ID or extra path segment")
		}
		return "", errors.New("invalid path; expected /audit-webhook or /audit-webhook-additional")
	}
}

// extractGVR constructs the Group/Version/Resource string from the audit event
// using k8s.io/apimachinery/pkg/runtime/schema utilities.
func (h *AuditHandler) extractGVR(event *audit.Event) string {
	if event.ObjectRef == nil || event.ObjectRef.APIVersion == "" {
		return "unknown/unknown/unknown"
	}

	gv, err := schema.ParseGroupVersion(event.ObjectRef.APIVersion)
	if err != nil {
		return fmt.Sprintf("invalid/%s/%s", event.ObjectRef.APIVersion, event.ObjectRef.Resource)
	}

	resource := event.ObjectRef.Resource
	if resource == "" {
		resource = "unknown"
	}

	gvr := gv.WithResource(resource)

	if gvr.Group == "" {
		return fmt.Sprintf("/%s/%s", gvr.Version, gvr.Resource)
	}
	return fmt.Sprintf("%s/%s/%s", gvr.Group, gvr.Version, gvr.Resource)
}

func (h *AuditHandler) classifyAuditIngress(
	source AuditSource,
	event *auditv1.Event,
	quality AuditEventQuality,
) auditIngressDecision {
	decision := auditIngressDecision{Process: true, Quality: quality}
	if event == nil {
		decision.Process = false
		decision.Reason = "nil_event"
		return decision
	}
	if event.Stage != auditv1.StageResponseComplete {
		decision.Process = false
		decision.Reason = "stage"
		return decision
	}

	op, ok := auditutil.VerbToOperation(event.Verb)
	if !ok {
		decision.Process = false
		decision.Reason = "read_only_or_unknown_verb"
		return decision
	}
	decision.Operation = op

	gvr, hasGVR := auditutil.ObjectRefGVR(event.ObjectRef)
	if hasGVR {
		decision.GVR = gvr
	}
	couldBeWatched := h.couldBeWatched(gvr, op, hasGVR)
	decision.CouldBeWatched = couldBeWatched

	switch {
	case source == AuditSourceAdditional && quality == AuditEventQualityMalformed:
		decision.Process = false
		decision.Reason = "malformed_additional"
	case source == AuditSourceAdditional && !couldBeWatched:
		decision.Process = false
		decision.Reason = "unwatched_additional"
	case source == AuditSourceOfficial && quality == AuditEventQualityIdentityShallow && !couldBeWatched:
		decision.Process = false
		decision.Reason = "unwatched_shallow_official"
	}
	return decision
}

func (h *AuditHandler) couldBeWatched(
	gvr schema.GroupVersionResource,
	operation configv1alpha1.OperationType,
	hasGVR bool,
) bool {
	matcher := h.config.RelevanceMatcher
	if matcher == nil || !matcher.IsReady() || !hasGVR {
		return true
	}
	return matcher.CouldMatchResource(gvr.Resource, operation, gvr.Group, gvr.Version)
}

// checkEvent validates an audit event before processing.
func (h *AuditHandler) checkEvent(event *audit.Event) (bool, error) {
	process := event.ObjectRef == nil || event.ObjectRef.Subresource != "status"
	if string(event.AuditID) == "" {
		return process, errors.New("invalid audit event: auditID cannot be empty")
	}

	return process, nil
}

func hasAuditV1ObjectBody(event *auditv1.Event) bool {
	return event != nil && (hasRuntimeUnknownBody(event.RequestObject) || hasRuntimeUnknownBody(event.ResponseObject))
}

func hasRuntimeUnknownBody(object *runtime.Unknown) bool {
	return object != nil && len(object.Raw) > 0
}

// writeAuditEventToFile writes an audit event to a YAML file for debugging and testing.
// Assumes event has been validated by validateEvent() - auditID is guaranteed to be non-empty.
// Skips file writing if DumpDir is empty (disabled).
func (h *AuditHandler) writeAuditEventToFile(event *audit.Event) {
	if h.config.DumpDir == "" {
		return
	}

	if err := os.MkdirAll(h.config.DumpDir, 0750); err != nil {
		logf.Log.Error(err, "Failed to create audit events dump directory", "directory", h.config.DumpDir)
		return
	}

	filename := fmt.Sprintf("%s.yaml", event.AuditID)
	filePath := filepath.Join(h.config.DumpDir, filename)

	var eventV1 auditv1.Event
	if err := h.scheme.Convert(event, &eventV1, nil); err != nil {
		logf.Log.Error(err, "Failed to convert event to v1", "auditID", event.AuditID)
		return
	}

	eventV1.Kind = "Event"
	eventV1.APIVersion = "audit.k8s.io/v1"

	eventYAML, err := yaml.Marshal(&eventV1)
	if err != nil {
		logf.Log.Error(err, "Failed to marshal audit event to YAML", "auditID", event.AuditID)
		return
	}

	if err := os.WriteFile(filePath, eventYAML, 0600); err != nil {
		logf.Log.Error(err, "Failed to write audit event to file", "file", filePath, "auditID", event.AuditID)
		return
	}

	logf.Log.V(1).Info("Audit event written to file", "file", filePath, "auditID", event.AuditID)
}
