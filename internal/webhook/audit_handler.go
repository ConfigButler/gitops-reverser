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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/apis/audit"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ConfigButler/gitops-reverser/internal/audit/outcome"
	"github.com/ConfigButler/gitops-reverser/internal/auditutil"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// DefaultAuditMaxRequestBodyBytes limits incoming audit payload size.
const DefaultAuditMaxRequestBodyBytes = int64(10 * 1024 * 1024)

// auditHandlerFirsts holds one-shot startup-milestone log gates. Per-event Info
// logs are noisy in steady state; these surface the first time the pipeline crosses
// each transition so operators can see ingress is wired up at a glance.
type auditHandlerFirsts struct {
	request           sync.Once
	factRecorded      sync.Once
	impersonatedEvent sync.Once
}

// AuditFactRecorder stores the minimal author-attribution fact for one accepted,
// mutating audit event. It is the only thing the audit webhook does now: watch
// carries the object body, so audit is a pure attribution lookup table. A nil
// recorder means committer-only mode — the handler is not wired at all.
type AuditFactRecorder interface {
	RecordFact(ctx context.Context, event auditv1.Event) error
}

// AuditHandlerConfig contains configuration for the audit handler.
type AuditHandlerConfig struct {
	// MaxRequestBodyBytes is the maximum accepted HTTP request body size.
	MaxRequestBodyBytes int64
	// FactRecorder persists the attribution fact for each accepted, mutating event.
	// A write failure returns an audit-request error so the API server retries
	// delivery — the CommitRequest controller waits on these facts.
	FactRecorder AuditFactRecorder
}

// AuditHandler receives kube-apiserver audit events on /audit-webhook and records
// the author-attribution fact for each Git-relevant mutation. It never writes object
// state — that comes from WATCH — so it decodes, applies the intrinsic accept gate
// (stage, verb, success, dry-run, unchanged-RV, subresource), and records the fact.
type AuditHandler struct {
	deserializer runtime.Decoder
	config       AuditHandlerConfig
	firsts       auditHandlerFirsts
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
	return &AuditHandler{
		deserializer: codecs.UniversalDeserializer(),
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

	if err := validateAuditWebhookPath(r.URL.Path); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	start := time.Now()
	result, eventCount := h.serveEventListRequest(ctx, w, r, log)
	h.recordEventListRequest(ctx, result, eventCount, time.Since(start))
}

// serveEventListRequest decodes and processes one EventList request, returning the
// bounded outcome and the number of decoded event items for the ingress metrics.
func (h *AuditHandler) serveEventListRequest(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	log logr.Logger,
) (string, int) {
	reqLog := log.WithValues("remoteAddr", r.RemoteAddr, "path", r.URL.Path)

	eventListV1, err := h.decodeEventList(r)
	if err != nil {
		reqLog.Error(err, "Failed to decode audit event list")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return outcomeDecodeError, 0
	}

	eventCount := len(eventListV1.Items)
	if eventCount == 0 {
		reqLog.Info("Received empty audit event list", "eventCount", 0, "processingOutcome", "empty")
		h.writeResponse(w, reqLog, "Empty event list processed")
		return outcomeEmpty, 0
	}

	h.firsts.request.Do(func() {
		reqLog.Info("Received first audit request", "eventCount", eventCount)
	})

	if err := h.processEvents(ctx, eventListV1.Items); err != nil {
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
	result string,
	eventCount int,
	elapsed time.Duration,
) {
	attrs := metric.WithAttributes(attribute.String("outcome", result))
	if telemetry.AuditEventListsTotal != nil {
		telemetry.AuditEventListsTotal.Add(ctx, 1, attrs)
	}
	if telemetry.AuditEventListDurationSeconds != nil {
		telemetry.AuditEventListDurationSeconds.Record(ctx, elapsed.Seconds(), attrs)
	}
	if result != outcomeDecodeError && telemetry.AuditEventListEventsTotal != nil {
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
	if _, _, err = h.deserializer.Decode(body, nil, &eventListV1); err != nil {
		return nil, fmt.Errorf("invalid audit event list: %w", err)
	}

	return &eventListV1, nil
}

// processEvents processes a list of audit events.
func (h *AuditHandler) processEvents(ctx context.Context, events []auditv1.Event) error {
	for i := range events {
		if err := h.processEvent(ctx, events[i]); err != nil {
			return err
		}
	}
	return nil
}

// processEvent applies the intrinsic accept gate and records the attribution fact
// for an accepted, mutating event. A rejected event is recorded with its terminal
// outcome and dropped; only a fact-store failure returns an error (the API server
// then retries delivery).
func (h *AuditHandler) processEvent(ctx context.Context, event auditv1.Event) error {
	log := logf.Log.WithName("audit-handler")
	h.logAuditEventReceived(event)

	if !shouldForwardSubresource(&event) {
		// A non-/scale subresource (or an unmapped-verb subresource): dropped before recording.
		outcome.Record(ctx, &event, outcome.NonScaleSubresource)
		return nil
	}

	if decision := classifyAuditIngress(&event); !decision.Process {
		outcome.Record(ctx, &event, outcome.Outcome(decision.Reason))
		log.V(1).Info("Dropped audit event before recording",
			"reason", decision.Reason, "gvr", extractGVR(&event), "auditID", event.AuditID)
		return nil
	}

	if h.config.FactRecorder != nil {
		if err := h.config.FactRecorder.RecordFact(ctx, event); err != nil {
			outcome.Record(ctx, &event, outcome.WriteError)
			return fmt.Errorf("record attribution fact %q: %w", event.AuditID, err)
		}
	}
	outcome.Record(ctx, &event, outcome.Queued)

	h.firsts.factRecorded.Do(func() {
		log.Info("Recorded first audit attribution fact", "auditID", event.AuditID, "verb", event.Verb)
	})
	log.V(1).Info("Recorded audit attribution fact",
		"gvr", extractGVR(&event), "verb", event.Verb, "auditID", event.AuditID,
		"user", effectiveAuditUsername(event))
	return nil
}

// logAuditEventReceived emits the structured "audit event received" log (and the
// first-impersonation banner). The per-event count is recorded once as the event's
// outcome on gitopsreverser_audit_events_total, so there is no separate counter here.
func (h *AuditHandler) logAuditEventReceived(event auditv1.Event) {
	log := logf.Log.WithName("audit-handler")
	if event.ImpersonatedUser != nil {
		h.firsts.impersonatedEvent.Do(func() {
			log.Info("First impersonated audit event observed",
				"authUser", event.User.Username, "impersonatedUser", event.ImpersonatedUser.Username)
		})
	}
	group, version, resource := gvrParts(&event)
	log.V(1).Info("Audit event received",
		"group", group, "version", version, "resource", resource,
		"subresource", subresourcePart(&event), "verb", event.Verb, "user", effectiveAuditUsername(event))
}

// auditIngressDecision is the intrinsic accept/reject verdict for an audit event,
// derived purely from the event — stage, verb, status, body shape. Reason labels a
// rejection for drop diagnostics; it carries no knowledge of WatchRules.
type auditIngressDecision struct {
	Process bool
	Reason  string
}

// classifyAuditIngress is the intrinsic gate. It decides accept/reject from the
// event alone — stage, verb, response status, and body shape — and never consults
// WatchRules.
//
//   - A non-ResponseComplete stage, or a read-only/unknown verb (get/list/watch),
//     is never a Git-relevant mutation, so it is rejected.
//   - A request the API server rejected (responseStatus.code >= 300, e.g. a 409
//     Conflict) changed nothing in etcd, so it is rejected.
//   - A dry-run request (`dryRun=All`) was not persisted, so it is rejected.
//   - A mutation-shaped update/patch whose request-side RV equals the response RV
//     did not advance stored object state, so it is rejected as noise.
func classifyAuditIngress(event *auditv1.Event) auditIngressDecision {
	switch {
	case event == nil:
		return auditIngressDecision{Reason: string(outcome.NilEvent)}
	case event.Stage != auditv1.StageResponseComplete:
		return auditIngressDecision{Reason: string(outcome.Stage)}
	}
	if _, ok := auditutil.VerbToOperation(event.Verb); !ok {
		return auditIngressDecision{Reason: string(outcome.ReadOnlyOrUnknownVerb)}
	}
	if isFailedAuditRequest(event) {
		return auditIngressDecision{Reason: string(outcome.FailedRequest)}
	}
	if isDryRunAllRequest(event) {
		return auditIngressDecision{Reason: string(outcome.DryRun)}
	}
	if hasUnchangedResourceVersion(event) {
		return auditIngressDecision{Reason: string(outcome.UnchangedResourceVersion)}
	}
	return auditIngressDecision{Process: true}
}

// isFailedAuditRequest reports whether the API server rejected the request the
// audit event describes. A non-success responseStatus.code (>= 300) means the
// mutation never reached etcd, and the event's responseObject is a metav1.Status
// error body rather than the resource.
func isFailedAuditRequest(event *auditv1.Event) bool {
	return event != nil && event.ResponseStatus != nil && event.ResponseStatus.Code >= 300
}

func isDryRunAllRequest(event *auditv1.Event) bool {
	if event == nil || event.RequestURI == "" {
		return false
	}
	parsed, err := url.Parse(event.RequestURI)
	if err != nil {
		return false
	}
	for _, value := range parsed.Query()["dryRun"] {
		if value == metav1.DryRunAll {
			return true
		}
	}
	return false
}

func hasUnchangedResourceVersion(event *auditv1.Event) bool {
	if event == nil {
		return false
	}
	responseRV := metadataResourceVersion(event.ResponseObject)
	if responseRV == "" {
		return false
	}
	requestRV := metadataResourceVersion(event.RequestObject)
	return requestRV != "" && requestRV == responseRV
}

func metadataResourceVersion(object *runtime.Unknown) string {
	if object == nil || len(object.Raw) == 0 {
		return ""
	}
	var probe struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(object.Raw, &probe); err != nil {
		return ""
	}
	return probe.Metadata.ResourceVersion
}

// shouldForwardSubresource is the cheap subresource forwarding gate. Top-level
// resource events always pass. A subresource event passes only when it is a mutating
// /scale — the single subresource whose author is worth attributing to the parent
// (the parent watch event lands at the scale response RV); status, exec, log, and
// every other subresource is dropped before recording.
func shouldForwardSubresource(event *auditv1.Event) bool {
	if event.ObjectRef == nil || event.ObjectRef.Subresource == "" {
		return true
	}
	if _, ok := auditutil.VerbToOperation(event.Verb); !ok {
		return false
	}
	return auditutil.IsScaleSubresource(event.ObjectRef.Subresource)
}

func effectiveAuditUsername(event auditv1.Event) string {
	if event.ImpersonatedUser != nil && event.ImpersonatedUser.Username != "" {
		return event.ImpersonatedUser.Username
	}
	return event.User.Username
}

// validateAuditWebhookPath accepts only the canonical /audit-webhook path. The
// aggregated-API body proxy and its /audit-webhook-additional endpoint were removed
// with the watch-first rewrite — watch carries the body, so there is no body to join.
func validateAuditWebhookPath(path string) error {
	switch path {
	case "/audit-webhook":
		return nil
	case "/audit-webhook/":
		return errors.New("audit webhook path must not include a trailing slash")
	default:
		if strings.HasPrefix(path, "/audit-webhook/") {
			return errors.New("audit webhook path must not include a cluster ID or extra path segment")
		}
		return errors.New("invalid path; expected /audit-webhook")
	}
}

// gvrParts splits an audit event's objectRef into bounded group/version/resource
// metric label values. An empty group denotes the core API group; an absent or
// unparseable ref collapses to "unknown"/"invalid" so the label set stays small.
func gvrParts(event *auditv1.Event) (string, string, string) {
	if event.ObjectRef == nil || event.ObjectRef.APIVersion == "" {
		return "unknown", "unknown", "unknown"
	}
	gv, err := schema.ParseGroupVersion(event.ObjectRef.APIVersion)
	if err != nil {
		return "invalid", event.ObjectRef.APIVersion, orUnknownResource(event.ObjectRef.Resource)
	}
	return gv.Group, gv.Version, orUnknownResource(event.ObjectRef.Resource)
}

// subresourcePart returns the audit event's objectRef.subresource, or "" when the
// event has no objectRef or targets a top-level resource.
func subresourcePart(event *auditv1.Event) string {
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
func extractGVR(event *auditv1.Event) string {
	group, version, resource := gvrParts(event)
	if group == "" {
		return fmt.Sprintf("/%s/%s", version, resource)
	}
	return fmt.Sprintf("%s/%s/%s", group, version, resource)
}
