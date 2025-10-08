// Package webhook handles admission webhook requests for the GitOps Reverser controller.
package webhook

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/ConfigButler/gitops-reverser/internal/eventqueue"
	"github.com/ConfigButler/gitops-reverser/internal/metrics"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
)

//nolint:lll // Kubebuilder webhook annotation
// +kubebuilder:webhook:path=/validate-v1-event,mutating=false,failurePolicy=ignore,sideEffects=None,groups="*",resources="*",verbs=create;update;delete,versions="*",name=gitops-reverser.configbutler.ai,admissionReviewVersions=v1

// EventHandler handles all incoming admission requests.
type EventHandler struct {
	Client                     client.Client
	Decoder                    *admission.Decoder
	RuleStore                  *rulestore.RuleStore
	EventQueue                 *eventqueue.Queue
	EnableVerboseAdmissionLogs bool
}

// Handle implements admission.Handler.
func (h *EventHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := logf.FromContext(ctx)
	metrics.EventsReceivedTotal.Add(ctx, 1)

	if h.EnableVerboseAdmissionLogs {
		log.Info(
			"Received admission request",
			"operation",
			req.Operation,
			"kind",
			req.Kind.Kind,
			"name",
			req.Name,
			"namespace",
			req.Namespace,
		)
	}

	if h.Decoder == nil {
		log.Error(errors.New("decoder is not initialized"), "Webhook handler received request but decoder is nil")
		return admission.Errored(http.StatusInternalServerError, errors.New("decoder is not initialized"))
	}

	obj := &unstructured.Unstructured{}
	var err error

	// Decode based on operation type and available data
	switch {
	case req.Operation == "DELETE" && req.OldObject.Size() > 0:
		err = (*h.Decoder).DecodeRaw(req.OldObject, obj)
	case req.Object.Size() > 0:
		err = (*h.Decoder).Decode(req, obj)
	default:
		// If no object data is available, create a minimal object from admission request metadata
		log.V(1).Info("No object data available, creating minimal object from request metadata")
		obj.SetAPIVersion(req.Kind.Group + "/" + req.Kind.Version)
		obj.SetKind(req.Kind.Kind)
		obj.SetName(req.Name)
		obj.SetNamespace(req.Namespace)
	}

	if err != nil {
		log.Error(err, "Failed to decode admission request", "operation", req.Operation, "kind", req.Kind.Kind)
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("failed to decode request: %w", err))
	}

	log.V(1).Info("Successfully decoded resource", //nolint:lll // Structured log
		"kind", obj.GetKind(), "name", obj.GetName(), "namespace", obj.GetNamespace(), "operation", req.Operation)

	matchingRules := h.RuleStore.GetMatchingRules(obj)
	if h.EnableVerboseAdmissionLogs {
		log.Info(
			"Checking for matching rules", //nolint:lll // Structured log
			"kind",
			obj.GetKind(),
			"name",
			obj.GetName(),
			"namespace",
			obj.GetNamespace(),
			"matchingRulesCount",
			len(matchingRules),
		)
	}

	if len(matchingRules) > 0 {
		log.Info("Found matching rules, enqueueing events", "matchingRulesCount", len(matchingRules))
		// Enqueue an event for each matching rule.
		for _, rule := range matchingRules {
			sanitizedObj := sanitize.Sanitize(obj)
			event := eventqueue.Event{
				Object:                 sanitizedObj,
				Request:                req,
				ResourcePlural:         req.Resource.Resource, // Use plural from admission request
				GitRepoConfigRef:       rule.GitRepoConfigRef,
				GitRepoConfigNamespace: rule.Source.Namespace, // Same namespace as the WatchRule
			}
			h.EventQueue.Enqueue(event)
			metrics.EventsProcessedTotal.Add(ctx, 1)
			metrics.GitCommitQueueSize.Add(ctx, 1)
			logf.FromContext(ctx).Info("Enqueued event for matched resource", //nolint:lll // Structured log
				"resource", sanitizedObj.GetName(), "namespace", sanitizedObj.GetNamespace(), "kind", sanitizedObj.GetKind(), "rule", rule.Source.Name)
		}
	} else if obj.GetNamespace() != "kube-system" && obj.GetNamespace() != "kube-node-lease" && obj.GetKind() != "Lease" {
		// Only log for non-system resources to avoid spam
		log.Info("No matching rules found for resource", "kind", obj.GetKind(), "name", obj.GetName(), "namespace", obj.GetNamespace())
	}

	return admission.Allowed("request is allowed")
}

// InjectDecoder injects the decoder.
func (h *EventHandler) InjectDecoder(d *admission.Decoder) error {
	h.Decoder = d
	log := logf.Log.WithName("webhook")
	log.Info("Decoder successfully injected into EventHandler - ready to decode all Kubernetes resource types")
	return nil
}
