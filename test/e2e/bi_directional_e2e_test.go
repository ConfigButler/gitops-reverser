/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

const biDirectionalEnabledEnv = "E2E_ENABLE_BI_DIRECTIONAL"

const (
	biPollInterval          = time.Second
	biEventuallyTimeout     = 45 * time.Second
	biFluxReconcileTimeout  = "90s"
	biStableCountShortWait  = 3 * time.Second
	biStableCountMediumWait = 5 * time.Second
	biStableCountLongWait   = 6 * time.Second
)

type biDirectionalRun struct {
	testID                     string
	testNs                     string
	repoName                   string
	checkoutDir                string
	repoURL                    string
	localGitRepoURL            string
	fluxSecretName             string
	fluxGitRepositoryName      string
	fluxCRDsName               string
	fluxLiveName               string
	fluxDecryptionSecret       string
	controllerEncryptionSecret string
	fluxSourceInterval         string
	fluxApplyInterval          string
	gitProviderName            string
	gitTargetName              string
	watchRuleName              string
	secretWatchRuleName        string
	livePath                   string
	crdPath                    string
	firstOrderName             string
	secondOrderName            string
	revertedOrderName          string
	reverseSecretName          string
	reverseSecretKey           string
	reverseSecretInitial       string
	reverseSecretUpdated       string
	reverseContainer           string
	reverseFlavor              string
	reverseTopping             string
}

type iceCreamOrderFile struct {
	Name         string
	Namespace    string
	CustomerName string
	Container    string
	Scoops       []iceCreamScoop
	Toppings     []string
}

type iceCreamScoop struct {
	Flavor   string
	Quantity int
}

var _ = Describe("Bi Directional", Label("bi-directional"), Ordered, func() {
	var run biDirectionalRun
	var testNs string

	BeforeAll(func() {
		if !biDirectionalEnabled() {
			Skip(fmt.Sprintf("bi-directional e2e is disabled; set %s=true to run", biDirectionalEnabledEnv))
		}

		run = newBiDirectionalRun()

		By("creating test namespace and applying git secrets")
		testNs = testNamespaceFor("bi-directional")
		run.testNs = testNs
		_, _ = kubectlRun("create", "namespace", testNs) // idempotent; ignore AlreadyExists
		secretsYaml := strings.TrimSpace(os.Getenv("E2E_SECRETS_YAML"))
		Expect(secretsYaml).NotTo(BeEmpty(), "E2E_SECRETS_YAML must be set by BeforeSuite")
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", secretsYaml)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")
		applySOPSAgeKeyToNamespace(testNs)
		run.assertCheckoutReady()
	})

	AfterAll(func() {
		run.cleanupFluxResources()
		run.cleanupReverseResources(testNs)
		_, _ = kubectlRunInNamespace(
			testNs,
			"delete",
			"secret",
			run.controllerEncryptionSecret,
			"--ignore-not-found=true",
		)
		_, _ = kubectlRunInNamespace(
			testNs,
			"delete",
			"secret",
			run.reverseSecretName,
			"--ignore-not-found=true",
		)
		_, _ = kubectlRunInNamespace(
			testNs,
			"delete",
			"icecreamorder",
			run.firstOrderName,
			"--ignore-not-found=true",
		)
		_, _ = kubectlRunInNamespace(
			testNs,
			"delete",
			"icecreamorder",
			run.secondOrderName,
			"--ignore-not-found=true",
		)
		_, _ = kubectlRunInNamespace(
			testNs,
			"delete",
			"icecreamorder",
			run.revertedOrderName,
			"--ignore-not-found=true",
		)
		_, _ = kubectlRun("delete", "crd", "icecreamorders.shop.example.com", "--ignore-not-found=true")

		By("deleting test namespace")
		_, _ = kubectlRun("delete", "namespace", testNs, "--ignore-not-found=true")
	})

	It("should avoid a commit loop while Flux and gitops-reverser share IceCreamOrder resources", func() {
		baselineCommitCount, err := run.gitMainCommitCount()
		Expect(err).NotTo(HaveOccurred(), "failed to capture baseline git commit count")

		By("committing the IceCreamOrder CRD through normal GitOps")
		run.writeCRDToRepo()
		Expect(run.commitAllAndPush("bi-directional: add icecreamorder crd")).To(Succeed())
		crdHead := run.gitHEAD()
		run.expectRemoteCommitCount(baselineCommitCount + 1)

		By("configuring Flux to sync the repository with separate CRD and live kustomizations")
		run.applyFluxGitRepository()
		run.applyFluxCRDKustomization()
		run.reconcileFluxSource()
		run.reconcileFluxKustomization(run.fluxCRDsName)
		run.waitForFluxGitRepositoryRevision(crdHead)
		run.waitForFluxKustomizationRevision(run.fluxCRDsName, crdHead)
		run.waitForCRDEstablished()
		run.consistentlyExpectRemoteCommitCount(baselineCommitCount+1, biStableCountMediumWait)

		By("enabling gitops-reverser before Flux starts managing IceCreamOrder resources")
		createGitProviderWithURLInNamespace(run.gitProviderName, testNs, e2eGitSecretHTTP(), run.repoURL)
		createGitTargetWithEncryptionOptions(
			run.gitTargetName,
			testNs,
			run.gitProviderName,
			run.livePath,
			"main",
			run.controllerEncryptionSecret,
			true,
		)

		err = applyFromTemplate("test/e2e/templates/watchrule-crd.tmpl", struct {
			Name            string
			Namespace       string
			DestinationName string
		}{
			Name:            run.watchRuleName,
			Namespace:       testNs,
			DestinationName: run.gitTargetName,
		}, testNs)
		Expect(err).NotTo(HaveOccurred(), "failed to apply bi-directional WatchRule")
		verifyResourceStatus("gitprovider", run.gitProviderName, testNs, "True", "Ready", "")
		run.waitForControllerEncryptionSecret()
		run.applyFluxDecryptionSecretFromControllerSecret()
		run.applyFluxLiveKustomization()
		run.waitForGitTargetReady()
		verifyResourceStatus("watchrule", run.watchRuleName, testNs, "True", "Ready", "")

		sharedBaselineCommitCount := run.waitForStableRemoteCommitCount(biStableCountShortWait)

		By("committing two IceCreamOrders through normal GitOps")
		run.writeLiveOrder(iceCreamOrderFile{
			Name:         run.firstOrderName,
			Namespace:    testNs,
			CustomerName: "Alice",
			Container:    "Cone",
			Scoops: []iceCreamScoop{
				{Flavor: "Vanilla", Quantity: 2},
			},
			Toppings: []string{"Sprinkles"},
		})
		run.writeLiveOrder(iceCreamOrderFile{
			Name:         run.secondOrderName,
			Namespace:    testNs,
			CustomerName: "Bob",
			Container:    "Cup",
			Scoops: []iceCreamScoop{
				{Flavor: "Strawberry", Quantity: 1},
			},
			Toppings: []string{"WhippedCream"},
		})
		Expect(run.commitAllAndPush("bi-directional: add two icecream orders")).To(Succeed())
		normalFlowHead := run.gitHEAD()
		run.reconcileFluxSource()
		run.reconcileFluxKustomization(run.fluxLiveName)
		run.waitForFluxGitRepositoryRevision(normalFlowHead)
		run.waitForFluxKustomizationRevision(run.fluxLiveName, normalFlowHead)
		run.waitForOrderSpec(run.firstOrderName, "Cone", "Vanilla", "Sprinkles")
		run.waitForOrderSpec(run.secondOrderName, "Cup", "Strawberry", "WhippedCream")
		run.consistentlyExpectRemoteCommitCount(sharedBaselineCommitCount+1, biStableCountLongWait)

		By("changing one IceCreamOrder through the Kubernetes API")
		_, err = kubectlRunInNamespace(
			testNs,
			"patch",
			"icecreamorder",
			run.firstOrderName,
			"--type=merge",
			"--patch",
			fmt.Sprintf(`{"spec":{"container":"%s","scoops":[{"flavor":"%s","quantity":4}],"toppings":["%s"]}}`,
				run.reverseContainer, run.reverseFlavor, run.reverseTopping),
		)
		Expect(err).NotTo(HaveOccurred(), "failed to patch IceCreamOrder through the API")

		By("verifying gitops-reverser creates one commit and Flux converges without a loop")
		Eventually(func(g Gomega) {
			g.Expect(run.gitPull()).To(Succeed())
			count, countErr := run.gitMainCommitCount()
			g.Expect(countErr).NotTo(HaveOccurred())
			g.Expect(count).To(Equal(sharedBaselineCommitCount + 2))
		}, biEventuallyTimeout, biPollInterval).Should(Succeed())

		run.waitForCommittedOrderSpec(run.firstOrderName, run.reverseContainer, run.reverseFlavor, run.reverseTopping)
		run.reconcileFluxSource()
		run.reconcileFluxKustomization(run.fluxLiveName)
		thirdHead := run.gitHEAD()
		run.waitForFluxGitRepositoryRevision(thirdHead)
		run.waitForFluxKustomizationRevision(run.fluxLiveName, thirdHead)
		run.waitForOrderSpec(run.firstOrderName, run.reverseContainer, run.reverseFlavor, run.reverseTopping)
		run.waitForOrderSpec(run.secondOrderName, "Cup", "Strawberry", "WhippedCream")
		run.waitForCommittedOrderSpec(run.secondOrderName, "Cup", "Strawberry", "WhippedCream")
		run.consistentlyExpectRemoteCommitCount(sharedBaselineCommitCount+2, biStableCountLongWait)

		By("adding a Secret to the shared live path with SOPS encryption")
		err = applyFromTemplate("test/e2e/templates/bi-directional/watchrule-secret.tmpl", struct {
			Name            string
			Namespace       string
			DestinationName string
		}{
			Name:            run.secretWatchRuleName,
			Namespace:       testNs,
			DestinationName: run.gitTargetName,
		}, testNs)
		Expect(err).NotTo(HaveOccurred(), "failed to apply Secret WatchRule")
		verifyResourceStatus("watchrule", run.secretWatchRuleName, testNs, "True", "Ready", "")

		_, _ = kubectlRunInNamespace(testNs, "delete", "secret", run.reverseSecretName, "--ignore-not-found=true")
		_, err = kubectlRunInNamespace(
			testNs,
			"create",
			"secret",
			"generic",
			run.reverseSecretName,
			"--from-literal",
			fmt.Sprintf("%s=%s", run.reverseSecretKey, run.reverseSecretInitial),
		)
		Expect(err).NotTo(HaveOccurred(), "failed to create Secret through the API")

		_, err = kubectlRunInNamespace(
			testNs,
			"patch",
			"secret",
			run.reverseSecretName,
			"--type=merge",
			"--patch",
			fmt.Sprintf(`{"stringData":{"%s":"%s"}}`, run.reverseSecretKey, run.reverseSecretUpdated),
		)
		Expect(err).NotTo(HaveOccurred(), "failed to patch Secret through the API")

		run.waitForCommittedEncryptedSecret(run.reverseSecretName, run.reverseSecretKey, run.reverseSecretUpdated)
		secretCommitCount, err := run.gitMainCommitCount()
		Expect(err).NotTo(HaveOccurred(), "failed to capture secret commit count")
		run.consistentlyExpectRemoteCommitCount(secretCommitCount, biStableCountShortWait)

		By("verifying Flux can reconcile the encrypted Secret without creating another reverse commit")
		secretHead := run.gitHEAD()
		run.reconcileFluxSource()
		run.reconcileFluxKustomization(run.fluxLiveName)
		run.waitForFluxGitRepositoryRevision(secretHead)
		run.waitForFluxKustomizationRevision(run.fluxLiveName, secretHead)
		run.waitForSecretValue(run.reverseSecretName, run.reverseSecretKey, run.reverseSecretUpdated)
		run.consistentlyExpectRemoteCommitCount(secretCommitCount, biStableCountMediumWait)

		By("testing if we can revert a commit")
		revertBaselineCommitCount, err := run.gitMainCommitCount()
		Expect(err).NotTo(HaveOccurred(), "failed to capture baseline git commit count for revert flow")

		run.writeLiveOrder(iceCreamOrderFile{
			Name:         run.revertedOrderName,
			Namespace:    testNs,
			CustomerName: "Charlie",
			Container:    "Cone",
			Scoops: []iceCreamScoop{
				{Flavor: "Chocolate", Quantity: 1},
			},
			Toppings: []string{"HotFudge"},
		})
		Expect(run.commitAllAndPush("bi-directional: add reversible icecream order")).To(Succeed())
		revertAddHead := run.gitHEAD()
		run.reconcileFluxSource()
		run.reconcileFluxKustomization(run.fluxLiveName)
		run.waitForFluxGitRepositoryRevision(revertAddHead)
		run.waitForFluxKustomizationRevision(run.fluxLiveName, revertAddHead)
		run.waitForCommittedOrderSpec(run.revertedOrderName, "Cone", "Chocolate", "HotFudge")
		run.waitForOrderSpec(run.revertedOrderName, "Cone", "Chocolate", "HotFudge")
		run.consistentlyExpectRemoteCommitCount(revertBaselineCommitCount+1, biStableCountMediumWait)

		Expect(run.revertHEADAndPush()).To(Succeed())
		revertHead := run.gitHEAD()
		run.reconcileFluxSource()
		run.reconcileFluxKustomization(run.fluxLiveName)
		run.waitForFluxGitRepositoryRevision(revertHead)
		run.waitForFluxKustomizationRevision(run.fluxLiveName, revertHead)
		run.waitForCommittedOrderDeleted(run.revertedOrderName)
		run.waitForOrderDeleted(run.revertedOrderName)
		run.consistentlyExpectRemoteCommitCount(revertBaselineCommitCount+2, biStableCountMediumWait)
	})
})

func biDirectionalEnabled() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(biDirectionalEnabledEnv)))
	return value == "1" || value == "true" || value == "yes"
}

func newBiDirectionalRun() biDirectionalRun {
	testID := strconv.FormatInt(time.Now().UnixNano(), 10)
	repoName := strings.TrimSpace(os.Getenv("E2E_REPO_NAME"))
	checkoutDir := strings.TrimSpace(os.Getenv("E2E_CHECKOUT_DIR"))

	Expect(repoName).NotTo(BeEmpty(), "E2E_REPO_NAME must be set by the suite")
	Expect(checkoutDir).NotTo(BeEmpty(), "E2E_CHECKOUT_DIR must be set by the suite")

	return biDirectionalRun{
		testID:                     testID,
		repoName:                   repoName,
		checkoutDir:                checkoutDir,
		repoURL:                    fmt.Sprintf(giteaRepoURLTemplate, repoName),
		localGitRepoURL:            fmt.Sprintf("http://localhost:13000/testorg/%s.git", repoName),
		fluxSecretName:             fmt.Sprintf("bi-flux-auth-%s", testID),
		fluxGitRepositoryName:      fmt.Sprintf("bi-repo-%s", testID),
		fluxCRDsName:               fmt.Sprintf("bi-crds-%s", testID),
		fluxLiveName:               fmt.Sprintf("bi-live-%s", testID),
		fluxDecryptionSecret:       fmt.Sprintf("bi-sops-%s", testID),
		controllerEncryptionSecret: fmt.Sprintf("bi-controller-sops-%s", testID),
		fluxSourceInterval:         "30m",
		fluxApplyInterval:          "30m",
		gitProviderName:            fmt.Sprintf("bi-provider-%s", testID),
		gitTargetName:              fmt.Sprintf("bi-target-%s", testID),
		watchRuleName:              fmt.Sprintf("bi-watchrule-%s", testID),
		secretWatchRuleName:        fmt.Sprintf("bi-secret-watchrule-%s", testID),
		livePath:                   fmt.Sprintf("bi-directional/%s/live", testID),
		crdPath:                    fmt.Sprintf("bi-directional/%s/crds", testID),
		firstOrderName:             fmt.Sprintf("bi-alice-order-%s", testID),
		secondOrderName:            fmt.Sprintf("bi-bob-order-%s", testID),
		revertedOrderName:          fmt.Sprintf("bi-charlie-order-%s", testID),
		reverseSecretName:          fmt.Sprintf("bi-secret-%s", testID),
		reverseSecretKey:           "password",
		reverseSecretInitial:       "do-not-commit",
		reverseSecretUpdated:       "never-commit-this",
		reverseContainer:           "WaffleBowl",
		reverseFlavor:              "MintChip",
		reverseTopping:             "Caramel",
	}
}

func (r biDirectionalRun) assertCheckoutReady() {
	_, err := os.Stat(filepath.Join(r.checkoutDir, ".git"))
	Expect(err).NotTo(HaveOccurred(), "expected checkout to exist at E2E_CHECKOUT_DIR")
	Expect(r.configureCheckoutAuth()).To(Succeed())
}

func (r biDirectionalRun) waitForCRDEstablished() {
	Eventually(func(g Gomega) {
		output, err := kubectlRun(
			"get",
			"crd",
			"icecreamorders.shop.example.com",
			"-o",
			"jsonpath={.status.conditions[?(@.type=='Established')].status}",
		)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(Equal("True"))
	}, 20*time.Second, biPollInterval).Should(Succeed())
}

func (r biDirectionalRun) repoPath(parts ...string) string {
	all := append([]string{r.checkoutDir}, parts...)
	return filepath.Join(all...)
}

func (r biDirectionalRun) liveOrderPath(name string) string {
	return r.repoPath(
		r.livePath,
		"shop.example.com",
		"v1",
		"icecreamorders",
		r.testNs,
		name+".yaml",
	)
}

func (r biDirectionalRun) liveSecretPath(name string) string {
	return r.repoPath(
		r.livePath,
		"v1",
		"secrets",
		r.testNs,
		name+".sops.yaml",
	)
}

func (r biDirectionalRun) writeCRDToRepo() {
	content, err := os.ReadFile("test/e2e/templates/icecreamorder-crd.yaml")
	Expect(err).NotTo(HaveOccurred(), "failed to read IceCreamOrder CRD fixture")
	Expect(
		os.MkdirAll(
			r.repoPath(r.crdPath, "apiextensions.k8s.io", "v1", "customresourcedefinitions"),
			0o755,
		),
	).To(Succeed())
	Expect(
		os.WriteFile(
			r.repoPath(
				r.crdPath,
				"apiextensions.k8s.io",
				"v1",
				"customresourcedefinitions",
				"icecreamorders.shop.example.com.yaml",
			),
			content,
			0o644,
		),
	).To(Succeed())
}

func (r biDirectionalRun) writeLiveOrder(order iceCreamOrderFile) {
	content, err := renderTemplate("test/e2e/templates/icecreamorder-instance.tmpl", struct {
		Name         string
		Namespace    string
		Labels       map[string]string
		Annotations  map[string]string
		CustomerName string
		Container    string
		Scoops       []iceCreamScoop
		Toppings     []string
	}{
		Name:         order.Name,
		Namespace:    order.Namespace,
		Labels:       nil,
		Annotations:  nil,
		CustomerName: order.CustomerName,
		Container:    order.Container,
		Scoops:       order.Scoops,
		Toppings:     order.Toppings,
	})
	Expect(err).NotTo(HaveOccurred(), "failed to render IceCreamOrder manifest")
	Expect(os.MkdirAll(filepath.Dir(r.liveOrderPath(order.Name)), 0o755)).To(Succeed())
	Expect(os.WriteFile(r.liveOrderPath(order.Name), []byte(content), 0o644)).To(Succeed())
}

func (r biDirectionalRun) commitAllAndPush(message string) error {
	if err := r.runGit("checkout", "-B", "main"); err != nil {
		return err
	}
	if err := r.runGit("add", "."); err != nil {
		return err
	}
	if err := r.runGit("commit", "--allow-empty", "-m", message); err != nil {
		return err
	}
	return r.runGit("push", "--set-upstream", "origin", "main")
}

func (r biDirectionalRun) revertHEADAndPush() error {
	if err := r.runGit("checkout", "-B", "main"); err != nil {
		return err
	}
	if err := r.runGit("revert", "--no-edit", "HEAD"); err != nil {
		return err
	}
	return r.runGit("push", "origin", "main")
}

func (r biDirectionalRun) configureCheckoutAuth() error {
	username, password := r.readGitCredentialSecretDataDecoded()
	authURL, err := r.authenticatedLocalGitURL(username, password)
	if err != nil {
		return err
	}
	return r.runGit("remote", "set-url", "origin", authURL)
}

func (r biDirectionalRun) authenticatedLocalGitURL(username, password string) (string, error) {
	parsedURL, err := url.Parse(r.localGitRepoURL)
	if err != nil {
		return "", fmt.Errorf("parse local Git repo URL: %w", err)
	}
	parsedURL.User = url.UserPassword(username, password)
	return parsedURL.String(), nil
}

func (r biDirectionalRun) runGit(args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.checkoutDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (r biDirectionalRun) runFlux(args ...string) error {
	fluxArgs := make([]string, 0, len(args)+2)
	if ctx := strings.TrimSpace(kubectlContext()); ctx != "" {
		fluxArgs = append(fluxArgs, "--context", ctx)
	}
	fluxArgs = append(fluxArgs, args...)

	cmd := exec.Command("flux", fluxArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("flux %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (r biDirectionalRun) gitPull() error {
	return r.runGit("pull", "--ff-only")
}

func (r biDirectionalRun) gitMainCommitCount() (int, error) {
	if err := r.runGit("rev-parse", "--verify", "refs/heads/main"); err != nil {
		if strings.Contains(err.Error(), "unknown revision") ||
			strings.Contains(err.Error(), "Needed a single revision") {
			return 0, nil
		}
		return 0, err
	}

	cmd := exec.Command("git", "rev-list", "--count", "refs/heads/main")
	cmd.Dir = r.checkoutDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("git rev-list --count refs/heads/main: %w: %s", err, strings.TrimSpace(string(output)))
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		return 0, fmt.Errorf("parse git commit count %q: %w", strings.TrimSpace(string(output)), err)
	}
	return count, nil
}

func (r biDirectionalRun) gitHEAD() string {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = r.checkoutDir
	output, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("failed to get git HEAD: %s", strings.TrimSpace(string(output))))
	return strings.TrimSpace(string(output))
}

func (r biDirectionalRun) expectRemoteCommitCount(expected int) {
	Eventually(func(g Gomega) {
		g.Expect(r.gitPull()).To(Succeed())
		count, err := r.gitMainCommitCount()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(count).To(Equal(expected))
	}, biEventuallyTimeout, biPollInterval).Should(Succeed())
}

func (r biDirectionalRun) consistentlyExpectRemoteCommitCount(expected int, duration time.Duration) {
	Consistently(func(g Gomega) {
		g.Expect(r.gitPull()).To(Succeed())
		count, err := r.gitMainCommitCount()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(count).To(Equal(expected))
	}, duration, biPollInterval).Should(Succeed())
}

func (r biDirectionalRun) applyFluxGitRepository() {
	username, password := r.readGitCredentialSecretDataBase64()
	err := applyFromTemplate("test/e2e/templates/bi-directional/flux-gitrepository-http.tmpl", struct {
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
		Interval:   r.fluxSourceInterval,
		Username:   username,
		Password:   password,
	}, "flux-system")
	Expect(err).NotTo(HaveOccurred(), "failed to apply Flux GitRepository")
}

func (r biDirectionalRun) waitForStableRemoteCommitCount(duration time.Duration) int {
	var stableCount int

	Eventually(func(g Gomega) {
		g.Expect(r.gitPull()).To(Succeed())
		count, err := r.gitMainCommitCount()
		g.Expect(err).NotTo(HaveOccurred())
		stableCount = count

		Consistently(func(inner Gomega) {
			inner.Expect(r.gitPull()).To(Succeed())
			currentCount, currentErr := r.gitMainCommitCount()
			inner.Expect(currentErr).NotTo(HaveOccurred())
			inner.Expect(currentCount).To(Equal(count))
		}, duration, biPollInterval).Should(Succeed())
	}, biEventuallyTimeout, biPollInterval).Should(Succeed())

	return stableCount
}

func (r biDirectionalRun) applyFluxDecryptionSecretFromControllerSecret() {
	ageKey := r.readControllerEncryptionAgeKey()

	manifest := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: flux-system
type: Opaque
stringData:
  identity.agekey: |
    %s
`, r.fluxDecryptionSecret, strings.TrimSpace(ageKey))

	_, err := kubectlRunWithStdin("flux-system", manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to apply Flux decryption Secret")
}

func (r biDirectionalRun) reconcileFluxSource() {
	By("reconciling the Flux GitRepository manually")
	Expect(
		r.runFlux(
			"reconcile",
			"source",
			"git",
			r.fluxGitRepositoryName,
			"-n",
			"flux-system",
			"--timeout",
			biFluxReconcileTimeout,
		),
	).To(Succeed())
}

func (r biDirectionalRun) reconcileFluxKustomization(name string) {
	By(fmt.Sprintf("reconciling Flux Kustomization %q manually", name))
	Expect(
		r.runFlux(
			"reconcile",
			"kustomization",
			name,
			"-n",
			"flux-system",
			"--timeout",
			biFluxReconcileTimeout,
		),
	).To(Succeed())
}

func (r biDirectionalRun) applyFluxCRDKustomization() {
	err := applyFromTemplate("test/e2e/templates/bi-directional/flux-kustomization.tmpl", struct {
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
		Name:       r.fluxCRDsName,
		Path:       "./" + r.crdPath,
		SourceName: r.fluxGitRepositoryName,
		Interval:   r.fluxApplyInterval,
		Prune:      true,
		Wait:       true,
	}, "flux-system")
	Expect(err).NotTo(HaveOccurred(), "failed to apply Flux CRD Kustomization")
}

func (r biDirectionalRun) applyFluxLiveKustomization() {
	err := applyFromTemplate("test/e2e/templates/bi-directional/flux-kustomization.tmpl", struct {
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
		Namespace:            "flux-system",
		Name:                 r.fluxLiveName,
		Path:                 "./" + r.livePath,
		SourceName:           r.fluxGitRepositoryName,
		Interval:             r.fluxApplyInterval,
		DependsOn:            r.fluxCRDsName,
		Prune:                true,
		Wait:                 true,
		DecryptionProvider:   "sops",
		DecryptionSecretName: r.fluxDecryptionSecret,
	}, "flux-system")
	Expect(err).NotTo(HaveOccurred(), "failed to apply Flux live Kustomization")
}

func (r biDirectionalRun) readGitCredentialSecretDataBase64() (string, string) {
	output, err := kubectlRunInNamespace(r.testNs, "get", "secret", e2eGitSecretHTTP(), "-o", "json")
	Expect(err).NotTo(HaveOccurred(), "failed to read git credential Secret for Flux")

	var obj unstructured.Unstructured
	Expect(json.Unmarshal([]byte(output), &obj)).To(Succeed())

	data, found, err := unstructured.NestedStringMap(obj.Object, "data")
	Expect(err).NotTo(HaveOccurred(), "failed to parse Secret data")
	Expect(found).To(BeTrue(), "git credential Secret data not found")

	username := strings.TrimSpace(data["username"])
	password := strings.TrimSpace(data["password"])
	Expect(username).NotTo(BeEmpty(), "git credential Secret username must be present")
	Expect(password).NotTo(BeEmpty(), "git credential Secret password must be present")

	return username, password
}

func (r biDirectionalRun) readGitCredentialSecretDataDecoded() (string, string) {
	usernameB64, passwordB64 := r.readGitCredentialSecretDataBase64()

	username, err := base64.StdEncoding.DecodeString(usernameB64)
	Expect(err).NotTo(HaveOccurred(), "failed to decode git credential Secret username")

	password, err := base64.StdEncoding.DecodeString(passwordB64)
	Expect(err).NotTo(HaveOccurred(), "failed to decode git credential Secret password")

	return strings.TrimSpace(string(username)), strings.TrimSpace(string(password))
}

func (r biDirectionalRun) waitForFluxGitRepositoryRevision(head string) {
	Eventually(func(g Gomega) {
		output, err := kubectlRunInNamespace(
			"flux-system",
			"get",
			"gitrepository",
			r.fluxGitRepositoryName,
			"-o",
			"json",
		)
		g.Expect(err).NotTo(HaveOccurred())

		var obj unstructured.Unstructured
		g.Expect(json.Unmarshal([]byte(output), &obj)).To(Succeed())

		revision, _, revErr := unstructured.NestedString(
			obj.Object,
			"status",
			"artifact",
			"revision",
		)
		g.Expect(revErr).NotTo(HaveOccurred())
		g.Expect(revision).To(ContainSubstring(head))
	}, biEventuallyTimeout, biPollInterval).Should(Succeed())
}

func (r biDirectionalRun) waitForFluxKustomizationRevision(name, head string) {
	Eventually(func(g Gomega) {
		output, err := kubectlRunInNamespace(
			"flux-system",
			"get",
			"kustomization",
			name,
			"-o",
			"json",
		)
		g.Expect(err).NotTo(HaveOccurred())

		var obj unstructured.Unstructured
		g.Expect(json.Unmarshal([]byte(output), &obj)).To(Succeed())

		conditions, found, condErr := unstructured.NestedSlice(obj.Object, "status", "conditions")
		g.Expect(condErr).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())

		ready := false
		for _, cond := range conditions {
			condMap, ok := cond.(map[string]interface{})
			if !ok {
				continue
			}
			if condMap["type"] == "Ready" && condMap["status"] == "True" {
				ready = true
				break
			}
		}
		g.Expect(ready).To(BeTrue())

		revision, _, revErr := unstructured.NestedString(
			obj.Object,
			"status",
			"lastAppliedRevision",
		)
		g.Expect(revErr).NotTo(HaveOccurred())
		g.Expect(revision).To(ContainSubstring(head))
	}, biEventuallyTimeout, biPollInterval).Should(Succeed())
}

func (r biDirectionalRun) waitForOrderSpec(name, container, flavor, topping string) {
	Eventually(func(g Gomega) {
		output, err := kubectlRunInNamespace(
			r.testNs,
			"get",
			"icecreamorder",
			name,
			"-o",
			"json",
		)
		g.Expect(err).NotTo(HaveOccurred())

		var obj unstructured.Unstructured
		g.Expect(json.Unmarshal([]byte(output), &obj)).To(Succeed())

		value, found, containerErr := unstructured.NestedString(obj.Object, "spec", "container")
		g.Expect(containerErr).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(value).To(Equal(container))

		scoops, found, scoopsErr := unstructured.NestedSlice(obj.Object, "spec", "scoops")
		g.Expect(scoopsErr).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(scoops).NotTo(BeEmpty())
		firstScoop, ok := scoops[0].(map[string]interface{})
		g.Expect(ok).To(BeTrue())
		g.Expect(firstScoop["flavor"]).To(Equal(flavor))

		toppings, found, toppingsErr := unstructured.NestedStringSlice(
			obj.Object,
			"spec",
			"toppings",
		)
		g.Expect(toppingsErr).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(toppings).To(ContainElement(topping))
	}, biEventuallyTimeout, biPollInterval).Should(Succeed())
}

func (r biDirectionalRun) waitForGitTargetReady() {
	Eventually(func(g Gomega) {
		output, err := kubectlRunInNamespace(
			r.testNs,
			"get",
			"gittarget",
			r.gitTargetName,
			"-o",
			"json",
		)
		g.Expect(err).NotTo(HaveOccurred())

		var obj unstructured.Unstructured
		g.Expect(json.Unmarshal([]byte(output), &obj)).To(Succeed())

		conditions, found, condErr := unstructured.NestedSlice(obj.Object, "status", "conditions")
		g.Expect(condErr).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue(), "status.conditions not found")

		var readyStatus, readyReason string
		for _, cond := range conditions {
			condMap, ok := cond.(map[string]interface{})
			if !ok {
				continue
			}
			if condMap["type"] == "Ready" {
				readyStatus, _ = condMap["status"].(string)
				readyReason, _ = condMap["reason"].(string)
				break
			}
		}

		g.Expect(readyStatus).To(Equal("True"))
		g.Expect([]string{"Ready", "OK"}).To(ContainElement(readyReason))
	}, biEventuallyTimeout, biPollInterval).Should(Succeed())
}

func (r biDirectionalRun) waitForControllerEncryptionSecret() {
	Eventually(func(g Gomega) {
		output, err := kubectlRunInNamespace(
			r.testNs,
			"get",
			"secret",
			r.controllerEncryptionSecret,
			"-o",
			"json",
		)
		g.Expect(err).NotTo(HaveOccurred())

		var obj unstructured.Unstructured
		g.Expect(json.Unmarshal([]byte(output), &obj)).To(Succeed())

		data, found, dataErr := unstructured.NestedStringMap(obj.Object, "data")
		g.Expect(dataErr).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(findFirstAgeKeySecretEntry(data)).NotTo(BeEmpty())
	}, biEventuallyTimeout, biPollInterval).Should(Succeed())
}

func (r biDirectionalRun) readControllerEncryptionAgeKey() string {
	output, err := kubectlRunInNamespace(
		r.testNs,
		"get",
		"secret",
		r.controllerEncryptionSecret,
		"-o",
		"json",
	)
	Expect(err).NotTo(HaveOccurred(), "failed to read controller-generated encryption Secret")

	var obj unstructured.Unstructured
	Expect(json.Unmarshal([]byte(output), &obj)).To(Succeed())

	data, found, dataErr := unstructured.NestedStringMap(obj.Object, "data")
	Expect(dataErr).NotTo(HaveOccurred(), "failed to parse controller encryption Secret data")
	Expect(found).To(BeTrue(), "controller encryption Secret data not found")

	key := findFirstAgeKeySecretEntry(data)
	Expect(key).NotTo(BeEmpty(), "expected controller encryption Secret to contain a *.agekey entry")

	decodedKey, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(data[key]))
	Expect(decodeErr).NotTo(HaveOccurred(), "failed to decode controller-generated age key")

	return strings.TrimSpace(string(decodedKey))
}

func findFirstAgeKeySecretEntry(data map[string]string) string {
	for key := range data {
		if strings.HasSuffix(strings.TrimSpace(key), ".agekey") {
			return key
		}
	}
	return ""
}

func (r biDirectionalRun) waitForCommittedOrderSpec(name, container, flavor, topping string) {
	Eventually(func(g Gomega) {
		g.Expect(r.gitPull()).To(Succeed())

		content, err := os.ReadFile(r.liveOrderPath(name))
		g.Expect(err).NotTo(HaveOccurred())

		var obj unstructured.Unstructured
		g.Expect(yaml.Unmarshal(content, &obj.Object)).To(Succeed())

		value, found, containerErr := unstructured.NestedString(obj.Object, "spec", "container")
		g.Expect(containerErr).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(value).To(Equal(container))

		scoops, found, scoopsErr := unstructured.NestedSlice(obj.Object, "spec", "scoops")
		g.Expect(scoopsErr).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(scoops).NotTo(BeEmpty())
		firstScoop, ok := scoops[0].(map[string]interface{})
		g.Expect(ok).To(BeTrue())
		g.Expect(firstScoop["flavor"]).To(Equal(flavor))

		toppings, found, toppingsErr := unstructured.NestedStringSlice(obj.Object, "spec", "toppings")
		g.Expect(toppingsErr).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(toppings).To(ContainElement(topping))
	}, biEventuallyTimeout, biPollInterval).Should(Succeed())
}

func (r biDirectionalRun) waitForCommittedEncryptedSecret(name, key, expectedValue string) {
	expectedValueB64 := base64.StdEncoding.EncodeToString([]byte(expectedValue))

	Eventually(func(g Gomega) {
		g.Expect(r.gitPull()).To(Succeed())

		content, err := os.ReadFile(r.liveSecretPath(name))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(string(content)).To(ContainSubstring("sops:"))
		g.Expect(string(content)).NotTo(ContainSubstring(expectedValue))
		g.Expect(string(content)).NotTo(ContainSubstring(expectedValueB64))

		ageKey := r.readControllerEncryptionAgeKey()

		decryptedOutput, decryptErr := decryptWithControllerSOPS(content, ageKey)
		g.Expect(decryptErr).NotTo(HaveOccurred())
		g.Expect(decryptedOutput).To(ContainSubstring(fmt.Sprintf("%s: %s", key, expectedValueB64)))
	}, biEventuallyTimeout, biPollInterval).Should(Succeed())
}

func (r biDirectionalRun) waitForCommittedOrderDeleted(name string) {
	Eventually(func(g Gomega) {
		g.Expect(r.gitPull()).To(Succeed())

		_, err := os.Stat(r.liveOrderPath(name))
		g.Expect(err).To(HaveOccurred())
		g.Expect(os.IsNotExist(err)).To(BeTrue())
	}, biEventuallyTimeout, biPollInterval).Should(Succeed())
}

func (r biDirectionalRun) waitForSecretValue(name, key, expectedValue string) {
	expectedValueB64 := base64.StdEncoding.EncodeToString([]byte(expectedValue))

	Eventually(func(g Gomega) {
		output, err := kubectlRunInNamespace(r.testNs, "get", "secret", name, "-o", "json")
		g.Expect(err).NotTo(HaveOccurred())

		var obj unstructured.Unstructured
		g.Expect(json.Unmarshal([]byte(output), &obj)).To(Succeed())

		data, found, dataErr := unstructured.NestedStringMap(obj.Object, "data")
		g.Expect(dataErr).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(data).To(HaveKeyWithValue(key, expectedValueB64))
	}, biEventuallyTimeout, biPollInterval).Should(Succeed())
}

func (r biDirectionalRun) waitForOrderDeleted(name string) {
	Eventually(func(g Gomega) {
		_, err := kubectlRunInNamespace(r.testNs, "get", "icecreamorder", name, "-o", "json")
		g.Expect(err).To(HaveOccurred())
		g.Expect(strings.ToLower(err.Error())).To(ContainSubstring("not found"))
	}, biEventuallyTimeout, biPollInterval).Should(Succeed())
}

func (r biDirectionalRun) cleanupFluxResources() {
	_, _ = kubectlRunInNamespace(
		"flux-system",
		"delete",
		"kustomization",
		r.fluxLiveName,
		"--ignore-not-found=true",
	)
	_, _ = kubectlRunInNamespace(
		"flux-system",
		"delete",
		"kustomization",
		r.fluxCRDsName,
		"--ignore-not-found=true",
	)
	_, _ = kubectlRunInNamespace(
		"flux-system",
		"delete",
		"gitrepository",
		r.fluxGitRepositoryName,
		"--ignore-not-found=true",
	)
	_, _ = kubectlRunInNamespace(
		"flux-system",
		"delete",
		"secret",
		r.fluxSecretName,
		"--ignore-not-found=true",
	)
	_, _ = kubectlRunInNamespace(
		"flux-system",
		"delete",
		"secret",
		r.fluxDecryptionSecret,
		"--ignore-not-found=true",
	)
}

func (r biDirectionalRun) cleanupReverseResources(ns string) {
	cleanupWatchRule(r.watchRuleName, ns)
	cleanupWatchRule(r.secretWatchRuleName, ns)
	cleanupGitTarget(r.gitTargetName, ns)
	_, _ = kubectlRunInNamespace(
		ns,
		"delete",
		"gitprovider",
		r.gitProviderName,
		"--ignore-not-found=true",
	)
}
