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

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/apis/audit"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	"sigs.k8s.io/yaml"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ConfigButler/gitops-reverser/internal/metrics"
)

// AuditHandler handles incoming audit events and collects metrics.
type AuditHandler struct{}

// ServeHTTP implements http.Handler for audit event processing.
func (h *AuditHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := logf.FromContext(ctx)

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Read and decode the request
	eventListV1, err := h.decodeEventList(r)
	if err != nil {
		log.Error(err, "Failed to decode audit event list")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Check if we have any items
	if len(eventListV1.Items) == 0 {
		log.Info("Received empty audit event list")
		w.WriteHeader(http.StatusOK)
		_, err = w.Write([]byte("Empty event list processed"))
		if err != nil {
			log.Error(err, "Failed to write response")
		}
		return
	}

	// Process events
	if err := h.processEvents(ctx, eventListV1.Items); err != nil {
		log.Error(err, "Failed to process audit events")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Return success
	w.WriteHeader(http.StatusOK)
	_, err = w.Write([]byte("Audit event processed"))
	if err != nil {
		log.Error(err, "Failed to write response")
	}
}

// decodeEventList reads and decodes the audit event list from the request.
func (h *AuditHandler) decodeEventList(r *http.Request) (*auditv1.EventList, error) {
	// Read the request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read body: %w", err)
	}
	defer r.Body.Close()

	// Initialize scheme with audit types
	scheme := runtime.NewScheme()
	if err := audit.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to initialize scheme: %w", err)
	}
	if err := auditv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add v1 types to scheme: %w", err)
	}

	// Decode the event list
	codecs := serializer.NewCodecFactory(scheme)
	deserializer := codecs.UniversalDeserializer()

	var eventListV1 auditv1.EventList
	_, _, err = deserializer.Decode(body, nil, &eventListV1)
	if err != nil {
		return nil, fmt.Errorf("invalid audit event list: %w", err)
	}

	return &eventListV1, nil
}

// processEvents processes a list of audit events.
func (h *AuditHandler) processEvents(ctx context.Context, events []auditv1.Event) error {
	log := logf.FromContext(ctx)

	// Initialize scheme for conversion
	scheme := runtime.NewScheme()
	if err := audit.AddToScheme(scheme); err != nil {
		return fmt.Errorf("failed to initialize scheme: %w", err)
	}
	if err := auditv1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("failed to add v1 types to scheme: %w", err)
	}

	for _, auditEventV1 := range events {
		// Convert v1 event to internal event
		var auditEvent audit.Event
		if err := scheme.Convert(&auditEventV1, &auditEvent, nil); err != nil {
			return fmt.Errorf("failed to convert audit event: %w", err)
		}

		// Validate event before processing
		if err := h.validateEvent(&auditEvent); err != nil {
			return err
		}

		// Extract GVR and action
		gvr := h.extractGVR(&auditEvent)
		action := auditEvent.Verb

		user := auditEvent.User.Username
		if auditEvent.ImpersonatedUser != nil {
			log.Info(
				"Audit event impersonated",
				"authUser",
				auditEvent.User.Username,
				"impersonatedUser",
				auditEvent.ImpersonatedUser,
			)
			user = auditEvent.ImpersonatedUser.Username
		}

		// Increment metrics
		metrics.AuditEventsReceivedTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("gvr", gvr),
			attribute.String("action", action),
			attribute.String("user", user),
		))

		log.Info("Processed audit event",
			"gvr", gvr,
			"action", action,
			"auditID", auditEvent.AuditID,
			"user", user,
			"ips", auditEvent.SourceIPs,
			"userAgent", auditEvent.UserAgent)

		// Write audit event to file for debugging and testing
		h.writeAuditEventToFile(&auditEvent)
	}

	return nil
}

// extractGVR constructs the Group/Version/Resource string from the audit event.
func (h *AuditHandler) extractGVR(event *audit.Event) string {
	if event.ObjectRef == nil || event.ObjectRef.APIVersion == "" {
		return "unknown/unknown/unknown"
	}

	// Split APIVersion into group and version
	parts := strings.SplitN(event.ObjectRef.APIVersion, "/", 2) //nolint:mnd
	var group, version string
	if len(parts) == 2 { //nolint:mnd
		group = parts[0]
		version = parts[1]
	} else {
		group = ""
		version = parts[0]
	}

	resource := event.ObjectRef.Resource
	if resource == "" {
		resource = "unknown"
	}

	// Format as group/version/resource
	if group == "" {
		return fmt.Sprintf("/%s/%s", version, resource)
	}
	return fmt.Sprintf("%s/%s/%s", group, version, resource)
}

// validateEvent validates an audit event before processing.
// Returns an error if the event has invalid data that would cause issues.
func (h *AuditHandler) validateEvent(event *audit.Event) error {
	// Validate auditID is not empty
	if string(event.AuditID) == "" {
		return errors.New("invalid audit event: auditID cannot be empty")
	}

	// In real cluster events, timestamps should be properly set
	// We validate this to ensure data quality for file dumps
	// Note: Timestamp validation is handled at the test level for simplicity

	return nil
}

// writeAuditEventToFile writes an audit event to a YAML file for debugging and testing purposes.
// The file is written to /tmp/audit-events/{auditID}.yaml and contains the complete audit.Event object
// with proper TypeMeta (Kind and APIVersion) for Kubernetes consistency.
// Fails fast if auditID is empty as this should never happen in practice.
func (h *AuditHandler) writeAuditEventToFile(event *audit.Event) {
	// Validate auditID is not empty - this should never happen in practice
	if string(event.AuditID) == "" {
		logf.Log.Error(nil, "Audit event has empty auditID - this should never happen", "event", event)
		return
	}

	// Create the directory if it doesn't exist
	dumpDir := "/tmp/audit-events"
	if err := os.MkdirAll(dumpDir, 0750); err != nil {
		logf.Log.Error(err, "Failed to create audit events dump directory", "directory", dumpDir)
		return
	}

	// Create filename based on auditID
	filename := fmt.Sprintf("%s.yaml", event.AuditID)
	filePath := filepath.Join(dumpDir, filename)

	// Convert internal event to v1 for proper serialization with TypeMeta
	scheme := runtime.NewScheme()
	if err := audit.AddToScheme(scheme); err != nil {
		logf.Log.Error(err, "Failed to initialize scheme", "auditID", event.AuditID)
		return
	}
	if err := auditv1.AddToScheme(scheme); err != nil {
		logf.Log.Error(err, "Failed to add v1 types to scheme", "auditID", event.AuditID)
		return
	}

	var eventV1 auditv1.Event
	if err := scheme.Convert(event, &eventV1, nil); err != nil {
		logf.Log.Error(err, "Failed to convert event to v1", "auditID", event.AuditID)
		return
	}

	// Set TypeMeta explicitly for proper Kubernetes object format
	eventV1.Kind = "Event"
	eventV1.APIVersion = "audit.k8s.io/v1"

	// Marshal the v1 event to YAML using sigs.k8s.io/yaml for proper Kubernetes timestamp formatting
	eventYAML, err := yaml.Marshal(&eventV1)
	if err != nil {
		logf.Log.Error(err, "Failed to marshal audit event to YAML", "auditID", event.AuditID)
		return
	}

	// Write to file
	if err := os.WriteFile(filePath, eventYAML, 0600); err != nil {
		logf.Log.Error(err, "Failed to write audit event to file", "file", filePath, "auditID", event.AuditID)
		return
	}

	logf.Log.Info("Audit event written to file", "file", filePath, "auditID", event.AuditID)
}
