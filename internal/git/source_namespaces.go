// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"fmt"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// resolveSourceNamespaces returns the source namespaces that reach a GitTarget, as
// docs/layout/model.md defines the set: the target's OWN namespace, plus the explicit
// rules[].sourceNamespace of every WatchRule pointing at it. It reads the config cluster and
// nothing else — no scan, no repository state.
//
// Three things it deliberately is not:
//
//   - It is not spec.allowedSourceNamespaces. That field is an authorization fence — who MAY write
//     here — and this is a question about what the folder means. They are computed from different
//     inputs and the wave that deletes the field leaves this untouched.
//   - It does not read ClusterWatchRules. Those watch cluster-scoped resources, which have no
//     namespace to contribute.
//   - It does not enumerate a wildcard. A rules[] item naming "*" makes the set unknowable from the
//     spec, so it is reported as such (wildcard=true) and the caller refuses rather than expands:
//     that holds under both readings of "*", which is all any caller of this needs.
//
// A rule that cannot be listed is an error, never an empty set: silently reading "one namespace"
// off a failed List is how a fence becomes a no-op.
func resolveSourceNamespaces(
	ctx context.Context,
	c client.Client,
	target *v1alpha3.GitTarget,
) ([]string, bool, error) {
	var rules v1alpha3.WatchRuleList
	if err := c.List(ctx, &rules, client.InNamespace(target.Namespace)); err != nil {
		return nil, false, fmt.Errorf("failed to list WatchRules for GitTarget %s/%s: %w",
			target.Namespace, target.Name, err)
	}

	wildcard := false
	seen := map[string]struct{}{target.Namespace: {}}
	for i := range rules.Items {
		rule := &rules.Items[i]
		if rule.Spec.TargetRef.Name != target.Name {
			continue
		}
		for _, item := range rule.Spec.Rules {
			if item.IsSourceNamespaceWildcard() {
				wildcard = true
				continue
			}
			seen[item.EffectiveSourceNamespace(rule.Namespace)] = struct{}{}
		}
	}

	namespaces := make([]string, 0, len(seen))
	for ns := range seen {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)
	return namespaces, wildcard, nil
}
