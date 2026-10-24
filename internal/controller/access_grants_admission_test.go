// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

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
	branches := func(patterns ...any) map[string]any {
		return map[string]any{"from": "Same", "branches": patterns}
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
		refused(gitProvider(branches("feature/?")), "spec.allowedGitBranches[0].branches[0]")
		refused(gitProvider(branches("feature/[ab]")), "spec.allowedGitBranches[0].branches[0]")
		refused(gitProvider(branches("a..b")), "can match no branch name")
	})

	It("refuses an empty scope or names list", func() {
		refused(gitProvider(branches()), "spec.allowedGitBranches[0].branches")
		refused(gitProvider(map[string]any{"from": "Same"}), "branches: Required value")
		refused(gitBranch(map[string]any{"from": "Names", "names": []any{}}), "names")
		refused(kubeProvider("allowedGitTargets", map[string]any{"from": "Same", "sourceNamespaces": []any{}}),
			"sourceNamespaces")
		refused(kubeProvider("allowedGitTargets", map[string]any{"from": "Same", "clusterResources": []any{}}),
			"clusterResources")
		refused(kubeProvider("allowedApplierClaims", map[string]any{"from": "Same", "applierNamespaces": []any{}}),
			"applierNamespaces")
		refused(kubeProvider("allowedApplierClaims", map[string]any{"from": "Same"}),
			"applierNamespaces: Required value")
	})

	It("refuses a grant without from, or with names or a selector beside the wrong from", func() {
		refused(gitBranch(map[string]any{}), "from: Required value")
		refused(gitBranch(map[string]any{"from": "Everyone"}), "Unsupported value")
		refused(gitBranch(map[string]any{"from": "Names"}), "from: Names needs names")
		refused(gitBranch(map[string]any{"from": "Same", "names": []any{"team-b"}}),
			"names goes only with from: Names")
		refused(gitBranch(map[string]any{"from": "All",
			"selector": map[string]any{"matchLabels": map[string]any{"a": "b"}}}),
			"selector goes only with from: Selector")
	})

	It("refuses a KubeProvider source grant that grants nothing", func() {
		refused(kubeProvider("allowedGitTargets", map[string]any{"from": "Same"}),
			"sets sourceNamespaces, clusterResources, or both")
		accepted(
			kubeProvider("allowedGitTargets", map[string]any{"from": "Same",
				"sourceNamespaces": []any{"{consumer.namespace}"}}),
		)
		accepted(kubeProvider("allowedGitTargets", map[string]any{"from": "Same",
			"clusterResources": []any{"clusterroles.rbac.authorization.k8s.io", "nodes", "*"}}))
	})

	It("refuses a variable in names, pointing to from: All and Same, and * pointing to from: All", func() {
		refused(gitBranch(map[string]any{"from": "Names", "names": []any{"{consumer.namespace}"}}), "use from: All")
		refused(gitBranch(map[string]any{"from": "Names", "names": []any{"{consumer.name}"}}), "or from: Same")
		refused(gitBranch(map[string]any{"from": "Names", "names": []any{"*"}}), "use from: All")
		refused(gitBranch(map[string]any{"from": "Names", "names": []any{"Team_A"}}), "namespace names")
	})

	It("refuses a Selector grant without a selector or with an empty one, and accepts a real one", func() {
		refused(gitBranch(map[string]any{"from": "Selector"}), "from: Selector needs a selector")
		refused(gitBranch(map[string]any{"from": "Selector", "selector": map[string]any{}}),
			"selector: {} is refused; to admit every namespace, use from: All")
		refused(gitBranch(map[string]any{"from": "Selector",
			"selector": map[string]any{"matchLabels": map[string]any{}}}), "selector: {} is refused")
		accepted(gitBranch(map[string]any{"from": "Selector",
			"selector": map[string]any{"matchLabels": map[string]any{"tenant": "true"}}}))
		accepted(gitBranch(map[string]any{"from": "Selector", "selector": map[string]any{
			"matchExpressions": []any{map[string]any{"key": "tenant", "operator": "Exists"}}}}))
		accepted(gitBranch(map[string]any{"from": "All"}, map[string]any{"from": "Same"}))
	})

	It("refuses a scope entry that is neither a name, a consumer variable alone, nor *", func() {
		alone := "'{consumer.namespace}' alone, or '*' alone"
		refused(kubeProvider("allowedGitTargets", map[string]any{"from": "Same",
			"sourceNamespaces": []any{"team-{consumer.namespace}"}}), alone)
		refused(kubeProvider("allowedGitTargets", map[string]any{"from": "Same",
			"sourceNamespaces": []any{"{namespace}"}}), alone)
		refused(kubeProvider("allowedApplierClaims", map[string]any{"from": "Same",
			"applierNamespaces": []any{"flux-*"}}), alone)
		refused(kubeProvider("allowedGitTargets", map[string]any{"from": "Same",
			"sourceNamespaces": []any{"{consumer.name}"}}), "the consumer picks its own name")
		refused(kubeProvider("allowedApplierClaims", map[string]any{"from": "Same",
			"applierNamespaces": []any{"{consumer.name}"}}), "the consumer picks its own name")
		refused(kubeProvider("allowedGitTargets", map[string]any{"from": "Same",
			"clusterResources": []any{"cluster*"}}), "clusterResources[0]")
	})
})
