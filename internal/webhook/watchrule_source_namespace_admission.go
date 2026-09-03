// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// isWatchRuleKind reports whether gr is the namespaced WatchRule. ClusterWatchRule is deliberately
// absent: it watches cluster-scoped resources, which have no namespace to bring to a target.
func isWatchRuleKind(gr metav1.GroupResource) bool {
	return gr == metav1.GroupResource{Group: "configbutler.ai", Resource: "watchrules"}
}

// validateWatchRuleSourceNamespaces is the FEEDBACK half of the one-source-namespace rule
// (docs/layout/model.md § "The second guard"): a GitTarget that declared its folder namespace-free
// admits exactly one source namespace, and a rule bringing a second one is rejected here, at the
// moment the mistake is made, instead of leaving a target that refuses every write until someone
// reads its status.
//
// It is not the rule's enforcement. The writer's own precondition is, and it holds whatever this
// returns: this check is one-shot, so it cannot see a serializeNamespace flipped to false after the
// rules were created, the webhook is fail-open (failurePolicy: Ignore), and a cluster can run the
// operator without the admission server at all. Everything that must be true of the bytes is
// decided at the write. See docs/spec/where-validation-lives.md.
//
// Every failure to evaluate ALLOWS, for the same reason: a rejection this handler cannot justify
// is a rejection of the user's object on the strength of an unread GitTarget.
func (h *ValidateOperatorTypesHandler) validateWatchRuleSourceNamespaces(
	ctx context.Context,
	req admission.Request,
) admission.Response {
	log := logf.FromContext(ctx).WithName("watchrule-source-namespaces")
	if h.Client == nil {
		return admission.Allowed("no client: source namespaces not evaluated")
	}

	var rule v1alpha3.WatchRule
	if err := json.Unmarshal(req.Object.Raw, &rule); err != nil {
		log.Error(err, "could not decode the WatchRule under review", "namespace", req.Namespace, "name", req.Name)
		return admission.Allowed("undecodable: source namespaces not evaluated")
	}
	// A CREATE carries no namespace on the object itself until the API server defaults it.
	if rule.Namespace == "" {
		rule.Namespace = req.Namespace
	}

	var target v1alpha3.GitTarget
	targetKey := k8stypes.NamespacedName{Namespace: rule.Namespace, Name: rule.Spec.GitTargetRef.Name}
	if err := h.Client.Get(ctx, targetKey, &target); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "could not read the GitTarget this rule names", "gitTarget", targetKey)
		}
		// A rule naming a target that does not exist yet is ordinary: rules and targets are
		// applied in whatever order the manifest happens to list them, and the reconciler holds
		// such a rule unready. It is not this check's business.
		return admission.Allowed("GitTarget unreadable: source namespaces not evaluated")
	}
	if target.Spec.SerializeNamespace == nil || *target.Spec.SerializeNamespace {
		return admission.Allowed("the target declares no namespace-free folder")
	}

	admitted, err := h.admittedSourceNamespaces(ctx, &target, rule.Name)
	if err != nil {
		log.Error(err, "could not list the sibling WatchRules", "gitTarget", targetKey)
		return admission.Allowed("sibling rules unreadable: source namespaces not evaluated")
	}

	for _, item := range rule.Spec.Rules {
		if item.IsSourceNamespaceWildcard() {
			return admission.Denied(fmt.Sprintf(
				"GitTarget %s/%s sets spec.serializeNamespace: false, which admits exactly one source "+
					"namespace, and sourceNamespace %q cannot be shown to be one namespace",
				target.Namespace, target.Name, v1alpha3.SourceNamespaceWildcard))
		}
		admitted[item.EffectiveSourceNamespace(rule.Namespace)] = struct{}{}
	}
	if len(admitted) <= 1 {
		return admission.Allowed("one source namespace")
	}

	names := make([]string, 0, len(admitted))
	for ns := range admitted {
		names = append(names, ns)
	}
	sort.Strings(names)
	return admission.Denied(fmt.Sprintf(
		"GitTarget %s/%s sets spec.serializeNamespace: false, which admits exactly one source namespace, "+
			"but this rule would make %v reach it; the documents it writes carry no metadata.namespace, so "+
			"two namespaces produce one document two objects overwrite in turn. Unset serializeNamespace on "+
			"the target so each document takes the namespace of the root governing it, or point this rule at "+
			"a target of its own",
		target.Namespace, target.Name, names))
}

// admittedSourceNamespaces is the set already reaching the target, excluding the rule under review
// so an UPDATE is judged on what it would become rather than on what it is. It mirrors
// (internal/git).resolveSourceNamespaces, which computes the same set at the write.
//
// A wildcard among the OTHER rules is not this request's fault, so it adds nothing here: the write
// refuses that target anyway, and rejecting an unrelated edit for it would leave the user unable to
// fix the rule that caused it.
func (h *ValidateOperatorTypesHandler) admittedSourceNamespaces(
	ctx context.Context,
	target *v1alpha3.GitTarget,
	excludeRule string,
) (map[string]struct{}, error) {
	var rules v1alpha3.WatchRuleList
	if err := h.Client.List(ctx, &rules, client.InNamespace(target.Namespace)); err != nil {
		return nil, err
	}
	admitted := map[string]struct{}{target.Namespace: {}}
	for i := range rules.Items {
		sibling := &rules.Items[i]
		if sibling.Name == excludeRule || sibling.Spec.GitTargetRef.Name != target.Name {
			continue
		}
		for _, item := range sibling.Spec.Rules {
			if item.IsSourceNamespaceWildcard() {
				continue
			}
			admitted[item.EffectiveSourceNamespace(sibling.Namespace)] = struct{}{}
		}
	}
	return admitted, nil
}
