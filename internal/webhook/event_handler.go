package webhook

import (
	"context"
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

// +kubebuilder:webhook:path=/validate-v1-event,mutating=false,failurePolicy=ignore,sideEffects=None,groups="*",resources="*",verbs=create;update;delete,versions="*",name=vwatchrule.kb.io,admissionReviewVersions=v1

// EventHandler handles all incoming admission requests.
type EventHandler struct {
	Client     client.Client
	Decoder    *admission.Decoder
	RuleStore  *rulestore.RuleStore
	EventQueue *eventqueue.Queue
}

// Handle implements admission.Handler.
func (h *EventHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	metrics.EventsReceivedTotal.Add(ctx, 1)

	obj := &unstructured.Unstructured{}
	err := (*h.Decoder).Decode(req, obj)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	matchingRules := h.RuleStore.GetMatchingRules(obj)
	if len(matchingRules) > 0 {
		// Enqueue an event for each matching rule.
		for _, rule := range matchingRules {
			sanitizedObj := sanitize.Sanitize(obj)
			event := eventqueue.Event{
				Object:           sanitizedObj,
				Request:          req,
				GitRepoConfigRef: rule.GitRepoConfigRef,
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
	return nil
}
