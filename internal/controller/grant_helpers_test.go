// SPDX-License-Identifier: Apache-2.0

package controller

import (
	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// grantNames converts namespace names into a grant's names.
func grantNames(namespaces ...string) []configbutleraiv1alpha3.GrantNamespace {
	names := make([]configbutleraiv1alpha3.GrantNamespace, 0, len(namespaces))
	for _, ns := range namespaces {
		names = append(names, configbutleraiv1alpha3.GrantNamespace(ns))
	}
	return names
}

// mirrorGrants admits the named namespaces to mirror their own namespace and every cluster-scoped
// resource, the reach a provider admitting them had before grants were scoped.
func mirrorGrants(namespaces ...string) []configbutleraiv1alpha3.KubeProviderGitTargetGrant {
	return []configbutleraiv1alpha3.KubeProviderGitTargetGrant{
		{
			ConsumerNamespaces: configbutleraiv1alpha3.ConsumerNamespaces{
				From:  configbutleraiv1alpha3.GrantFromNames,
				Names: grantNames(namespaces...),
			},
			SourceNamespaces: []configbutleraiv1alpha3.ScopeNamespace{configbutleraiv1alpha3.ConsumerNamespaceVariable},
			ClusterResources: []configbutleraiv1alpha3.ClusterResourceIdentity{"*"},
		},
	}
}

// applierGrants admits the named namespaces to hold appliers in scope.
func applierGrants(
	scope []configbutleraiv1alpha3.ScopeNamespace, namespaces ...string,
) []configbutleraiv1alpha3.KubeProviderApplierClaimGrant {
	return []configbutleraiv1alpha3.KubeProviderApplierClaimGrant{
		{
			ConsumerNamespaces: configbutleraiv1alpha3.ConsumerNamespaces{
				From:  configbutleraiv1alpha3.GrantFromNames,
				Names: grantNames(namespaces...),
			},
			ApplierNamespaces: scope,
		},
	}
}

// branchesFromSame is the allowedGitBranches of a GitProvider that lets GitBranches in its own namespace
// claim the branches patterns match.
func branchesFromSame(
	patterns ...configbutleraiv1alpha3.BranchPattern,
) []configbutleraiv1alpha3.GitProviderGitBranchGrant {
	return []configbutleraiv1alpha3.GitProviderGitBranchGrant{
		{
			ConsumerNamespaces: configbutleraiv1alpha3.ConsumerNamespaces{From: configbutleraiv1alpha3.GrantFromSame},
			BranchPatterns:     patterns,
		},
	}
}

// writersFromSame is the allowedGitTargets of a GitBranch shared with its own namespace only.
func writersFromSame() []configbutleraiv1alpha3.GitBranchGitTargetGrant {
	return []configbutleraiv1alpha3.GitBranchGitTargetGrant{
		{ConsumerNamespaces: configbutleraiv1alpha3.ConsumerNamespaces{From: configbutleraiv1alpha3.GrantFromSame}},
	}
}
