// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// CompileWatchRule is THE ONLY PATH from a WatchRule to a compiled rule. It resolves the whole
// per-item source-namespace scope first and compiles only on an admitted verdict.
//
// It is one function, called by both the WatchRule reconciler and the watch manager's startup
// bootstrap, because two call sites that each remember to check is an arrangement this codebase
// has already got wrong once. Bootstrap lists every WatchRule and seeds the store BEFORE the first
// reconcile, then marks the store ready — so a gate the reconciler alone enforced would be
// bypassed for the whole startup window, on EVERY restart, which is exactly when nobody is
// watching. Routing compilation through here closes that by construction rather than by
// discipline: there is no second place that can call AddOrUpdateWatchRule for a WatchRule.
//
// Its two outcomes map onto the two things the caller must do:
//
//   - ADMITTED — the rule is compiled with every item resolved to its source-namespace set. The
//     caller publishes SourceNamespaceAuthorized=True.
//   - DENIED — any previously compiled rule is REMOVED here, before the caller publishes anything.
//     A gate that only writes a condition is not a gate; the caller must still replan the watch
//     manager and then publish the Failed trio, in that order.
//
// There is no third "cannot say yet" outcome, and there used to be. It existed because the gate's
// selector half read Namespace labels in ANOTHER cluster, which could be syncing, unreachable or
// Forbidden, and a rule with an already-resolved scope had to RETAIN it through such a gap rather
// than narrow to the empty set (a narrowed set is the input to a sweep, so failing closed there
// would have deleted a tenant's Git content over an outage). Every input is now a control-plane
// object this reconcile already read, so there is nothing left to retain a scope through, and the
// per-rule grant memory that existed only to serve that retention is gone with it.
//
// Bootstrap cannot publish status (it runs before controllers start), so a rule denied there is
// simply not compiled and the first reconcile writes the terminal condition. That ordering — fail
// closed first, explain second — is correct, not a limitation.
func CompileWatchRule(
	ctx context.Context,
	reader client.Reader,
	store *rulestore.RuleStore,
	rule configv1alpha3.WatchRule,
	target configv1alpha3.GitTarget,
) (authz.ResolvedSourceScope, error) {
	key := k8stypes.NamespacedName{Name: rule.Name, Namespace: rule.Namespace}

	if err := types.ValidateObjectSelectors(rule.Spec.ObjectSelectors()); err != nil {
		store.Delete(key)
		return authz.ResolvedSourceScope{}, &ObjectSelectorError{Message: err.Error()}
	}

	resolved, err := authz.ResolveWatchRuleSourceScope(ctx, reader, &rule, &target)
	if err != nil {
		// Transient: leave whatever is compiled alone and let the caller requeue. Tearing down a
		// running stream because the apiserver blipped is the failure this avoids.
		return resolved, err
	}

	if resolved.Admitted() {
		store.AddOrUpdateWatchRule(
			rule,
			itemNamespaces(resolved),
			target.Name, target.Namespace,
			target.GitBranchKey().Name, target.GitBranchKey().Namespace,
			target.Spec.Path,
		)
		return resolved, nil
	}

	// Stop the data plane before the caller says anything about it.
	store.Delete(key)
	return resolved, nil
}

// itemNamespaces projects the resolved scope into the per-item slice the store compiles from.
func itemNamespaces(resolved authz.ResolvedSourceScope) [][]string {
	out := make([][]string, 0, len(resolved.Items))
	for _, item := range resolved.Items {
		out = append(out, item.Namespaces)
	}
	return out
}

// CompileClusterWatchRule is THE ONLY PATH from a ClusterWatchRule to a compiled cluster rule.
//
// Two refusals, both terminal, in this order:
//
//  1. the referenced GitTarget's namespace must be admitted by that target's KubeProvider — a
//     ClusterWatchRule's gitTargetRef carries a namespace, so it can name a target in ANY namespace
//     and widen that target's mirror scope cluster-wide;
//  2. every type the rule names must be in the clusterResources the grants that admit the target
//     give it (clusterResourcesRefusal). One forbidden type refuses the whole rule.
//
// selected is what the planner selects for an item in the GitTarget's source cluster (see
// Manager.ClusterResourcesSelectedFor); nil, or a cluster not discovered yet, selects nothing. The
// compiled rule carries the granted clusterResources too, so the planner never watches a type the
// grant does not name, also one that discovery serves only after this compile. The rule reconciles
// again on its steady interval, and is refused then.
//
// Like CompileWatchRule it is shared by the reconciler and the startup bootstrap, so a restart
// cannot open an unauthorized watch before the first reconcile can publish status.
func CompileClusterWatchRule(
	ctx context.Context,
	reader client.Reader,
	store *rulestore.RuleStore,
	rule configv1alpha3.ClusterWatchRule,
	target configv1alpha3.GitTarget,
	selected ClusterResourceSelection,
) (ClusterWatchRuleDecision, error) {
	key := k8stypes.NamespacedName{Name: rule.Name}

	if err := types.ValidateObjectSelectors(rule.Spec.ObjectSelectors()); err != nil {
		store.DeleteClusterWatchRule(key)
		return ClusterWatchRuleDecision{}, &ObjectSelectorError{Message: err.Error()}
	}

	admitted, scope, err := authz.GitTargetSourceScope(ctx, reader, &target)
	if err != nil {
		// Transient: leave whatever is compiled alone and let the caller requeue.
		return ClusterWatchRuleDecision{}, err
	}
	if !admitted.Allowed {
		store.DeleteClusterWatchRule(key)
		return ClusterWatchRuleDecision{
			Reason: ClusterWatchRuleReasonGitTargetNamespaceNotAuthorized,
			Message: fmt.Sprintf("ClusterWatchRule may not compile against GitTarget '%s/%s': %s",
				target.Namespace, target.Name, admitted.Message),
		}, nil
	}
	if forbidden := clusterResourcesRefusal(&rule, scope, selected); len(forbidden) > 0 {
		store.DeleteClusterWatchRule(key)
		return ClusterWatchRuleDecision{
			ResourcesRefused: true,
			Reason:           ClusterWatchRuleReasonClusterResourceNotAllowed,
			Message: fmt.Sprintf(
				"KubeProvider %s lets namespace %q mirror cluster-scoped resources %s "+
					"(spec.allowedGitTargets[].clusterResources); %s not allowed, so the ClusterWatchRule is refused",
				target.KubeProviderKey(), target.Namespace, describeIdentities(scope.ClusterResources),
				strings.Join(forbidden, "; ")),
		}, nil
	}

	store.AddOrUpdateClusterWatchRule(
		rule,
		scope.ClusterResources,
		target.Name, target.Namespace,
		target.GitBranchKey().Name, target.GitBranchKey().Namespace,
		target.Spec.Path,
	)
	return ClusterWatchRuleDecision{Admitted: true}, nil
}

// ClusterWatchRule compile refusal reasons. They live here rather than in the controller because
// bootstrap refuses on the same grounds without a controller in sight, and one vocabulary is what
// keeps the two from drifting.
const (
	// ClusterWatchRuleReasonClusterResourceNotAllowed is the terminal ResourcesResolved reason when
	// the rule names a cluster-scoped type outside the clusterResources the KubeProvider's grants give
	// the GitTarget's namespace, or "*" in apiGroups or resources without clusterResources: ["*"].
	ClusterWatchRuleReasonClusterResourceNotAllowed = "ClusterResourceNotAllowed"

	// ClusterWatchRuleReasonGitTargetNamespaceNotAuthorized is the terminal reason when the
	// referenced GitTarget's namespace is not admitted by that target's KubeProvider — either
	// because no grant in its spec.allowedGitTargets admits it or because the provider does not
	// exist at all.
	//
	// One rule-side reason covers both provider-side causes on purpose: from the ClusterWatchRule's
	// point of view the single fact that matters is that this rule may not compile against this
	// target. The Message carries which of the two it was.
	ClusterWatchRuleReasonGitTargetNamespaceNotAuthorized = "GitTargetNamespaceNotAuthorized"
)

// ClusterWatchRuleDecision is the outcome of the shared ClusterWatchRule compile path.
type ClusterWatchRuleDecision struct {
	// Admitted reports whether the rule compiled.
	Admitted bool
	// ResourcesRefused reports a refusal of the rule's own resources rather than of its GitTarget,
	// which lands on ResourcesResolved.
	ResourcesRefused bool
	// Reason is the terminal condition reason when it did not.
	Reason string
	// Message explains the refusal to an operator.
	Message string
}

// ReasonInvalidObjectSelector is the terminal ResourcesResolved reason for a rule whose
// spec.rules[].objectSelector does not parse. The rule is refused as a whole, for the reason a
// denied source namespace refuses one: mirroring the items that did parse is worse than a loud
// failure.
const ReasonInvalidObjectSelector = "InvalidObjectSelector"

// ObjectSelectorError is the compile refusal for an invalid spec.rules[].objectSelector. It is an
// error rather than a decision because the compile functions already return one for a transient
// failure, and the callers must tell the two apart: this one is terminal, and the compiled rule is
// already out of the store when it is returned.
type ObjectSelectorError struct {
	// Message names the offending item and why its selector is invalid.
	Message string
}

func (e *ObjectSelectorError) Error() string { return e.Message }

// ClusterResourceSelection returns the cluster-scoped types the watch planner selects for a
// ClusterWatchRule item now.
type ClusterResourceSelection func(item configv1alpha3.ClusterResourceRule) []schema.GroupResource

// clusterResourcesRefusal names what a ClusterWatchRule asks for outside scope's clusterResources. An
// item with "*" in apiGroups or resources needs clusterResources: ["*"], decided on the rule's TEXT as
// rules[].sourceNamespace: "*" is. Any other item asks for every pair its apiGroups and resources name,
// and for every type the planner selects for it, which is how an item that omits apiGroups is
// checked: by the planner's own matching, versions and ambiguity rule included. Under ["*"] nothing
// is refused.
func clusterResourcesRefusal(
	rule *configv1alpha3.ClusterWatchRule, scope configv1alpha3.SourceScope, selected ClusterResourceSelection,
) []string {
	if scope.AllowsAllClusterResources() {
		return nil
	}
	var forbidden []string
	for i := range rule.Spec.Rules {
		item := rule.Spec.Rules[i]
		if slices.Contains(item.APIGroups, "*") || slices.Contains(item.Resources, "*") {
			forbidden = append(forbidden, fmt.Sprintf(
				"spec.rules[%d] has \"*\" in apiGroups or resources, which needs clusterResources: [\"*\"]", i))
			continue
		}
		var asked []schema.GroupResource
		for _, resource := range item.Resources {
			for _, group := range item.APIGroups {
				asked = append(asked, schema.GroupResource{Group: group, Resource: normalizeResource(resource)})
			}
		}
		if selected != nil {
			asked = append(asked, selected(item)...)
		}
		var types []string
		for _, gr := range asked {
			identity := configv1alpha3.ClusterResourceIdentityFor(gr.Group, gr.Resource)
			if !scope.AllowsClusterResource(gr.Group, gr.Resource) && !slices.Contains(types, identity) {
				types = append(types, identity)
			}
		}
		if len(types) > 0 {
			forbidden = append(
				forbidden,
				fmt.Sprintf("spec.rules[%d] asks for %s, which is", i, describeIdentities(types)),
			)
		}
	}
	return forbidden
}

// describeIdentities renders cluster-resource identities for a message.
func describeIdentities(identities []string) string {
	if len(identities) == 0 {
		return "none"
	}
	quoted := make([]string, 0, len(identities))
	for _, identity := range identities {
		quoted = append(quoted, strconv.Quote(identity))
	}
	return strings.Join(quoted, ", ")
}
