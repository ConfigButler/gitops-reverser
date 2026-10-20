// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/metric"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// A folder a trigger applies belongs to that trigger's applier (docs/design/applier-echoes.md, F4).
// An object another applier applies, mirrored into it, would be applied by this applier too, and two
// appliers applying one object fight each other. So an object owned by another applier is excluded
// from a triggered folder: a live event of it is not mirrored, and a snapshot of it is neither
// upserted nor swept. A folder with no trigger excludes nothing: mirroring another applier's objects
// one way is what such a folder can exist for.
//
// The owner is read from the raw object, where the watch holds it before sanitize strips it: Flux's
// owner labels, and Argo CD's tracking annotation, which names its Application.

const (
	// fluxOwnerNameLabel and fluxOwnerNamespaceLabel name the Kustomization that applied an object.
	fluxOwnerNameLabel      = "kustomize.toolkit.fluxcd.io/name"
	fluxOwnerNamespaceLabel = "kustomize.toolkit.fluxcd.io/namespace"
	// argoCDTrackingAnnotation is <application>:<group>/<kind>:<namespace>/<name>, where <application>
	// is the Application's name, prefixed with <namespace>_ when it lives outside Argo CD's own
	// namespace (Application.InstanceName in external-sources/argo-cd).
	argoCDTrackingAnnotation = "argocd.argoproj.io/tracking-id"
)

// AppliedBy is the applier that owns an object, as the object itself says. The zero value is an
// object no applier owns.
type AppliedBy struct {
	// Kind is v1alpha3.ApplierKindKustomization or v1alpha3.ApplierKindApplication.
	Kind string
	// Namespace is the applier's namespace; empty for an Argo CD Application in Argo CD's own
	// namespace, which its tracking annotation does not name.
	Namespace string
	Name      string
}

// IsZero reports whether no applier owns the object.
func (a AppliedBy) IsZero() bool { return a.Kind == "" }

// String is the owner for logs.
func (a AppliedBy) String() string { return a.Kind + "/" + a.Namespace + "/" + a.Name }

// is reports whether the owner is the applier ref.
func (a AppliedBy) is(ref ApplierRef) bool {
	if a.Kind != ref.Kind || a.Name != ref.Name {
		return false
	}
	return a.Namespace == ref.Namespace || (a.Kind == v1alpha3.ApplierKindApplication && a.Namespace == "")
}

// AppliedByOf reads the applier that owns an object from its raw labels and annotations: Flux's owner
// labels, or else Argo CD's tracking annotation. It must be called before sanitize.
func AppliedByOf(u *unstructured.Unstructured) AppliedBy {
	if u == nil {
		return AppliedBy{}
	}
	labels := u.GetLabels()
	if name, namespace := labels[fluxOwnerNameLabel], labels[fluxOwnerNamespaceLabel]; name != "" && namespace != "" {
		return AppliedBy{Kind: v1alpha3.ApplierKindKustomization, Namespace: namespace, Name: name}
	}
	tracking := u.GetAnnotations()[argoCDTrackingAnnotation]
	app, _, found := strings.Cut(tracking, ":")
	if !found || app == "" {
		return AppliedBy{}
	}
	if namespace, name, qualified := strings.Cut(app, "_"); qualified {
		return AppliedBy{Kind: v1alpha3.ApplierKindApplication, Namespace: namespace, Name: name}
	}
	return AppliedBy{Kind: v1alpha3.ApplierKindApplication, Name: app}
}

// applierOf is the applier whose trigger a GitTarget names on this branch, nil when it names none.
func (l *branchWorkerEventLoop) applierOf(name, namespace string) *applierState {
	for _, ref := range l.sortedAppliers() {
		state := l.appliers[ref]
		for _, target := range state.targets {
			if target.ref.Name == name && target.ref.Namespace == namespace {
				return state
			}
		}
	}
	return nil
}

// ownedElsewhere reports whether an object of the GitTarget's folder is owned by an applier other
// than the one the folder's trigger names, and counts and logs it when it is.
func (l *branchWorkerEventLoop) ownedElsewhere(name, namespace, object string, owner AppliedBy) bool {
	if owner.IsZero() {
		return false
	}
	applier := l.applierOf(name, namespace)
	if applier == nil || owner.is(applier.ref) {
		return false
	}
	l.recordOwnershipConflict(namespace+"/"+name, object, applier.ref, owner)
	return true
}

// noteCreated leaves an adopt pending for the applier of the folder an API create landed in (S2 in
// docs/design/applier-echoes.md). An applier prunes only what it applied, and an object the API
// created carries neither Flux's owner labels nor Argo CD's tracking annotation, even when its
// content equals what Git holds, so a later Git-side delete would leave it on the cluster. A create
// whose newest managers are an applier's is that applier's own apply, and adopts nothing; one another
// applier owns never reaches here (ownedElsewhere).
func (l *branchWorkerEventLoop) noteCreated(event Event) {
	if writtenByAnApplier(event.LastFieldManagers) {
		return
	}
	l.adopt(event.GitTargetName, event.GitTargetNamespace, event.Identifier.String())
}

// adopt leaves an adopt pending for the applier of a GitTarget's folder, when its trigger names one.
func (l *branchWorkerEventLoop) adopt(name, namespace, object string) {
	applier := l.applierOf(name, namespace)
	if applier == nil {
		return
	}
	if !applier.adopt {
		l.w.Log.Info("An object the API created is in a folder an applier applies; it is adopted at the "+
			"next apply, so a later Git-side delete prunes it", "gitTarget", namespace+"/"+name,
			"object", object, "applier", applier.ref.String())
	}
	applier.adopt = true
}

// writtenByAnApplier reports whether every one of managers is a field manager of an applier kind.
func writtenByAnApplier(managers []string) bool {
	if len(managers) == 0 {
		return false
	}
	for _, kind := range []string{v1alpha3.ApplierKindKustomization, v1alpha3.ApplierKindApplication} {
		if _, alone := (ApplierRef{Kind: kind}).writtenBy(managers); alone {
			return true
		}
	}
	return false
}

// withoutOwnedElsewhere is a resync's snapshot without the objects another applier owns, and those
// objects by objectKey, for the sweep to leave alone.
func (l *branchWorkerEventLoop) withoutOwnedElsewhere(
	req *ResyncRequest,
) ([]manifestanalyzer.DesiredResource, map[string]bool) {
	if len(req.Owners) == 0 {
		return req.Desired, nil
	}
	desired := make([]manifestanalyzer.DesiredResource, 0, len(req.Desired))
	var kept map[string]bool
	for _, dr := range req.Desired {
		owner := req.Owners[dr.Resource]
		if !l.ownedElsewhere(req.GitTargetName, req.GitTargetNamespace, dr.Resource.String(), owner) {
			desired = append(desired, dr)
			continue
		}
		if kept == nil {
			kept = map[string]bool{}
		}
		kept[objectKey(dr.Resource)] = true
	}
	return desired, kept
}

// recordOwnershipConflict counts an object another applier owns, kept out of a triggered folder, and
// logs it once per object.
func (l *branchWorkerEventLoop) recordOwnershipConflict(target, object string, applier ApplierRef, owner AppliedBy) {
	if telemetry.ApplyOwnershipConflictsTotal != nil {
		telemetry.ApplyOwnershipConflictsTotal.Add(context.WithoutCancel(l.w.ctx), 1,
			metric.WithAttributes(l.w.providerAttrs()...))
	}
	key := target + "|" + object
	if _, logged := l.ownershipLogged[key]; logged {
		return
	}
	if l.ownershipLogged == nil {
		l.ownershipLogged = map[string]struct{}{}
	}
	l.ownershipLogged[key] = struct{}{}
	l.w.Log.Info("Not mirroring an object another applier owns into a folder its trigger's applier applies: "+
		"two appliers applying one object fight each other",
		"gitTarget", target, "object", object, "applier", applier.String(), "ownedBy", owner.String())
}
