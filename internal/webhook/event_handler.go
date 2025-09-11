package webhook

import (
	"context"
	"fmt"
	"net/http"

	"github.com/ConfigButler/gitops-reverser/internal/eventqueue"
	"github.com/ConfigButler/gitops-reverser/internal/metrics"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/sanitize"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/validate-v1-event,mutating=false,failurePolicy=ignore,sideEffects=None,groups="*",resources="*",verbs=create;update;delete,versions="*",name=gitops-reverser.configbutler.ai,admissionReviewVersions=v1

// EventHandler handles all incoming admission requests.
type EventHandler struct {
	Client     client.Client
	Decoder    *admission.Decoder
	RuleStore  *rulestore.RuleStore
	EventQueue *eventqueue.Queue
}

// Handle implements admission.Handler.
func (h *EventHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := logf.FromContext(ctx)
	metrics.EventsReceivedTotal.Add(ctx, 1)

	log.Info("Received admission request", "operation", req.Operation, "kind", req.Kind.Kind, "name", req.Name, "namespace", req.Namespace)

	if h.Decoder == nil {
		log.Error(fmt.Errorf("decoder is not initialized"), "Webhook handler received request but decoder is nil")
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("decoder is not initialized"))
	}

	obj := &unstructured.Unstructured{}
	err := (*h.Decoder).Decode(req, obj)
	if err != nil {
		log.Error(err, "Failed to decode admission request", "operation", req.Operation, "kind", req.Kind.Kind)
		return admission.Errored(http.StatusBadRequest, err)
	}

	log.V(1).Info("Successfully decoded resource", "kind", obj.GetKind(), "name", obj.GetName(), "namespace", obj.GetNamespace(), "operation", req.Operation)

	matchingRules := h.RuleStore.GetMatchingRules(obj)
	if len(matchingRules) > 0 {
		// Enqueue an event for each matching rule.
		for _, rule := range matchingRules {
			sanitizedObj := sanitize.Sanitize(obj)
			event := eventqueue.Event{
				Object:                 sanitizedObj,
				Request:                req,
				GitRepoConfigRef:       rule.GitRepoConfigRef,
				GitRepoConfigNamespace: rule.Source.Namespace, // Same namespace as the WatchRule
			}
			h.EventQueue.Enqueue(event)
			metrics.EventsProcessedTotal.Add(ctx, 1)
			metrics.GitCommitQueueSize.Add(ctx, 1)
			logf.FromContext(ctx).Info("Enqueued event for matched resource", "resource", sanitizedObj.GetName(), "namespace", sanitizedObj.GetNamespace(), "kind", sanitizedObj.GetKind(), "rule", rule.Source.Name)
		}
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
