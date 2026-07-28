// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Aggregated deletecollection attribution is the case the deleted response-body expander
// gave up on entirely, driven against a real aggregated API server rather than a fixture.
//
// The kube-apiserver PROXIES a request for an aggregated resource to the extension server. It
// audits the request it proxied, but it never decodes the response it streamed back, so
// responseObject is empty — row 15 of the lab corpus established exactly that for an
// aggregated-API create (test/mutationlab/README.md), and a collection delete travels the same
// proxy path. The expander needed that body to reconstruct one fact per object, so for wardle it
// produced nothing and every removal shipped committer-authored.
//
// The collection fact does not need it. The audit event still carries what the actor asked for —
// the type, the namespace, the selector on the request URI, and the stage timestamp — and a removal
// joins that by SCOPE. This spec asserts the outcome that follows: every removal commit is authored
// by the actor who ran the collection delete.
//
// It deliberately asserts the AUTHOR rather than the metric tier. Which tier fires is a fact about
// the API server's proxy behaviour, not about this operator: if some future apiserver did return a
// body, the join would silently upgrade to uid membership and this spec should still pass. The tier
// that actually fired is reported, not asserted, so a change in that behaviour is visible without
// being a failure.
//
// Not Serial: the wardle APIService is installed once at cluster setup and only read here; the
// collection delete is scoped by a per-run label selector, so concurrent specs cannot be caught by
// it. See docs/spec/e2e-serial-registry.md.
var _ = Describe("Aggregated API deletecollection attribution", Label("aggregated-api"), Ordered, func() {
	var (
		testNs        string
		repo          *RepoArtifacts
		providerName  string
		targetName    string
		watchRuleName string
		basePath      string
		alice         dynamic.Interface
	)

	BeforeAll(func() {
		if configuredAuthorModeEnabled() {
			Skip("watch-first configured-author mode has no audit facts for delete attribution")
		}

		testNs = testNamespaceFor("agg-dc")
		providerName = "agg-dc-provider"
		targetName = "agg-dc-target"
		watchRuleName = "agg-dc-watchrule"
		basePath = "e2e/aggregated-deletecollection"

		_, _ = kubectlRun("create", "namespace", testNs)

		repo = SetupRepo(
			resolveE2EContext(),
			testNs,
			fmt.Sprintf("e2e-agg-dc-%d", GinkgoRandomSeed()),
		)
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to the aggregated-dc namespace")
		applySOPSAgeKeyToNamespace(testNs)

		By("setting up GitProvider (0s commit window), GitTarget and a flunder WatchRule")
		createReadyGitProvider(providerName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)
		createValidatedGitTarget(targetName, testNs, providerName, basePath)
		Expect(applyFromTemplate(
			"test/e2e/templates/aggregated-api/watchrule-flunder.tmpl",
			struct {
				Name          string
				Namespace     string
				GitTargetName string
			}{Name: watchRuleName, Namespace: testNs, GitTargetName: targetName},
			testNs,
		)).To(Succeed())
		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Succeeded", "")
		waitForStreamsRunning(targetName, testNs)

		By("building an impersonated dynamic client for the actor")
		alice, err = impersonatedDynamicClient("oidc-alice", "Alice Liddell", "alice@configbutler.ai")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		cleanupPipeline(testNs, providerName, targetName, watchRuleName)
		cleanupNamespace(testNs)
	})

	It("attributes a body-less aggregated collection delete to the actor", func() {
		label := fmt.Sprintf("aggdc-%d", GinkgoRandomSeed())
		doomedA := label + "-doomed-a"
		doomedB := label + "-doomed-b"
		survivor := label + "-survivor"

		flunderPath := func(name string) string {
			return path.Join(basePath, fmt.Sprintf("%s/wardle.example.com/flunders/%s.yaml", testNs, name))
		}

		By("creating two flunders the collection will cover and one it will not")
		for name, tier := range map[string]string{doomedA: "doomed", doomedB: "doomed", survivor: "keep"} {
			Expect(createLabeledFlunder(alice, testNs, name, label, tier)).To(Succeed())
		}
		for _, name := range []string{doomedA, doomedB, survivor} {
			waitForFilePresent(repo, flunderPath(name))
		}

		before := collectionResolutionCounts()

		By("deleting only the doomed subset as the actor, through the aggregation layer")
		Expect(deleteFlunderCollection(alice, testNs, "aggdc="+label+",tier=doomed")).To(Succeed())

		By("asserting both removals are authored by the actor, not the committer")
		waitForFileDeletedByActor(repo, flunderPath(doomedA))
		waitForFileDeletedByActor(repo, flunderPath(doomedB))

		By("asserting the flunder outside the selector survives untouched")
		Consistently(func(g Gomega) {
			pullLatestRepoState(g, repo.CheckoutDir)
			_, statErr := os.Stat(filepath.Join(repo.CheckoutDir, flunderPath(survivor)))
			g.Expect(statErr).NotTo(HaveOccurred(),
				"a flunder the selector did not match must not be removed")
		}, 8*time.Second, 2*time.Second).Should(Succeed())

		reportCollectionTier(before)
	})
})

// collectionResolutionCounts snapshots the two collection tiers so the spec can report which one
// the aggregated collection delete actually took.
func collectionResolutionCounts() map[string]float64 {
	// Without this the queries below fail on a nil client and the tier goes unreported, which is a
	// silent hole rather than a failure: the spec asserts the author, so it passes either way.
	ensurePrometheusClient()
	counts := map[string]float64{}
	for _, tier := range []string{"collection_uid", "collection_scope"} {
		n, err := queryPrometheus(fmt.Sprintf(
			`sum(max_over_time(gitopsreverser_attribution_resolutions_total{result=%q}[2h])) or vector(0)`, tier))
		if err != nil {
			return map[string]float64{}
		}
		counts[tier] = n
	}
	return counts
}

// reportCollectionTier says which collection tier resolved this spec's removals. It reports rather
// than asserts: whether the API server sends a response body for a proxied collection delete is a
// fact about the API server, and the join is correct either way.
func reportCollectionTier(before map[string]float64) {
	if len(before) == 0 {
		return
	}
	after := collectionResolutionCounts()
	uid := after["collection_uid"] - before["collection_uid"]
	scope := after["collection_scope"] - before["collection_scope"]
	switch {
	case scope > 0 && uid == 0:
		_, _ = fmt.Fprintf(GinkgoWriter,
			"\nℹ️  aggregated deletecollection resolved by SCOPE (+%.0f collection_scope): the proxied "+
				"request carried no response body, which is exactly the case the deleted expander "+
				"produced nothing for.\n", scope)
	case uid > 0:
		_, _ = fmt.Fprintf(GinkgoWriter,
			"\nℹ️  aggregated deletecollection resolved by UID membership (+%.0f collection_uid, "+
				"+%.0f collection_scope): this API server DID return the deleted set for a proxied "+
				"collection delete.\n", uid, scope)
	default:
		_, _ = fmt.Fprintf(GinkgoWriter,
			"\nℹ️  aggregated deletecollection resolved through a stronger per-object tier: neither "+
				"collection tier moved, so each removal found its own fact first.\n")
	}
}

// flunderGVR is the aggregated type this spec mirrors.
var flunderGVR = schema.GroupVersionResource{
	Group:    "wardle.example.com",
	Version:  "v1alpha1",
	Resource: "flunders",
}

// impersonatedDynamicClient builds a dynamic client that impersonates asUser with OIDC
// display-name/email claims, so writes it makes are attributed to "<displayName> <email>" in Git.
// It is the aggregated-type sibling of impersonatedConfigMapClient, which is typed and therefore
// cannot reach wardle.
func impersonatedDynamicClient(asUser, displayName, email string) (dynamic.Interface, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	if ctx := kubectlContext(); ctx != "" {
		overrides.CurrentContext = ctx
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	config.Impersonate = rest.ImpersonationConfig{
		UserName: asUser,
		Groups:   []string{"system:masters"},
		Extra: map[string][]string{
			"configbutler.ai/claims/display-name": {displayName},
			"configbutler.ai/claims/email":        {email},
		},
	}
	return dynamic.NewForConfig(config)
}

// createLabeledFlunder creates one flunder carrying the per-run aggdc label and a tier label the
// collection delete selects on.
func createLabeledFlunder(client dynamic.Interface, ns, name, aggdc, tier string) error {
	flunder := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "wardle.example.com/v1alpha1",
		"kind":       "Flunder",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
			"labels":    map[string]any{"aggdc": aggdc, "tier": tier},
		},
		"spec": map[string]any{"reference": "aggregated-deletecollection"},
	}}
	_, err := client.Resource(flunderGVR).Namespace(ns).
		Create(context.Background(), flunder, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create flunder %s: %w", name, err)
	}
	return nil
}

// deleteFlunderCollection issues the name-less collection delete this spec is about.
func deleteFlunderCollection(client dynamic.Interface, ns, labelSelector string) error {
	err := client.Resource(flunderGVR).Namespace(ns).DeleteCollection(
		context.Background(),
		metav1.DeleteOptions{},
		metav1.ListOptions{LabelSelector: labelSelector},
	)
	if err != nil {
		return fmt.Errorf("deletecollection flunders (%s): %w", labelSelector, err)
	}
	return nil
}
