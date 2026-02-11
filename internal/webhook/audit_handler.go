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
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/apis/audit"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	"sigs.k8s.io/yaml"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ConfigButler/gitops-reverser/internal/metrics"
)

const (
	// DefaultAuditDumpDir is the default directory for audit event dumps.
	DefaultAuditDumpDir = "/tmp/audit-events"
	// DefaultAuditMaxRequestBodyBytes limits incoming audit payload size.
	DefaultAuditMaxRequestBodyBytes = int64(10 * 1024 * 1024)
	// MaxClusterIDMetricLabelLength constrains label cardinality impact.
	MaxClusterIDMetricLabelLength = 63
	// UnknownClusterIDMetricValue is used when cluster ID cannot be labeled safely.
	UnknownClusterIDMetricValue = "unknown"
)

// AuditHandlerConfig contains configuration for the audit handler.
type AuditHandlerConfig struct {
	// DumpDir is the directory where audit events are written for debugging.
	// If empty, defaults to DefaultAuditDumpDir.
	DumpDir string
	// MaxRequestBodyBytes is the maximum accepted HTTP request body size.
	MaxRequestBodyBytes int64
}

// AuditHandler handles incoming audit events and collects metrics.
type AuditHandler struct {
	scheme       *runtime.Scheme
	deserializer runtime.Decoder
	config       AuditHandlerConfig
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

	clusterID, err := extractClusterID(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	reqLog := log.WithValues(
		"clusterID", clusterID,
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

	if err := h.processEvents(ctx, clusterID, eventListV1.Items); err != nil {
		reqLog.Error(err, "Failed to process audit events")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	reqLog.Info("Processed audit request", "eventCount", len(eventListV1.Items), "processingOutcome", "success")

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
func (h *AuditHandler) processEvents(ctx context.Context, clusterID string, events []auditv1.Event) error {
	log := logf.Log.WithName("audit-handler")
	clusterIDMetric := sanitizeClusterIDForMetric(clusterID)

	for _, auditEventV1 := range events {
		var auditEvent audit.Event
		if err := h.scheme.Convert(&auditEventV1, &auditEvent, nil); err != nil {
			return fmt.Errorf("failed to convert audit event: %w", err)
		}

		process, err := h.checkEvent(&auditEvent)
		if err != nil {
			return fmt.Errorf("failed to check audit event: %w", err)
		}

		gvr := h.extractGVR(&auditEvent)
		action := auditEvent.Verb

		user := auditEvent.User.Username
		if auditEvent.ImpersonatedUser != nil {
			log.Info(
				"Audit event impersonated",
				"clusterID",
				clusterID,
				"authUser",
				auditEvent.User.Username,
				"impersonatedUser",
				auditEvent.ImpersonatedUser,
			)
			user = auditEvent.ImpersonatedUser.Username
		}

		metrics.AuditEventsReceivedTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("cluster_id", clusterIDMetric),
			attribute.String("gvr", gvr),
			attribute.String("action", action),
			attribute.String("user", user),
			attribute.String("processed", strconv.FormatBool(process)),
		))

		if process {
			// For now we hardly do a thing
			log.Info("Processed audit event",
				"clusterID", clusterID,
				"gvr", gvr,
				"action", action,
				"auditID", auditEvent.AuditID,
				"user", user,
				"ips", auditEvent.SourceIPs,
				"userAgent", auditEvent.UserAgent)

			h.writeAuditEventToFile(&auditEvent)
		}
	}

	return nil
}

func extractClusterID(path string) (string, error) {
	const auditPrefix = "/audit-webhook/"
	if path == "/audit-webhook" {
		return "", errors.New("missing cluster ID in path; expected /audit-webhook/{clusterID}")
	}
	if !strings.HasPrefix(path, auditPrefix) {
		return "", errors.New("invalid path; expected /audit-webhook/{clusterID}")
	}

	clusterID := strings.TrimPrefix(path, auditPrefix)
	clusterID = strings.TrimSuffix(clusterID, "/")
	if clusterID == "" {
		return "", errors.New("missing cluster ID in path; expected /audit-webhook/{clusterID}")
	}
	if strings.Contains(clusterID, "/") {
		return "", errors.New("invalid path; expected single segment cluster ID in /audit-webhook/{clusterID}")
	}

	return clusterID, nil
}

func sanitizeClusterIDForMetric(clusterID string) string {
	clusterID = strings.TrimSpace(clusterID)
	if clusterID == "" {
		return UnknownClusterIDMetricValue
	}

	var builder strings.Builder
	builder.Grow(len(clusterID))
	for _, r := range clusterID {
		if isAllowedClusterIDRune(r) {
			builder.WriteRune(r)
			continue
		}
		builder.WriteByte('_')
	}

	sanitized := builder.String()
	if len(sanitized) > MaxClusterIDMetricLabelLength {
		sanitized = sanitized[:MaxClusterIDMetricLabelLength]
	}
	if sanitized == "" {
		return UnknownClusterIDMetricValue
	}

	return sanitized
}

func isAllowedClusterIDRune(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		r == '-' || r == '_' || r == '.'
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

// checkEvent validates an audit event before processing.
func (h *AuditHandler) checkEvent(event *audit.Event) (bool, error) {
	process := event.ObjectRef.Subresource != "status"
	if string(event.AuditID) == "" {
		return process, errors.New("invalid audit event: auditID cannot be empty")
	}

	return process, nil
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

	logf.Log.Info("Audit event written to file", "file", filePath, "auditID", event.AuditID)
}
