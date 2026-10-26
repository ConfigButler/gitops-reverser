// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"maps"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The grant grammar is checked by the API server, so a refusal names the mistake at apply time. Each
// case is sent as unstructured, because a typed object drops the empty lists and the empty
// selector these rules exist to refuse.
var _ = Describe("Access grants at admission", func() {
	var counter int
	object := func(kind string, spec map[string]any) *unstructured.Unstructured {
		counter++
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "configbutler.ai/v1alpha3",
			"kind":       kind,
			"metadata":   map[string]any{"name": fmt.Sprintf("grant-cel-%d", counter), "namespace": "default"},
			"spec":       spec,
		}}
		return u
	}
	gitProvider := func(grants ...any) *unstructured.Unstructured {
		return object("GitProvider", map[string]any{
			"url": "https://example.com/repo.git", "allowedGitBranches": grants,
		})
	}
	gitBranch := func(grants ...any) *unstructured.Unstructured {
		return object("GitBranch", map[string]any{
			"gitProviderRef": map[string]any{"name": "p"}, "branch": "main", "allowedGitTargets": grants,
		})
	}
	kubeProvider := func(field string, grants ...any) *unstructured.Unstructured {
		return object("KubeProvider", map[string]any{
			"secretRef": map[string]any{"name": "kubeconfig"}, field: grants,
		})
	}
	create := func(obj client.Object) error {
		err := k8sClient.Create(context.Background(), obj)
		if err == nil {
			DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), obj) })
		}
		return err
	}
	refused := func(obj client.Object, message string) {
		GinkgoHelper()
		err := create(obj)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(message))
	}
	accepted := func(obj client.Object) {
		GinkgoHelper()
		Expect(create(obj)).To(Succeed())
	}
	same := map[string]any{"from": "Same"}
	branches := func(patterns ...any) map[string]any {
		return map[string]any{"consumerNamespaces": same, "branchPatterns": patterns}
	}
	// consumers is a grant holding only its consumerNamespaces block, the whole of a GitBranch grant.
	consumers := func(block map[string]any) map[string]any {
		return map[string]any{"consumerNamespaces": block}
	}
	// withConsumers is scope plus a consumerNamespaces block.
	withConsumers := func(block map[string]any, scope map[string]any) map[string]any {
		grant := maps.Clone(scope)
		grant["consumerNamespaces"] = block
		return grant
	}

	It("accepts the branch patterns the grammar allows", func() {
		accepted(gitProvider(branches("main", "release/*", "teams/{consumer.namespace}/*", "{consumer.namespace}",
			"teams/{consumer.namespace}/{consumer.name}", "{consumer.namespace}/{consumer.name}/*", "*",
			"feature/*/v2")))
	})

	It("refuses {consumer.name} without {consumer.namespace} in the same pattern", func() {
		refused(gitProvider(branches("env/{consumer.name}")), "{consumer.name} needs {consumer.namespace}")
		refused(gitProvider(branches("{consumer.name}/*")), "{consumer.name} needs {consumer.namespace}")
	})

	It("refuses a consumer variable that is not a whole path segment", func() {
		refused(gitProvider(branches("teams/{consumer.namespace}*")), "must each be a whole path segment")
		refused(gitProvider(branches("teams/x{consumer.namespace}/live")), "must each be a whole path segment")
		refused(gitProvider(branches("teams/{consumer.namespace}-{consumer.name}")),
			"must each be a whole path segment")
		refused(gitProvider(branches("{consumer.namespace}{consumer.name}")), "must each be a whole path segment")
	})

	It("refuses an unknown variable, naming it and the ones that exist", func() {
		refused(gitProvider(branches("teams/{namespace}/*")),
			"branch pattern teams/{namespace}/* references unknown variable {namespace}; "+
				"the variables are {consumer.namespace} and {consumer.name}")
		refused(gitProvider(branches("teams/{consumer.namespace")), "unmatched brace")
	})

	It("refuses what no branch name can hold", func() {
		refused(gitProvider(branches("feature/?")), "spec.allowedGitBranches[0].branchPatterns[0]")
		refused(gitProvider(branches("feature/[ab]")), "spec.allowedGitBranches[0].branchPatterns[0]")
		refused(gitProvider(branches("a..b")), "can match no branch name")
	})

	It("refuses an empty scope or names list", func() {
		refused(gitProvider(branches()), "spec.allowedGitBranches[0].branchPatterns")
		refused(gitProvider(consumers(same)), "branchPatterns: Required value")
		refused(gitBranch(consumers(map[string]any{"from": "Names", "names": []any{}})), "names")
		refused(kubeProvider("allowedGitTargets", withConsumers(same, map[string]any{"sourceNamespaces": []any{}})),
			"sourceNamespaces")
		refused(kubeProvider("allowedGitTargets", withConsumers(same, map[string]any{"clusterResources": []any{}})),
			"clusterResources")
		refused(kubeProvider("allowedApplierClaims", withConsumers(same, map[string]any{"applierNamespaces": []any{}})),
			"applierNamespaces")
		refused(kubeProvider("allowedApplierClaims", consumers(same)), "applierNamespaces: Required value")
	})

	It("takes a GitTarget's RemoteSaveRequest grants by source namespace name or *, and nothing else", func() {
		gitTarget := func(grants ...any) *unstructured.Unstructured {
			return object("GitTarget", map[string]any{
				"gitBranchRef": map[string]any{"name": "b"}, "path": "apps",
				"kubeProviderRef": map[string]any{"name": "k"}, "allowedRemoteSaveRequests": grants,
			})
		}
		from := func(namespaces ...any) map[string]any { return map[string]any{"sourceNamespaces": namespaces} }
		accepted(gitTarget(from("shop", "billing")))
		accepted(gitTarget(from("*")))
		labelled := from("shop")
		labelled["requestSelector"] = map[string]any{"matchLabels": map[string]any{"app": "cart"}}
		accepted(gitTarget(labelled))
		refused(gitTarget(from("{consumer.namespace}")), "variables are refused")
		refused(gitTarget(from("Shop")), "a source namespace is a namespace name")
		refused(gitTarget(from()), "sourceNamespaces")
		refused(gitTarget(map[string]any{}), "sourceNamespaces: Required value")
	})

	// consumerNamespaces is required on every grant type and from is required inside it, with no default
	// for either, so a grant that leaves the "who" out admits nobody instead of its holder's namespace.
	// The block's own rules cannot make it required, so each list is checked here.
	for _, list := range []struct {
		name  string
		build func(grant map[string]any) *unstructured.Unstructured
		scope map[string]any
	}{
		{"GitProvider allowedGitBranches",
			func(g map[string]any) *unstructured.Unstructured { return gitProvider(g) },
			map[string]any{"branchPatterns": []any{"main"}}},
		{"GitBranch allowedGitTargets",
			func(g map[string]any) *unstructured.Unstructured { return gitBranch(g) },
			map[string]any{}},
		{"KubeProvider allowedGitTargets",
			func(g map[string]any) *unstructured.Unstructured { return kubeProvider("allowedGitTargets", g) },
			map[string]any{"sourceNamespaces": []any{"{consumer.namespace}"}}},
		{"KubeProvider allowedApplierClaims",
			func(g map[string]any) *unstructured.Unstructured { return kubeProvider("allowedApplierClaims", g) },
			map[string]any{"applierNamespaces": []any{"flux-system"}}},
	} {
		It("refuses a "+list.name+" grant without a complete consumerNamespaces block", func() {
			refused(list.build(maps.Clone(list.scope)), "consumerNamespaces: Required value")
			nullBlock := maps.Clone(list.scope)
			nullBlock["consumerNamespaces"] = nil
			refused(list.build(nullBlock), "consumerNamespaces: Required value")
			refused(list.build(withConsumers(map[string]any{}, list.scope)), "from: Required value")
			refused(list.build(withConsumers(map[string]any{"names": []any{"team-a"}}, list.scope)),
				"from: Required value")
			accepted(list.build(withConsumers(same, list.scope)))
		})
	}

	It("refuses a grant without from, or with names or a selector beside the wrong from", func() {
		refused(gitBranch(consumers(map[string]any{})), "from: Required value")
		refused(gitBranch(consumers(map[string]any{"from": "Everyone"})), "Unsupported value")
		refused(gitBranch(consumers(map[string]any{"from": "Names"})), "from: Names needs names")
		refused(gitBranch(consumers(map[string]any{"from": "Same", "names": []any{"team-b"}})),
			"names goes only with from: Names")
		refused(gitBranch(consumers(map[string]any{"from": "All",
			"selector": map[string]any{"matchLabels": map[string]any{"a": "b"}}})),
			"selector goes only with from: Selector")
	})

	It("refuses a KubeProvider GitTarget grant that grants nothing", func() {
		refused(kubeProvider("allowedGitTargets", consumers(same)),
			"sets sourceNamespaces, clusterResources, or both")
		accepted(kubeProvider("allowedGitTargets",
			withConsumers(same, map[string]any{"sourceNamespaces": []any{"{consumer.namespace}"}})))
		accepted(kubeProvider("allowedGitTargets", withConsumers(same,
			map[string]any{"clusterResources": []any{"clusterroles.rbac.authorization.k8s.io", "nodes", "*"}})))
	})

	It("refuses a variable in names, pointing to from: All and Same, and * pointing to from: All", func() {
		names := func(name string) map[string]any {
			return consumers(map[string]any{"from": "Names", "names": []any{name}})
		}
		refused(gitBranch(names("{consumer.namespace}")), "use from: All")
		refused(gitBranch(names("{consumer.name}")), "or from: Same")
		refused(gitBranch(names("*")), "use from: All")
		refused(gitBranch(names("Team_A")), "namespace names")
	})

	It("refuses a Selector grant without a selector or with an empty one, and accepts a real one", func() {
		refused(gitBranch(consumers(map[string]any{"from": "Selector"})), "from: Selector needs a selector")
		refused(gitBranch(consumers(map[string]any{"from": "Selector", "selector": map[string]any{}})),
			"selector: {} is refused; to admit every namespace, use from: All")
		refused(gitBranch(consumers(map[string]any{"from": "Selector",
			"selector": map[string]any{"matchLabels": map[string]any{}}})), "selector: {} is refused")
		accepted(gitBranch(consumers(map[string]any{"from": "Selector",
			"selector": map[string]any{"matchLabels": map[string]any{"tenant": "true"}}})))
		accepted(gitBranch(consumers(map[string]any{"from": "Selector", "selector": map[string]any{
			"matchExpressions": []any{map[string]any{"key": "tenant", "operator": "Exists"}}}})))
		accepted(gitBranch(consumers(map[string]any{"from": "All"}), consumers(same)))
	})

	It("refuses a scope entry that is neither a name, a consumer variable alone, nor *", func() {
		alone := "'{consumer.namespace}' alone, or '*' alone"
		sources := func(entry string) map[string]any {
			return withConsumers(same, map[string]any{"sourceNamespaces": []any{entry}})
		}
		appliers := func(entry string) map[string]any {
			return withConsumers(same, map[string]any{"applierNamespaces": []any{entry}})
		}
		refused(kubeProvider("allowedGitTargets", sources("team-{consumer.namespace}")), alone)
		refused(kubeProvider("allowedGitTargets", sources("{namespace}")), alone)
		refused(kubeProvider("allowedApplierClaims", appliers("flux-*")), alone)
		refused(kubeProvider("allowedGitTargets", sources("{consumer.name}")), "the consumer picks its own name")
		refused(kubeProvider("allowedApplierClaims", appliers("{consumer.name}")), "the consumer picks its own name")
		refused(kubeProvider("allowedGitTargets", withConsumers(same,
			map[string]any{"clusterResources": []any{"cluster*"}})), "clusterResources[0]")
	})
})
