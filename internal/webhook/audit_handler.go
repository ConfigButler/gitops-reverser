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
	logf "sigs.k8s.io/controller-runtime/pkg/log"

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

// DefaultAuditMaxRequestBodyBytes limits incoming audit payload size.
const DefaultAuditMaxRequestBodyBytes = int64(10 * 1024 * 1024)

// AuditHandlerConfig contains configuration for the audit handler.
type AuditHandlerConfig struct {
	// MaxRequestBodyBytes is the maximum accepted HTTP request body size.
	MaxRequestBodyBytes int64
	// Queue enqueues accepted audit events to a durable backend.
	// If nil, queueing is disabled.
	Queue AuditEventQueue
	// DebugQueue enqueues every decoded event before audit processing begins.
	// If nil, early debug stream queueing is disabled.
	DebugQueue AuditDebugEventQueue
	// Joiner optionally parks additional-source bodies and deduplicates canonical audit events.
	Joiner AuditEventJoiner
}

// AuditEventQueue persists accepted audit events for downstream processing.
type AuditEventQueue interface {
	Enqueue(ctx context.Context, event auditv1.Event) error
}

// AuditDebugEventQueue persists decoded events for early audit debugging.
type AuditDebugEventQueue interface {
	Enqueue(ctx context.Context, source string, event auditv1.Event) error
}

// auditIngressDecision is the intrinsic accept/reject verdict for an audit event
// before it enters the join pipeline. The verdict is derived purely from the
// event — stage, verb, and body shape — and carries no knowledge of WatchRules;
// rule relevance is a consumer-side concern applied later. Reason labels a
// rejection for drop diagnostics.
type auditIngressDecision struct {
	Process bool
	Reason  string
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

// EventList request-boundary outcome labels. They stay bounded — no path,
// remote address, or status-code dimension — so the ingress metric set is small.
const (
	outcomeProcessed    = "processed"
	outcomeEmpty        = "empty"
	outcomeDecodeError  = "decode_error"
	outcomeProcessError = "process_error"
)

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

	start := time.Now()
	outcome, eventCount := h.serveEventListRequest(ctx, w, r, source, log)
	h.recordEventListRequest(ctx, source, outcome, eventCount, time.Since(start))
}

// serveEventListRequest decodes and processes one EventList request, returning the
// bounded outcome and the number of decoded event items for the ingress metrics.
func (h *AuditHandler) serveEventListRequest(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	source AuditSource,
	log logr.Logger,
) (string, int) {
	reqLog := log.WithValues(
		"source", source,
		"remoteAddr", r.RemoteAddr,
		"path", r.URL.Path,
	)

	eventListV1, err := h.decodeEventList(r)
	if err != nil {
		reqLog.Error(err, "Failed to decode audit event list")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return outcomeDecodeError, 0
	}

	eventCount := len(eventListV1.Items)
	if err := h.enqueueDebugEvents(ctx, source, eventListV1.Items); err != nil {
		reqLog.Error(err, "Failed to enqueue early audit debug events")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return outcomeProcessError, eventCount
	}
	if eventCount == 0 {
		reqLog.Info("Received empty audit event list", "eventCount", 0, "processingOutcome", "empty")
		h.writeResponse(w, reqLog, "Empty event list processed")
		return outcomeEmpty, 0
	}

	h.logFirstAuditRequest(reqLog, source, eventCount)

	if err := h.processEvents(ctx, source, eventListV1.Items); err != nil {
		reqLog.Error(err, "Failed to process audit events")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return outcomeProcessError, eventCount
	}
	reqLog.V(1).Info("Processed audit request", "eventCount", eventCount, "processingOutcome", "success")

	h.writeResponse(w, reqLog, "Audit event processed")
	return outcomeProcessed, eventCount
}

// writeResponse writes a 200 OK body, logging any write failure.
func (h *AuditHandler) writeResponse(w http.ResponseWriter, log logr.Logger, body string) {
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(body)); err != nil {
		log.Error(err, "Failed to write response")
	}
}

// recordEventListRequest emits the three EventList ingress-boundary metrics.
// The event-item counter has no sample for decode_error, since item count is
// only known after a successful decode.
func (h *AuditHandler) recordEventListRequest(
	ctx context.Context,
	source AuditSource,
	outcome string,
	eventCount int,
	elapsed time.Duration,
) {
	attrs := metric.WithAttributes(
		attribute.String("source", string(source)),
		attribute.String("outcome", outcome),
	)
	if telemetry.AuditEventListsTotal != nil {
		telemetry.AuditEventListsTotal.Add(ctx, 1, attrs)
	}
	if telemetry.AuditEventListDurationSeconds != nil {
		telemetry.AuditEventListDurationSeconds.Record(ctx, elapsed.Seconds(), attrs)
	}
	if outcome != outcomeDecodeError && telemetry.AuditEventListEventsTotal != nil {
		telemetry.AuditEventListEventsTotal.Add(ctx, int64(eventCount), attrs)
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

// enqueueDebugEvents preserves each decoded event before normal audit processing can filter it.
func (h *AuditHandler) enqueueDebugEvents(ctx context.Context, source AuditSource, events []auditv1.Event) error {
	if h.config.DebugQueue == nil {
		return nil
	}
	for _, event := range events {
		if err := h.config.DebugQueue.Enqueue(ctx, string(source), event); err != nil {
			return fmt.Errorf("failed to enqueue early audit debug event %q: %w", event.AuditID, err)
		}
	}
	return nil
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
	decision := classifyAuditIngress(source, &auditEventV1, quality)
	if !decision.Process {
		logAuditJoinSkip(
			"Dropped audit event before join pipeline",
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

	return nil
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
	handlerLog := logf.Log.WithName("audit-handler")
	if auditEvent.ImpersonatedUser != nil {
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

	// The username is intentionally not a metric label (cardinality bomb); it
	// stays in the structured logs only.
	group, version, resource := gvrParts(&auditEvent)
	subresource := subresourcePart(&auditEvent)
	handlerLog.V(1).Info("Audit event received",
		"source", source,
		"group", group,
		"version", version,
		"resource", resource,
		"subresource", subresource,
		"verb", auditEvent.Verb,
		"user", effectiveAuditUsername(auditEvent))
	telemetry.AuditEventsReceivedTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("source", string(source)),
		attribute.String("group", group),
		attribute.String("version", version),
		attribute.String("resource", resource),
		attribute.String("subresource", subresource),
		attribute.String("verb", auditEvent.Verb),
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

// gvrParts splits an audit event's objectRef into bounded group/version/resource
// metric label values. An empty group denotes the core API group; an absent or
// unparseable ref collapses to "unknown"/"invalid" so the label set stays small.
func gvrParts(event *audit.Event) (string, string, string) {
	if event.ObjectRef == nil || event.ObjectRef.APIVersion == "" {
		return "unknown", "unknown", "unknown"
	}
	gv, err := schema.ParseGroupVersion(event.ObjectRef.APIVersion)
	if err != nil {
		return "invalid", event.ObjectRef.APIVersion, orUnknownResource(event.ObjectRef.Resource)
	}
	return gv.Group, gv.Version, orUnknownResource(event.ObjectRef.Resource)
}

// subresourcePart returns the audit event's objectRef.subresource, or "" when
// the event has no objectRef or targets a top-level resource. Kubernetes
// subresources are a bounded, closed set (status, scale, exec, log, ...), so
// this is safe to use as a metric label.
func subresourcePart(event *audit.Event) string {
	if event.ObjectRef == nil {
		return ""
	}
	return event.ObjectRef.Subresource
}

func orUnknownResource(resource string) string {
	if resource == "" {
		return "unknown"
	}
	return resource
}

// extractGVR constructs the Group/Version/Resource string from the audit event
// for use as a log field.
func (h *AuditHandler) extractGVR(event *audit.Event) string {
	group, version, resource := gvrParts(event)
	if group == "" {
		return fmt.Sprintf("/%s/%s", version, resource)
	}
	return fmt.Sprintf("%s/%s/%s", group, version, resource)
}

// classifyAuditIngress is the intrinsic gate in front of the join pipeline. It
// decides accept/reject from the event alone — stage, verb, response status, and
// body shape — and never consults WatchRules: rule relevance is a consumer-side
// concern applied later, when an event is turned into a Git write.
//
//   - A non-ResponseComplete stage, or a read-only/unknown verb (get/list/watch),
//     is never a Git-relevant mutation, so it is rejected for both sources.
//   - A request the API server rejected (responseStatus.code >= 300, e.g. a 409
//     Conflict from an optimistic-concurrency failure) changed nothing in etcd.
//     Its responseObject is a metav1.Status error body, not the resource, so it
//     must never reach Git; it is rejected for both sources.
//   - An additional-source event is only worth parking when it actually carries a
//     request/response body; a shallow (malformed) one has nothing to contribute.
//
// Everything else — including every shallow official event — is accepted, so the
// joiner can wait for and merge a proxy-supplied body for it.
func classifyAuditIngress(
	source AuditSource,
	event *auditv1.Event,
	quality AuditEventQuality,
) auditIngressDecision {
	switch {
	case event == nil:
		return auditIngressDecision{Reason: "nil_event"}
	case event.Stage != auditv1.StageResponseComplete:
		return auditIngressDecision{Reason: "stage"}
	}
	if _, ok := auditutil.VerbToOperation(event.Verb); !ok {
		return auditIngressDecision{Reason: "read_only_or_unknown_verb"}
	}
	if isFailedAuditRequest(event) {
		return auditIngressDecision{Reason: "failed_request"}
	}
	if source == AuditSourceAdditional && quality == AuditEventQualityMalformed {
		return auditIngressDecision{Reason: "malformed_additional"}
	}
	return auditIngressDecision{Process: true}
}

// isFailedAuditRequest reports whether the API server rejected the request the
// audit event describes. A non-success responseStatus.code (>= 300) means the
// mutation never reached etcd, and the event's responseObject is a metav1.Status
// error body rather than the resource. The status code is left at zero by some
// additional-source proxies; that is treated as success here, because the
// matching official event — which always carries responseStatus — is the one
// that drives the canonical stream.
func isFailedAuditRequest(event *auditv1.Event) bool {
	return event != nil && event.ResponseStatus != nil && event.ResponseStatus.Code >= 300
}

// checkEvent validates an audit event before processing.
func (h *AuditHandler) checkEvent(event *audit.Event) (bool, error) {
	process := shouldForwardSubresource(event)
	if string(event.AuditID) == "" {
		return process, errors.New("invalid audit event: auditID cannot be empty")
	}

	return process, nil
}

// shouldForwardSubresource is the cheap subresource forwarding gate. Top-level
// resource events always pass. A subresource event passes only when it is a mutating
// verb and not hard-denied, so a supported subresource (e.g. deployments/scale)
// reaches the consumer to be translated into a parent-manifest field patch, while
// status, exec, proxy, log, and other non-desired-state subresources are dropped
// before Redis. The consumer remains the authority for whether a forwarded
// subresource can actually be resolved. See
// docs/design/manifest/version2/scale-subresource-audit-rehydration.md.
func shouldForwardSubresource(event *audit.Event) bool {
	if event.ObjectRef == nil || event.ObjectRef.Subresource == "" {
		return true
	}
	if _, ok := auditutil.VerbToOperation(event.Verb); !ok {
		return false
	}
	return !auditutil.IsHardDeniedSubresource(event.ObjectRef.Resource, event.ObjectRef.Subresource)
}

func hasAuditV1ObjectBody(event *auditv1.Event) bool {
	return event != nil && (hasRuntimeUnknownBody(event.RequestObject) || hasRuntimeUnknownBody(event.ResponseObject))
}

func hasRuntimeUnknownBody(object *runtime.Unknown) bool {
	return object != nil && len(object.Raw) > 0
}
