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

// Package webhook handles admission webhook requests and experimental audit webhook for the GitOps Reverser controller.
// The admission webhook serves as a correlation store only - it captures user attribution but does NOT
// enqueue events. The watch path (informers) is the sole source of events to the queue.
// The audit webhook collects experimental metrics from Kubernetes audit events.
package webhook

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apitypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/ConfigButler/gitops-reverser/internal/correlation"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

//nolint:lll // Kubebuilder webhook annotation
// +kubebuilder:webhook:path=/process-validating-webhook,mutating=false,failurePolicy=ignore,sideEffects=None,groups="*",resources="*",verbs=create;update;delete,versions="*",name=gitops-reverser.configbutler.ai,admissionReviewVersions=v1

// EventHandler handles all incoming admission requests.
// It stores correlation entries (username → sanitized content) for ALL resources.
// The watch path will filter based on rules - webhook just captures user attribution.
type EventHandler struct {
	Client           client.Client
	Decoder          *admission.Decoder
	CorrelationStore *correlation.Store
}

// Handle implements admission.Handler.
//

func (h *EventHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := logf.FromContext(ctx)

	// Metrics: attribute role based on pod labels
	roleAttr := h.getPodRoleAttribute(ctx)
	telemetry.EventsReceivedTotal.Add(ctx, 1, metric.WithAttributes(roleAttr))

	// Safety: require decoder
	if h.Decoder == nil {
		err := errors.New("decoder is not initialized")
		log.Error(err, "Webhook handler received request but decoder is nil")
		return admission.Errored(http.StatusInternalServerError, err)
	}

	// Decode incoming object (handles create/update/delete and missing payloads)
	obj, err := h.decodeObject(ctx, req)
	if err != nil {
		log.Error(err, "Failed to decode admission request", "operation", req.Operation, "kind", req.Kind.Kind)
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("failed to decode request: %w", err))
	}

	log.V(1).Info("Successfully decoded resource", //nolint:lll // Structured log
		"kind", obj.GetKind(), "name", obj.GetName(), "namespace", obj.GetNamespace(), "operation", req.Operation)

	// Store correlation for ALL resources (watch path will filter based on rules)
	identifier := types.FromAdmissionRequest(req)
	h.storeCorrelation(ctx, obj, identifier, req)

	return admission.Allowed("request is allowed")
}

// decodeObject decodes the AdmissionRequest into an Unstructured object, with sensible fallbacks.
func (h *EventHandler) decodeObject(ctx context.Context, req admission.Request) (*unstructured.Unstructured, error) {
	log := logf.FromContext(ctx)
	obj := &unstructured.Unstructured{}

	switch {
	case req.Operation == "DELETE" && req.OldObject.Size() > 0:
		if err := (*h.Decoder).DecodeRaw(req.OldObject, obj); err != nil {
			return nil, err
		}
	case req.Object.Size() > 0:
		if err := (*h.Decoder).Decode(req, obj); err != nil {
			return nil, err
		}
	default:
		// If no object data is available, create a minimal object from admission request metadata
		log.V(1).Info("No object data available, creating minimal object from request metadata")
		obj.SetAPIVersion(req.Kind.Group + "/" + req.Kind.Version)
		obj.SetKind(req.Kind.Kind)
		obj.SetName(req.Name)
		obj.SetNamespace(req.Namespace)
	}

	return obj, nil
}

// storeCorrelation stores a correlation entry for ALL resources.
// Watch path will filter based on rules - webhook just captures user attribution.
func (h *EventHandler) storeCorrelation(
	ctx context.Context,
	obj *unstructured.Unstructured,
	identifier types.ResourceIdentifier,
	req admission.Request,
) {
	log := logf.FromContext(ctx)

	if h.CorrelationStore == nil {
		return
	}

	sanitizedObj := sanitize.Sanitize(obj)
	sanitizedYAML, err := sanitize.MarshalToOrderedYAML(sanitizedObj)
	if err != nil {
		log.Error(err, "Failed to marshal sanitized object for correlation", "identifier", identifier.String())
		return
	}

	key := correlation.GenerateKey(identifier, string(req.Operation), sanitizedYAML)
	h.CorrelationStore.Put(key, req.UserInfo.Username)

	// Increment webhook-specific correlation metric
	roleAttr := h.getPodRoleAttribute(ctx)
	telemetry.WebhookCorrelationsTotal.Add(ctx, 1, metric.WithAttributes(roleAttr))

	log.V(1).Info("Stored correlation entry (unfiltered)",
		"kind", obj.GetKind(),
		"name", obj.GetName(),
		"namespace", obj.GetNamespace(),
		"username", req.UserInfo.Username,
		"operation", req.Operation)
}

// getPodRoleAttribute returns the role attribute for metrics based on pod labels.
func (h *EventHandler) getPodRoleAttribute(ctx context.Context) attribute.KeyValue {
	podName := os.Getenv("POD_NAME")
	podNamespace := os.Getenv("POD_NAMESPACE")

	if podName == "" || podNamespace == "" {
		return attribute.String("role", "unknown")
	}

	pod := &corev1.Pod{}
	err := h.Client.Get(ctx, apitypes.NamespacedName{Name: podName, Namespace: podNamespace}, pod)
	if err != nil {
		return attribute.String("role", "unknown")
	}

	// Check if pod has the leader label
	if role, ok := pod.Labels["role"]; ok && role == "leader" {
		return attribute.String("role", "leader")
	}

	return attribute.String("role", "follower")
}

// InjectDecoder injects the decoder.
func (h *EventHandler) InjectDecoder(d *admission.Decoder) error {
	h.Decoder = d
	log := logf.Log.WithName("webhook")
	log.Info("Decoder successfully injected into EventHandler - ready to decode all Kubernetes resource types")
	return nil
}
