// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"testing"

	"github.com/stretchr/testify/assert"
	k8stypes "k8s.io/apimachinery/pkg/types"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"

	"github.com/ConfigButler/gitops-reverser/internal/attribution"
	"github.com/ConfigButler/gitops-reverser/internal/audit/outcome"
)

// The audit census names a route only when a KubeProvider declares it: the producer picks the path
// segment or annotation, so an undeclared value collapses into one bucket instead of a series each.
func TestRouteLabel_IsBoundedByDeclaredRoutes(t *testing.T) {
	health := &attribution.RouteHealth{}
	provider := k8stypes.NamespacedName{Namespace: "team-a", Name: "prod"}
	health.DeclareRoute(provider, "team-a.prod")
	h := &AuditHandler{config: AuditHandlerConfig{RouteHealth: health, AuditRouteAnnotationKey: "route"}}

	assert.Equal(t, "team-a.prod", h.routeLabel(auditRoute{route: "team-a.prod"}, &auditv1.Event{}))
	assert.Equal(t, outcome.RouteUnrecognized, h.routeLabel(auditRoute{route: "made-up"}, &auditv1.Event{}))

	shared := auditRoute{annotationKey: "route"}
	annotated := &auditv1.Event{Annotations: map[string]string{"route": "team-a.prod"}}
	assert.Equal(t, "team-a.prod", h.routeLabel(shared, annotated), "the shared endpoint reads it per event")
	assert.Equal(t, outcome.RouteNone, h.routeLabel(shared, &auditv1.Event{}), "an event naming no route")

	health.ForgetRoute(provider)
	assert.Equal(t, outcome.RouteUnrecognized, h.routeLabel(auditRoute{route: "team-a.prod"}, &auditv1.Event{}),
		"a deleted provider's route is no longer a label value")
}
