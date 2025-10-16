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

// Package webhook handles admission webhook requests for the GitOps Reverser controller.
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

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/eventqueue"
	"github.com/ConfigButler/gitops-reverser/internal/metrics"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

//nolint:lll // Kubebuilder webhook annotation
// +kubebuilder:webhook:path=/process-validating-webhook,mutating=false,failurePolicy=ignore,sideEffects=None,groups="*",resources="*",verbs=create;update;delete,versions="*",name=gitops-reverser.configbutler.ai,admissionReviewVersions=v1

// EventHandler handles all incoming admission requests.
type EventHandler struct {
	Client                     client.Client
	Decoder                    *admission.Decoder
	RuleStore                  *rulestore.RuleStore
	EventQueue                 *eventqueue.Queue
	EnableVerboseAdmissionLogs bool
}

// Handle implements admission.Handler.
//

func (h *EventHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := logf.FromContext(ctx)

	// Metrics: attribute role based on pod labels
	roleAttr := h.getPodRoleAttribute(ctx)
	metrics.EventsReceivedTotal.Add(ctx, 1, metric.WithAttributes(roleAttr))

	// Optional verbose request log
	if h.EnableVerboseAdmissionLogs {
		log.Info(
			"Received admission request",
			"operation", req.Operation,
			"kind", req.Kind.Kind,
			"name", req.Name,
			"namespace", req.Namespace,
		)
	}

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

	// Extract request parameters
	resourcePlural := req.Resource.Resource
	operation := configv1alpha1.OperationType(req.Operation)
	apiGroup := req.Resource.Group
	apiVersion := req.Resource.Version

	// Determine scope and any namespace labels
	isClusterScoped := obj.GetNamespace() == ""
	namespaceLabels := h.getNamespaceLabels(ctx, obj, isClusterScoped)

	// Rule matching
	matchingRules := h.RuleStore.GetMatchingRules(
		obj, resourcePlural, operation, apiGroup, apiVersion, isClusterScoped,
	)
	matchingClusterRules := h.RuleStore.GetMatchingClusterRules(
		resourcePlural, operation, apiGroup, apiVersion, isClusterScoped, namespaceLabels,
	)

	// Process results: logging and enqueue
	h.processRuleMatches(ctx, obj, req, matchingRules, matchingClusterRules)

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

// getNamespaceLabels returns namespace labels for namespaced resources, or nil for cluster-scoped.
func (h *EventHandler) getNamespaceLabels(
	ctx context.Context,
	obj *unstructured.Unstructured,
	isClusterScoped bool,
) map[string]string {
	if isClusterScoped {
		return nil
	}
	ns := &corev1.Namespace{}
	if err := h.Client.Get(ctx, apitypes.NamespacedName{Name: obj.GetNamespace()}, ns); err == nil {
		return ns.Labels
	}
	return nil
}

// processRuleMatches logs match details and enqueues events for WatchRule and ClusterWatchRule matches.
func (h *EventHandler) processRuleMatches(
	ctx context.Context,
	obj *unstructured.Unstructured,
	req admission.Request,
	matchingRules []rulestore.CompiledRule,
	matchingClusterRules []rulestore.CompiledClusterRule,
) {
	log := logf.FromContext(ctx)

	totalMatches := len(matchingRules) + len(matchingClusterRules)

	if h.EnableVerboseAdmissionLogs {
		log.Info(
			"Checking for matching rules", //nolint:lll // Structured log
			"kind", obj.GetKind(),
			"resourcePlural", req.Resource.Resource,
			"name", obj.GetName(),
			"namespace", obj.GetNamespace(),
			"matchingWatchRules", len(matchingRules),
			"matchingClusterWatchRules", len(matchingClusterRules),
		)
	}

	if totalMatches == 0 {
		// Only log for non-system resources to avoid spam
		if obj.GetNamespace() != "kube-system" && obj.GetNamespace() != "kube-node-lease" && obj.GetKind() != "Lease" {
			log.Info("No matching rules found for resource",
				"kind", obj.GetKind(), "name", obj.GetName(), "namespace", obj.GetNamespace())
		}
		return
	}

	identifier := types.FromAdmissionRequest(req)
	log.Info(
		fmt.Sprintf("Received %s for %s: matched %d watchrule(s) and %d clusterwatchrule(s)",
			req.Operation, identifier.String(), len(matchingRules), len(matchingClusterRules)),
	)

	// Enqueue events for WatchRule matches
	for _, rule := range matchingRules {
		h.enqueueEvent(
			ctx,
			obj,
			identifier,
			req,
			rule.GitRepoConfigRef,
			rule.Source.Namespace,
			rule.Branch,
			rule.BaseFolder,
		)
	}

	// Enqueue events for ClusterWatchRule matches
	for _, clusterRule := range matchingClusterRules {
		h.enqueueEvent(
			ctx,
			obj,
			identifier,
			req,
			clusterRule.GitRepoConfigRef,
			clusterRule.GitRepoConfigNamespace,
			clusterRule.Branch,
			clusterRule.BaseFolder,
		)
	}
}

// enqueueEvent creates and enqueues an event for processing.
func (h *EventHandler) enqueueEvent(
	ctx context.Context,
	obj *unstructured.Unstructured,
	identifier types.ResourceIdentifier,
	req admission.Request,
	gitRepoConfigRef string,
	gitRepoConfigNamespace string,
	branch string,
	baseFolder string,
) {
	sanitizedObj := sanitize.Sanitize(obj)
	event := eventqueue.Event{
		Object:     sanitizedObj,
		Identifier: identifier,
		Operation:  string(req.Operation),
		UserInfo: eventqueue.UserInfo{
			Username: req.UserInfo.Username,
			UID:      req.UserInfo.UID,
		},
		GitRepoConfigRef:       gitRepoConfigRef,
		GitRepoConfigNamespace: gitRepoConfigNamespace,
		Branch:                 branch,
		BaseFolder:             baseFolder,
	}
	h.EventQueue.Enqueue(event)
	roleAttr := h.getPodRoleAttribute(ctx)
	metrics.EventsProcessedTotal.Add(ctx, 1, metric.WithAttributes(roleAttr))
	metrics.GitCommitQueueSize.Add(ctx, 1, metric.WithAttributes(roleAttr))
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
