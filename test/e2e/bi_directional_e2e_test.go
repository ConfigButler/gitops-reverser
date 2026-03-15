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
)

const biDirectionalEnabledEnv = "E2E_ENABLE_BI_DIRECTIONAL"

type biDirectionalRun struct {
	testID                string
	repoName              string
	checkoutDir           string
	repoURL               string
	localGitRepoURL       string
	fluxSecretName        string
	fluxGitRepositoryName string
	fluxCRDsName          string
	fluxLiveName          string
	gitProviderName       string
	gitTargetName         string
	watchRuleName         string
	livePath              string
	crdPath               string
	firstOrderName        string
	secondOrderName       string
	reverseContainer      string
	reverseFlavor         string
	reverseTopping        string
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

	BeforeAll(func() {
		if !biDirectionalEnabled() {
			Skip(fmt.Sprintf("bi-directional e2e is disabled; set %s=true to run", biDirectionalEnabledEnv))
		}

		run = newBiDirectionalRun()
		run.assertCheckoutReady()
	})

	AfterAll(func() {
		run.cleanupFluxResources()
		run.cleanupReverseResources()
		_, _ = kubectlRunInNamespace(
			namespace,
			"delete",
			"icecreamorder",
			run.firstOrderName,
			"--ignore-not-found=true",
		)
		_, _ = kubectlRunInNamespace(
			namespace,
			"delete",
			"icecreamorder",
			run.secondOrderName,
			"--ignore-not-found=true",
		)
		_, _ = kubectlRun("delete", "crd", "icecreamorders.shop.example.com", "--ignore-not-found=true")
	})

	It("should avoid a commit loop while Flux and gitops-reverser share IceCreamOrder resources", func() {
		baselineCommitCount, err := run.gitCommitCount()
		Expect(err).NotTo(HaveOccurred(), "failed to capture baseline git commit count")

		By("installing the IceCreamOrder CRD before the GitOps flow starts")
		_, err = kubectlRun("apply", "-f", "test/e2e/templates/icecreamorder-crd.yaml")
		Expect(err).NotTo(HaveOccurred(), "failed to install IceCreamOrder CRD")
		run.waitForCRDEstablished()

		By("creating the initial GitOps commit with the first IceCreamOrder")
		run.writeCRDToRepo()
		run.writeLiveOrder(iceCreamOrderFile{
			Name:         run.firstOrderName,
			Namespace:    namespace,
			CustomerName: "Alice",
			Container:    "Cone",
			Scoops: []iceCreamScoop{
				{Flavor: "Vanilla", Quantity: 2},
			},
			Toppings: []string{"Sprinkles"},
		})
		Expect(run.commitAllAndPush("bi-directional: bootstrap first icecream order")).To(Succeed())
		run.expectRemoteCommitCount(baselineCommitCount + 1)

		By("configuring gitops-reverser to watch the same IceCreamOrder path before Flux syncs")
		createGitProviderWithURLInNamespace(run.gitProviderName, namespace, "main", e2eGitSecretHTTP(), run.repoURL)
		createGitTarget(run.gitTargetName, namespace, run.gitProviderName, run.livePath, "main")
		err = applyFromTemplate("test/e2e/templates/watchrule-crd.tmpl", struct {
			Name            string
			Namespace       string
			DestinationName string
		}{
			Name:            run.watchRuleName,
			Namespace:       namespace,
			DestinationName: run.gitTargetName,
		}, namespace)
		Expect(err).NotTo(HaveOccurred(), "failed to apply bi-directional WatchRule")
		verifyResourceStatus("gitprovider", run.gitProviderName, namespace, "True", "Ready", "")
		verifyResourceStatus("gittarget", run.gitTargetName, namespace, "True", "Ready", "")
		verifyResourceStatus("watchrule", run.watchRuleName, namespace, "True", "Ready", "")

		By("configuring Flux to sync the same repository and live path")
		run.applyFluxGitRepository()
		run.applyFluxKustomizations()

		By("verifying the initial GitOps sync succeeds without reverse-commit churn")
		run.waitForFluxGitRepositoryRevision(run.gitHEAD())
		run.waitForFluxKustomizationRevision(run.fluxCRDsName, run.gitHEAD())
		run.waitForFluxKustomizationRevision(run.fluxLiveName, run.gitHEAD())
		run.waitForOrderSpec(run.firstOrderName, "Cone", "Vanilla", "Sprinkles")
		run.consistentlyExpectRemoteCommitCount(baselineCommitCount+1, 20*time.Second)

		By("creating a second normal GitOps commit with an update and a new order")
		run.writeLiveOrder(iceCreamOrderFile{
			Name:         run.firstOrderName,
			Namespace:    namespace,
			CustomerName: "Alice",
			Container:    "WaffleBowl",
			Scoops: []iceCreamScoop{
				{Flavor: "Chocolate", Quantity: 3},
			},
			Toppings: []string{"CookieCrumbs"},
		})
		run.writeLiveOrder(iceCreamOrderFile{
			Name:         run.secondOrderName,
			Namespace:    namespace,
			CustomerName: "Bob",
			Container:    "Cup",
			Scoops: []iceCreamScoop{
				{Flavor: "Strawberry", Quantity: 1},
			},
			Toppings: []string{"WhippedCream"},
		})
		Expect(run.commitAllAndPush("bi-directional: update first order and add second")).To(Succeed())
		secondHead := run.gitHEAD()
		run.waitForFluxGitRepositoryRevision(secondHead)
		run.waitForFluxKustomizationRevision(run.fluxLiveName, secondHead)
		run.waitForOrderSpec(run.firstOrderName, "WaffleBowl", "Chocolate", "CookieCrumbs")
		run.waitForOrderSpec(run.secondOrderName, "Cup", "Strawberry", "WhippedCream")
		run.consistentlyExpectRemoteCommitCount(baselineCommitCount+2, 20*time.Second)

		By("changing one IceCreamOrder through the Kubernetes API")
		_, err = kubectlRunInNamespace(
			namespace,
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
			count, countErr := run.gitCommitCount()
			g.Expect(countErr).NotTo(HaveOccurred())
			g.Expect(count).To(Equal(baselineCommitCount + 3))

			content, readErr := os.ReadFile(run.liveOrderPath(run.firstOrderName))
			g.Expect(readErr).NotTo(HaveOccurred())
			g.Expect(string(content)).To(ContainSubstring("container: " + run.reverseContainer))
			g.Expect(string(content)).To(ContainSubstring("flavor: " + run.reverseFlavor))
			g.Expect(string(content)).To(ContainSubstring("- " + run.reverseTopping))
		}, 90*time.Second, 2*time.Second).Should(Succeed())

		thirdHead := run.gitHEAD()
		run.waitForFluxGitRepositoryRevision(thirdHead)
		run.waitForFluxKustomizationRevision(run.fluxLiveName, thirdHead)
		run.waitForOrderSpec(run.firstOrderName, run.reverseContainer, run.reverseFlavor, run.reverseTopping)
		run.consistentlyExpectRemoteCommitCount(baselineCommitCount+3, 25*time.Second)
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
		testID:                testID,
		repoName:              repoName,
		checkoutDir:           checkoutDir,
		repoURL:               fmt.Sprintf(giteaRepoURLTemplate, repoName),
		localGitRepoURL:       fmt.Sprintf("http://localhost:13000/testorg/%s.git", repoName),
		fluxSecretName:        fmt.Sprintf("bi-flux-auth-%s", testID),
		fluxGitRepositoryName: fmt.Sprintf("bi-repo-%s", testID),
		fluxCRDsName:          fmt.Sprintf("bi-crds-%s", testID),
		fluxLiveName:          fmt.Sprintf("bi-live-%s", testID),
		gitProviderName:       fmt.Sprintf("bi-provider-%s", testID),
		gitTargetName:         fmt.Sprintf("bi-target-%s", testID),
		watchRuleName:         fmt.Sprintf("bi-watchrule-%s", testID),
		livePath:              fmt.Sprintf("bi-directional/%s/live", testID),
		crdPath:               fmt.Sprintf("bi-directional/%s/crds", testID),
		firstOrderName:        fmt.Sprintf("bi-alice-order-%s", testID),
		secondOrderName:       fmt.Sprintf("bi-bob-order-%s", testID),
		reverseContainer:      "BananaSplitBoat",
		reverseFlavor:         "MintChip",
		reverseTopping:        "Caramel",
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
	}, 30*time.Second, time.Second).Should(Succeed())
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
		namespace,
		name+".yaml",
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

func (r biDirectionalRun) gitPull() error {
	return r.runGit("pull", "--ff-only")
}

func (r biDirectionalRun) gitCommitCount() (int, error) {
	cmd := exec.Command("git", "rev-list", "--count", "--all")
	cmd.Dir = r.checkoutDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("git rev-list --count: %w: %s", err, strings.TrimSpace(string(output)))
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
		count, err := r.gitCommitCount()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(count).To(Equal(expected))
	}, 30*time.Second, 2*time.Second).Should(Succeed())
}

func (r biDirectionalRun) consistentlyExpectRemoteCommitCount(expected int, duration time.Duration) {
	Consistently(func(g Gomega) {
		g.Expect(r.gitPull()).To(Succeed())
		count, err := r.gitCommitCount()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(count).To(Equal(expected))
	}, duration, 2*time.Second).Should(Succeed())
}

func (r biDirectionalRun) applyFluxGitRepository() {
	username, password := r.readGitCredentialSecretDataBase64()
	err := applyFromTemplate("test/e2e/templates/flux-gitrepository-http.tmpl", struct {
		Namespace  string
		SecretName string
		Name       string
		RepoURL    string
		Branch     string
		Username   string
		Password   string
	}{
		Namespace:  "flux-system",
		SecretName: r.fluxSecretName,
		Name:       r.fluxGitRepositoryName,
		RepoURL:    r.repoURL,
		Branch:     "main",
		Username:   username,
		Password:   password,
	}, "flux-system")
	Expect(err).NotTo(HaveOccurred(), "failed to apply Flux GitRepository")
}

func (r biDirectionalRun) applyFluxKustomizations() {
	err := applyFromTemplate("test/e2e/templates/flux-kustomization.tmpl", struct {
		Namespace   string
		Name        string
		Path        string
		SourceName  string
		DependsOn   string
		Prune       bool
		Wait        bool
		TargetNS    string
		HasTargetNS bool
	}{
		Namespace:  "flux-system",
		Name:       r.fluxCRDsName,
		Path:       "./" + r.crdPath,
		SourceName: r.fluxGitRepositoryName,
		Prune:      true,
		Wait:       true,
	}, "flux-system")
	Expect(err).NotTo(HaveOccurred(), "failed to apply Flux CRD Kustomization")

	err = applyFromTemplate("test/e2e/templates/flux-kustomization.tmpl", struct {
		Namespace   string
		Name        string
		Path        string
		SourceName  string
		DependsOn   string
		Prune       bool
		Wait        bool
		TargetNS    string
		HasTargetNS bool
	}{
		Namespace:  "flux-system",
		Name:       r.fluxLiveName,
		Path:       "./" + r.livePath,
		SourceName: r.fluxGitRepositoryName,
		DependsOn:  r.fluxCRDsName,
		Prune:      true,
		Wait:       true,
	}, "flux-system")
	Expect(err).NotTo(HaveOccurred(), "failed to apply Flux live Kustomization")
}

func (r biDirectionalRun) readGitCredentialSecretDataBase64() (string, string) {
	output, err := kubectlRunInNamespace(namespace, "get", "secret", e2eGitSecretHTTP(), "-o", "json")
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
	}, 90*time.Second, 2*time.Second).Should(Succeed())
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
	}, 90*time.Second, 2*time.Second).Should(Succeed())
}

func (r biDirectionalRun) waitForOrderSpec(name, container, flavor, topping string) {
	Eventually(func(g Gomega) {
		output, err := kubectlRunInNamespace(
			namespace,
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
	}, 90*time.Second, 2*time.Second).Should(Succeed())
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
}

func (r biDirectionalRun) cleanupReverseResources() {
	cleanupWatchRule(r.watchRuleName, namespace)
	cleanupGitTarget(r.gitTargetName, namespace)
	_, _ = kubectlRunInNamespace(
		namespace,
		"delete",
		"gitprovider",
		r.gitProviderName,
		"--ignore-not-found=true",
	)
}
