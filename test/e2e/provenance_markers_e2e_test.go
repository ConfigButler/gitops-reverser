// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Provenance markers: what each GitOps producer stamps on the objects it creates.
//
// GitOps Reverser is a live -> Git tool, so before it may mirror a live object it
// must answer one question: DOES THIS OBJECT HAVE A HOME IN GIT? An object a
// controller synthesised has none, and writing it to Git invents a second source of
// truth that fights the controller that made it.
//
// The answer is read off the provenance evidence the applying controller leaves
// behind — and the design docs assumed that evidence was a controller
// ownerReference. It is not. This spec pins what the evidence ACTUALLY is, against
// real controllers, so the gate is built on measurement rather than on memory.
//
// It is the executable form of docs/facts/expansion-provenance-markers.md. If a
// controller upgrade changes a marker, this spec fails loudly — which is the point.
//
// Five producers, one Git repository, one commit:
//
//	Flux Kustomization  -> kustomize.toolkit.fluxcd.io/*        source: a FOLDER   -> has a home
//	Argo CD Application -> argocd.argoproj.io/tracking-id       source: a FOLDER   -> has a home
//	Flux HelmRelease    -> helm.toolkit.fluxcd.io/*             source: a CHART    -> NO home
//	flux-operator RS    -> resourceset.fluxcd.controlplane.io/* source: a TEMPLATE -> NO home
//	Argo ApplicationSet -> ownerReference on the Application    source: a GENERATOR-> NO home
//
// Note the two rows that decide everything: kustomize.toolkit and helm.toolkit are
// SIBLING PREFIXES from the same vendor with OPPOSITE verdicts. Any rule that gates
// on the prefix family gets one of them exactly backwards.
//
// Lives in the bi-directional corner because that is the only place Argo CD is
// installed (docs/spec/e2e-bi-directional-corner.md). It drives no GitTarget and no
// WatchRule: the reverser is not under test here — the ecosystem is.
var _ = Describe("Expansion provenance markers", Label("bi-directional", "provenance"), Ordered, func() {
	var run provenanceRun
	var testNs string

	BeforeAll(func() {
		skipUnlessBiDirectionalEnabled()
		requireArgoCDInstalled()

		By("creating test namespace")
		testNs = testNamespaceFor("provenance")
		_, _ = kubectlRun("create", "namespace", testNs) // idempotent; ignore AlreadyExists

		By("setting up a Gitea repo the producers all read from")
		provenanceRepo = SetupRepo(
			resolveE2EContext(),
			testNs,
			fmt.Sprintf("e2e-provenance-%d", GinkgoRandomSeed()),
		)

		_, err := kubectlRunInNamespace(testNs, "apply", "-f", provenanceRepo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")

		run = newProvenanceRun(testNs)
		run.assertCheckoutReady()

		// SetupRepo leaves the remote EMPTY, so `main` does not exist until we push.
		// Every producer below sources from that branch, so it must be seeded first.
		By("seeding main with one folder of plain KRM and one Helm chart")
		run.writeRepoContents()
		Expect(run.commitAllAndPush("provenance: seed folders and chart")).To(Succeed())
	})

	AfterAll(func() {
		if !biDirectionalEnabled() {
			// BeforeAll skipped before creating anything; `run` and `testNs` are zero.
			return
		}
		run.cleanup()
		cleanupNamespace(testNs)
	})

	It("records what each producer stamps, and whether its source is a folder or a template", func() {
		run.applyFluxGitRepository()

		// ------------------------------------------------------------------
		// Producer 1 — Flux Kustomization. Source: a folder of files.
		// ------------------------------------------------------------------
		By("applying a Flux Kustomization over a folder of plain KRM")
		run.applyFluxKustomization()
		run.waitForConfigMap(run.kustomizeCM)

		prov := run.configMapProvenance(run.kustomizeCM)

		Expect(prov.labels).To(HaveKeyWithValue(provKustomizeNameLabel, run.fluxKustomizationName),
			"kustomize-controller must label the objects it applies")
		Expect(prov.labels).To(HaveKey(provKustomizeNamespaceLabel))
		Expect(prov.owners).To(BeEmpty(),
			"kustomize-controller sets NO ownerReference — the derived-object gate cannot key on one")
		Expect(prov.managers).To(ContainElement("kustomize-controller"))
		Expect(prov.annotations).NotTo(HaveKey(provArgoTrackingID))

		// ------------------------------------------------------------------
		// Producer 2 — Flux HelmRelease. Source: a chart. SAME repo, SAME commit.
		// ------------------------------------------------------------------
		By("applying a Flux HelmRelease whose chart is a folder in the same repository")
		run.applyFluxHelmRelease()
		run.waitForHelmReleaseReady()
		run.waitForConfigMap(run.helmCM)

		prov = run.configMapProvenance(run.helmCM)

		Expect(prov.labels).To(HaveKeyWithValue(provHelmNameLabel, run.fluxHelmReleaseName),
			"helm-controller labels rendered objects with helm.toolkit.fluxcd.io/name")
		Expect(prov.labels).To(HaveKey(provHelmNamespaceLabel))
		Expect(prov.owners).To(BeEmpty(),
			"helm-controller sets NO ownerReference either — a HelmRelease's output is invisible to an ownerReference gate")
		Expect(prov.managers).To(ContainElement("helm-controller"))

		// THE LOAD-BEARING CONTRAST. Both objects came from the same repository and
		// the same commit. Both carry a `*.toolkit.fluxcd.io/` label. One has a home
		// file and one does not, and the label prefix does not tell them apart —
		// only the controller's SOURCE does.
		Expect(prov.labels).NotTo(HaveKey(provKustomizeNameLabel),
			"the two Flux markers must be distinguishable: they carry opposite verdicts")

		// ------------------------------------------------------------------
		// Producer 3 — flux-operator ResourceSet. Source: a template in the CR.
		// ------------------------------------------------------------------
		By("applying a flux-operator ResourceSet with two inputs and inline KRM")
		run.applyResourceSet()

		for _, tenant := range []string{run.tenantOneCM, run.tenantTwoCM} {
			run.waitForConfigMap(tenant)
			prov = run.configMapProvenance(tenant)

			Expect(prov.labels).To(HaveKeyWithValue(provResourceSetNameLabel, run.resourceSetName),
				"flux-operator labels expanded objects with resourceset.fluxcd.controlplane.io/name")
			Expect(prov.labels).To(HaveKey(provResourceSetNamespaceLabel))
			Expect(prov.owners).To(BeEmpty(),
				"flux-operator sets NO ownerReference — it tracks children by label plus status.inventory")
			Expect(prov.managers).To(ContainElement("flux-operator"))
		}

		// The parent's inventory is the only enumeration of what it owns.
		By("asserting the ResourceSet's status.inventory enumerates both children")
		Expect(run.resourceSetInventoryIDs()).To(HaveLen(2),
			"status.inventory is flux-operator's substitute for ownerReferences")

		// ------------------------------------------------------------------
		// Producer 4 — Argo CD Application. Source: a folder of files.
		// ------------------------------------------------------------------
		By("applying an Argo CD Application over a folder of plain KRM")
		run.applyArgoRepoSecret()
		run.applyArgoApplication()
		run.syncArgoApplication()
		run.waitForConfigMap(run.argoCM)

		prov = run.configMapProvenance(run.argoCM)

		Expect(prov.annotations).To(HaveKeyWithValue(provArgoTrackingID, run.expectedTrackingID()),
			"Argo CD's default tracking method is `annotation`, and this is its exact format")
		Expect(prov.owners).To(BeEmpty(), "Argo CD sets no ownerReference on the objects it applies")
		Expect(prov.managers).To(ContainElement("argocd-controller"))
		Expect(prov.labels).NotTo(HaveKey(provKustomizeNameLabel))

		// ------------------------------------------------------------------
		// Producer 5 — Argo CD ApplicationSet. Source: a generator.
		// The ONLY producer of the five that uses an ownerReference.
		// ------------------------------------------------------------------
		By("applying an ApplicationSet and reading the Application it generates")
		run.applyApplicationSet()
		run.waitForGeneratedApplication()

		generated := run.generatedApplication()

		genOwners, _, err := unstructured.NestedSlice(generated.Object, "metadata", "ownerReferences")
		Expect(err).NotTo(HaveOccurred())
		Expect(genOwners).To(HaveLen(1),
			"the applicationset-controller IS the one producer that sets an ownerReference")

		owner, ok := genOwners[0].(map[string]interface{})
		Expect(ok).To(BeTrue())
		Expect(owner["kind"]).To(Equal("ApplicationSet"))
		Expect(owner["name"]).To(Equal(run.applicationSetName))
		Expect(owner["controller"]).To(BeTrue())

		// The structural asymmetry with a ResourceSet, in one assertion: the
		// generated Application has no home file, but it POINTS AT one. So the
		// workloads beneath it round-trip normally — which is exactly what a
		// ResourceSet's expanded objects can never do.
		By("asserting the generated Application points at a real path in Git")
		sourcePath, _, err := unstructured.NestedString(generated.Object, "spec", "source", "path")
		Expect(err).NotTo(HaveOccurred())
		Expect(sourcePath).To(Equal(provAppSetPath))
		Expect(run.repoPath(provAppSetPath, "configmap.yaml")).To(BeAnExistingFile(),
			"the path the generated Application names is a real folder of real files")
	})
})

// provenanceRepo holds the file-local repo fixtures for the provenance describe block.
var provenanceRepo *RepoArtifacts

// The markers themselves. Each is a fact about a controller, verified by this spec.
const (
	provKustomizeNameLabel        = "kustomize.toolkit.fluxcd.io/name"
	provKustomizeNamespaceLabel   = "kustomize.toolkit.fluxcd.io/namespace"
	provHelmNameLabel             = "helm.toolkit.fluxcd.io/name"
	provHelmNamespaceLabel        = "helm.toolkit.fluxcd.io/namespace"
	provResourceSetNameLabel      = "resourceset.fluxcd.controlplane.io/name"
	provResourceSetNamespaceLabel = "resourceset.fluxcd.controlplane.io/namespace"
	provArgoTrackingID            = "argocd.argoproj.io/tracking-id"
)

// Paths inside the seeded repository. Each producer gets its own folder so that no
// two of them apply the same object — two Argo Applications over one path would
// each stamp their own tracking-id and raise SharedResourceWarning on the other.
const (
	provFluxPath   = "flux-apps"
	provArgoPath   = "argo-apps"
	provAppSetPath = "appset-apps"
	provChartPath  = "chart"
)

const (
	provEventuallyTimeout = 120 * time.Second
	provHelmTimeout       = 180 * time.Second
)

type provenanceRun struct {
	gitCheckout

	testNs string
	argoNs string

	repoURL string

	fluxSecretName        string
	fluxGitRepositoryName string
	fluxKustomizationName string
	fluxHelmReleaseName   string
	resourceSetName       string

	argoRepoSecretName string
	argoAppName        string
	applicationSetName string
	generatedAppName   string

	// One ConfigMap per producer, so a marker can never be attributed to the wrong one.
	kustomizeCM string
	helmCM      string
	argoCM      string
	tenantOneCM string
	tenantTwoCM string
}

func newProvenanceRun(testNs string) provenanceRun {
	id := strconv.FormatInt(time.Now().UnixNano(), 10)
	return provenanceRun{
		gitCheckout:           newGitCheckout(provenanceRepo, testNs),
		testNs:                testNs,
		argoNs:                argoNamespace(),
		repoURL:               provenanceRepo.RepoURLHTTP,
		fluxSecretName:        fmt.Sprintf("prov-auth-%s", id),
		fluxGitRepositoryName: fmt.Sprintf("prov-repo-%s", id),
		fluxKustomizationName: fmt.Sprintf("prov-kustomization-%s", id),
		fluxHelmReleaseName:   fmt.Sprintf("prov-helm-%s", id),
		resourceSetName:       fmt.Sprintf("prov-rs-%s", id),
		argoRepoSecretName:    fmt.Sprintf("prov-argo-repo-%s", id),
		argoAppName:           fmt.Sprintf("prov-app-%s", id),
		applicationSetName:    fmt.Sprintf("prov-appset-%s", id),
		generatedAppName:      fmt.Sprintf("prov-generated-%s", id),
		kustomizeCM:           "prov-kustomize-applied",
		helmCM:                "prov-helm-rendered",
		argoCM:                "prov-argo-applied",
		tenantOneCM:           "prov-tenant-one",
		tenantTwoCM:           "prov-tenant-two",
	}
}

// writeRepoContents seeds three folders of plain KRM and one real Helm chart.
//
// The chart's template holds `{{ .Release.Namespace }}`, which Helm must render and
// Go's text/template must never see — so these files are written directly rather
// than through renderTemplate.
func (r provenanceRun) writeRepoContents() {
	GinkgoHelper()

	configMap := func(name string) string {
		return fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
data:
  producer: %s
`, name, r.testNs, name)
	}

	for path, cm := range map[string]string{
		provFluxPath:   r.kustomizeCM,
		provArgoPath:   r.argoCM,
		provAppSetPath: "prov-appset-never-synced",
	} {
		dir := r.repoPath(path)
		Expect(os.MkdirAll(dir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "configmap.yaml"), []byte(configMap(cm)), 0o644)).To(Succeed())
	}

	chartDir := r.repoPath(provChartPath)
	Expect(os.MkdirAll(filepath.Join(chartDir, "templates"), 0o755)).To(Succeed())
	Expect(os.WriteFile(filepath.Join(chartDir, "Chart.yaml"), []byte(`apiVersion: v2
name: provenance
version: 0.1.0
description: A minimal chart, so the helm-controller has something real to render.
`), 0o644)).To(Succeed())
	Expect(os.WriteFile(filepath.Join(chartDir, "templates", "configmap.yaml"), []byte(fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: {{ .Release.Namespace }}
data:
  producer: helm-controller
`, r.helmCM)), 0o644)).To(Succeed())
}

func (r provenanceRun) applyFluxGitRepository() {
	GinkgoHelper()
	username, password := r.readGitCredentialSecretDataBase64()
	Expect(applyFromTemplate("test/e2e/templates/bi-directional/flux-gitrepository-http.tmpl", struct {
		Namespace  string
		SecretName string
		Name       string
		RepoURL    string
		Branch     string
		Interval   string
		Username   string
		Password   string
	}{
		Namespace:  "flux-system",
		SecretName: r.fluxSecretName,
		Name:       r.fluxGitRepositoryName,
		RepoURL:    r.repoURL,
		Branch:     "main",
		Interval:   "30m",
		Username:   username,
		Password:   password,
	}, "flux-system")).To(Succeed(), "failed to apply Flux GitRepository")
}

func (r provenanceRun) applyFluxKustomization() {
	GinkgoHelper()
	Expect(applyFromTemplate("test/e2e/templates/bi-directional/flux-kustomization.tmpl", struct {
		Namespace            string
		Name                 string
		Path                 string
		SourceName           string
		Interval             string
		DependsOn            string
		Prune                bool
		Wait                 bool
		TargetNS             string
		HasTargetNS          bool
		DecryptionProvider   string
		DecryptionSecretName string
	}{
		Namespace:  "flux-system",
		Name:       r.fluxKustomizationName,
		Path:       "./" + provFluxPath,
		SourceName: r.fluxGitRepositoryName,
		Interval:   "30m",
		Prune:      true,
		Wait:       true,
	}, "flux-system")).To(Succeed(), "failed to apply Flux Kustomization")
}

func (r provenanceRun) applyFluxHelmRelease() {
	GinkgoHelper()
	Expect(applyFromTemplate("test/e2e/templates/provenance/helmrelease.tmpl", struct {
		Name            string
		Namespace       string
		TargetNamespace string
		ChartPath       string
		SourceName      string
		Interval        string
	}{
		Name:            r.fluxHelmReleaseName,
		Namespace:       "flux-system",
		TargetNamespace: r.testNs,
		ChartPath:       "./" + provChartPath,
		SourceName:      r.fluxGitRepositoryName,
		Interval:        "30m",
	}, "flux-system")).To(Succeed(), "failed to apply Flux HelmRelease")
}

func (r provenanceRun) applyResourceSet() {
	GinkgoHelper()
	Expect(applyFromTemplate("test/e2e/templates/provenance/resourceset.tmpl", struct {
		Name            string
		Namespace       string
		TargetNamespace string
		TenantOne       string
		TenantTwo       string
	}{
		Name:            r.resourceSetName,
		Namespace:       "flux-system",
		TargetNamespace: r.testNs,
		TenantOne:       r.tenantOneCM,
		TenantTwo:       r.tenantTwoCM,
	}, "flux-system")).To(Succeed(), "failed to apply flux-operator ResourceSet")
}

func (r provenanceRun) applyArgoRepoSecret() {
	GinkgoHelper()
	username, password := r.readGitCredentialSecretDataDecoded()
	Expect(applyFromTemplate("test/e2e/templates/bi-directional/argocd-repo-secret.tmpl", struct {
		Name      string
		Namespace string
		RepoURL   string
		Username  string
		Password  string
	}{
		Name:      r.argoRepoSecretName,
		Namespace: r.argoNs,
		RepoURL:   r.repoURL,
		Username:  username,
		Password:  password,
	}, r.argoNs)).To(Succeed(), "failed to apply Argo CD repository Secret")
}

func (r provenanceRun) applyArgoApplication() {
	GinkgoHelper()
	Expect(applyFromTemplate("test/e2e/templates/bi-directional/argocd-application.tmpl", argoAppConfig{
		Name:                 r.argoAppName,
		ArgoNamespace:        r.argoNs,
		RepoURL:              r.repoURL,
		Branch:               "main",
		Path:                 provArgoPath,
		DestinationNamespace: r.testNs,
	}, r.argoNs)).To(Succeed(), "failed to apply Argo CD Application")
}

func (r provenanceRun) applyApplicationSet() {
	GinkgoHelper()
	Expect(applyFromTemplate("test/e2e/templates/provenance/applicationset.tmpl", struct {
		Name                 string
		ArgoNamespace        string
		AppSuffix            string
		RepoURL              string
		Branch               string
		Path                 string
		DestinationNamespace string
	}{
		Name:                 r.applicationSetName,
		ArgoNamespace:        r.argoNs,
		AppSuffix:            r.generatedAppName,
		RepoURL:              r.repoURL,
		Branch:               "main",
		Path:                 provAppSetPath,
		DestinationNamespace: r.testNs,
	}, r.argoNs)).To(Succeed(), "failed to apply Argo CD ApplicationSet")
}

// syncArgoApplication refreshes and syncs without the push webhook: this spec never
// pushes a second commit, so there is nothing for a webhook to notice, and Argo's
// own poll is ~120s + jitter.
func (r provenanceRun) syncArgoApplication() {
	GinkgoHelper()
	By("refreshing and syncing the Argo CD Application")

	_, err := kubectlRunInNamespace(r.argoNs, "annotate", "application.argoproj.io", r.argoAppName,
		"argocd.argoproj.io/refresh=normal", "--overwrite")
	Expect(err).NotTo(HaveOccurred(), "failed to request an Argo CD refresh")

	Eventually(func(g Gomega) {
		_, patchErr := kubectlRunInNamespace(r.argoNs, "patch", "application.argoproj.io", r.argoAppName,
			"--type=merge", "--patch",
			`{"operation":{"initiatedBy":{"username":"e2e"},"sync":{"revision":"main"}}}`)
		g.Expect(patchErr).NotTo(HaveOccurred())
	}, provEventuallyTimeout, biPollInterval).Should(Succeed())

	Eventually(func(g Gomega) {
		out, getErr := kubectlRunInNamespace(r.argoNs, "get", "application.argoproj.io", r.argoAppName,
			"-o", "jsonpath={.status.operationState.phase}")
		g.Expect(getErr).NotTo(HaveOccurred())
		g.Expect(out).To(Equal("Succeeded"), "Argo CD sync did not succeed")
	}, provEventuallyTimeout, biPollInterval).Should(Succeed())
}

func (r provenanceRun) expectedTrackingID() string {
	// <app>:<group>/<Kind>:<namespace>/<name>. A core ConfigMap has an empty group.
	return fmt.Sprintf("%s:/ConfigMap:%s/%s", r.argoAppName, r.testNs, r.argoCM)
}

func (r provenanceRun) waitForConfigMap(name string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		_, err := kubectlRunInNamespace(r.testNs, "get", "configmap", name)
		g.Expect(err).NotTo(HaveOccurred())
	}, provEventuallyTimeout, biPollInterval).Should(Succeed(), fmt.Sprintf("ConfigMap %q never appeared", name))
}

func (r provenanceRun) waitForHelmReleaseReady() {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		out, err := kubectlRunInNamespace("flux-system", "get", "helmrelease", r.fluxHelmReleaseName,
			"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).To(Equal("True"), "HelmRelease never became Ready")
	}, provHelmTimeout, biPollInterval).Should(Succeed())
}

// objectProvenance is everything a provenance gate could possibly key on.
type objectProvenance struct {
	labels      map[string]string
	annotations map[string]string
	owners      []interface{}
	managers    []string
}

// configMapProvenance reads every provenance signal off one live ConfigMap.
func (r provenanceRun) configMapProvenance(name string) objectProvenance {
	GinkgoHelper()

	// --show-managed-fields is required: kubectl's json/yaml printers strip
	// managedFields by default, and the applying field manager is one of the four
	// provenance signals this spec exists to record.
	out, err := kubectlRunInNamespace(r.testNs, "get", "configmap", name, "-o", "json", "--show-managed-fields")
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("failed to read ConfigMap %q", name))

	var obj unstructured.Unstructured
	Expect(json.Unmarshal([]byte(out), &obj.Object)).To(Succeed())

	var prov objectProvenance

	prov.labels, _, err = unstructured.NestedStringMap(obj.Object, "metadata", "labels")
	Expect(err).NotTo(HaveOccurred())
	prov.annotations, _, err = unstructured.NestedStringMap(obj.Object, "metadata", "annotations")
	Expect(err).NotTo(HaveOccurred())
	prov.owners, _, err = unstructured.NestedSlice(obj.Object, "metadata", "ownerReferences")
	Expect(err).NotTo(HaveOccurred())

	fields, _, err := unstructured.NestedSlice(obj.Object, "metadata", "managedFields")
	Expect(err).NotTo(HaveOccurred())
	for _, f := range fields {
		entry, ok := f.(map[string]interface{})
		if !ok {
			continue
		}
		if manager, ok := entry["manager"].(string); ok {
			prov.managers = append(prov.managers, manager)
		}
	}

	return prov
}

func (r provenanceRun) resourceSetInventoryIDs() []interface{} {
	GinkgoHelper()
	var entries []interface{}

	Eventually(func(g Gomega) {
		out, err := kubectlRunInNamespace("flux-system", "get", "resourceset", r.resourceSetName, "-o", "json")
		g.Expect(err).NotTo(HaveOccurred())

		var obj unstructured.Unstructured
		g.Expect(json.Unmarshal([]byte(out), &obj.Object)).To(Succeed())

		var (
			found     bool
			nestedErr error
		)
		entries, found, nestedErr = unstructured.NestedSlice(obj.Object, "status", "inventory", "entries")
		g.Expect(nestedErr).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue(), "ResourceSet has no status.inventory")
	}, provEventuallyTimeout, biPollInterval).Should(Succeed())

	return entries
}

func (r provenanceRun) waitForGeneratedApplication() {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		_, err := kubectlRunInNamespace(r.argoNs, "get", "application.argoproj.io", r.generatedAppName)
		g.Expect(err).NotTo(HaveOccurred())
	}, provEventuallyTimeout, biPollInterval).Should(Succeed(),
		"the ApplicationSet never generated an Application")
}

func (r provenanceRun) generatedApplication() *unstructured.Unstructured {
	GinkgoHelper()
	out, err := kubectlRunInNamespace(r.argoNs, "get", "application.argoproj.io", r.generatedAppName, "-o", "json")
	Expect(err).NotTo(HaveOccurred())

	var obj unstructured.Unstructured
	Expect(json.Unmarshal([]byte(out), &obj.Object)).To(Succeed())
	return &obj
}

func (r provenanceRun) cleanup() {
	// The ApplicationSet must go first: deleting it garbage-collects the Application
	// it owns, via the one ownerReference this whole spec exists to demonstrate.
	cleanupNamespacedResource(r.argoNs, "applicationset", r.applicationSetName)
	cleanupNamespacedResource(r.argoNs, "application", r.argoAppName)
	cleanupNamespacedResource(r.argoNs, "secret", r.argoRepoSecretName)

	cleanupNamespacedResource("flux-system", "resourceset", r.resourceSetName)
	cleanupNamespacedResource("flux-system", "helmrelease", r.fluxHelmReleaseName)
	cleanupNamespacedResource("flux-system", "kustomization", r.fluxKustomizationName)
	cleanupNamespacedResource("flux-system", "gitrepository", r.fluxGitRepositoryName)
	cleanupNamespacedResource("flux-system", "secret", r.fluxSecretName)
}
