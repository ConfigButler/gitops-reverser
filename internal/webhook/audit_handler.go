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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"k8s.io/apiserver/pkg/apis/audit"

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

	// Read the request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Error(err, "Failed to read request body")
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// Parse the audit event list from the request body
	var eventList audit.EventList
	if err := json.Unmarshal(body, &eventList); err != nil {
		log.Error(err, "Failed to unmarshal audit event list")
		http.Error(w, "Invalid audit event list JSON", http.StatusBadRequest)
		return
	}

	// Check if we have any items
	if len(eventList.Items) == 0 {
		log.Info("Received empty audit event list")
		w.WriteHeader(http.StatusOK)
		_, err = w.Write([]byte("Empty event list processed"))
		if err != nil {
			log.Error(err, "Failed to write response")
		}
		return
	}

	// Process each event in the list
	for _, auditEvent := range eventList.Items {
		// Extract GVR and action
		gvr := h.extractGVR(&auditEvent)
		action := auditEvent.Verb

		// Increment metrics
		metrics.AuditEventsReceivedTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("gvr", gvr),
			attribute.String("action", action),
		))

		log.Info("Processed audit event",
			"gvr", gvr,
			"action", action,
			"auditID", auditEvent.AuditID,
			"user", auditEvent.User.Username)
	}

	// Return success
	w.WriteHeader(http.StatusOK)
	_, err = w.Write([]byte("Audit event processed"))
	if err != nil {
		log.Error(err, "Failed to write response")
	}
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
