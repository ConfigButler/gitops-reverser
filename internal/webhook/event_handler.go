// Package webhook handles admission webhook requests for the GitOps Reverser controller.
package webhook

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apitypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	configv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/eventqueue"
	"github.com/ConfigButler/gitops-reverser/internal/metrics"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	"github.com/ConfigButler/gitops-reverser/internal/types"
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
//
//nolint:funlen // Complex admission handler with multiple validation steps
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

	// Extract filtering parameters from admission request
	resourcePlural := req.Resource.Resource
	operation := configv1alpha1.OperationType(req.Operation)
	apiGroup := req.Resource.Group
	apiVersion := req.Resource.Version

	// Determine if resource is cluster-scoped (no namespace)
	isClusterScoped := obj.GetNamespace() == ""

	// Get namespace labels for ClusterWatchRule matching (only for namespaced resources)
	var namespaceLabels map[string]string
	if !isClusterScoped {
		ns := &corev1.Namespace{}
		if err := h.Client.Get(ctx, apitypes.NamespacedName{Name: obj.GetNamespace()}, ns); err == nil {
			namespaceLabels = ns.Labels
		}
	}

	// Get matching WatchRules
	matchingRules := h.RuleStore.GetMatchingRules(
		obj,
		resourcePlural,
		operation,
		apiGroup,
		apiVersion,
		isClusterScoped,
	)

	// Get matching ClusterWatchRules
	matchingClusterRules := h.RuleStore.GetMatchingClusterRules(
		resourcePlural,
		operation,
		apiGroup,
		apiVersion,
		isClusterScoped,
		namespaceLabels,
	)

	totalMatches := len(matchingRules) + len(matchingClusterRules)

	if h.EnableVerboseAdmissionLogs {
		log.Info(
			"Checking for matching rules", //nolint:lll // Structured log
			"kind",
			obj.GetKind(),
			"resourcePlural",
			resourcePlural,
			"name",
			obj.GetName(),
			"namespace",
			obj.GetNamespace(),
			"matchingWatchRules",
			len(matchingRules),
			"matchingClusterWatchRules",
			len(matchingClusterRules),
		)
	}

	if totalMatches > 0 {
		identifier := types.FromAdmissionRequest(req)
		log.Info(
			fmt.Sprintf("Received %s for %s: matched %d watchrule(s) and %d clusterwatchrule(s)",
				req.Operation,
				identifier.String(),
				len(matchingRules),
				len(matchingClusterRules)),
		)

		// Enqueue events for WatchRule matches
		for _, rule := range matchingRules {
			h.enqueueEvent(ctx, obj, identifier, req, rule.GitRepoConfigRef, rule.Source.Namespace)
		}

		// Enqueue events for ClusterWatchRule matches
		for _, clusterRule := range matchingClusterRules {
			h.enqueueEvent(ctx, obj, identifier, req, clusterRule.GitRepoConfigRef, clusterRule.GitRepoConfigNamespace)
		}
	} else if obj.GetNamespace() != "kube-system" && obj.GetNamespace() != "kube-node-lease" && obj.GetKind() != "Lease" {
		// Only log for non-system resources to avoid spam
		log.Info("No matching rules found for resource", "kind", obj.GetKind(), "name", obj.GetName(), "namespace", obj.GetNamespace())
	}

	return admission.Allowed("request is allowed")
}

// enqueueEvent creates and enqueues an event for processing.
func (h *EventHandler) enqueueEvent(
	ctx context.Context,
	obj *unstructured.Unstructured,
	identifier types.ResourceIdentifier,
	req admission.Request,
	gitRepoConfigRef string,
	gitRepoConfigNamespace string,
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
	}
	h.EventQueue.Enqueue(event)
	metrics.EventsProcessedTotal.Add(ctx, 1)
	metrics.GitCommitQueueSize.Add(ctx, 1)
}

// InjectDecoder injects the decoder.
func (h *EventHandler) InjectDecoder(d *admission.Decoder) error {
	h.Decoder = d
	log := logf.Log.WithName("webhook")
	log.Info("Decoder successfully injected into EventHandler - ready to decode all Kubernetes resource types")
	return nil
}
