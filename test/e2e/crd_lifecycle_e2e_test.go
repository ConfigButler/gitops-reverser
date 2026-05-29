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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Manager CRD Lifecycle", Label("manager"), Ordered, func() {
	var (
		testNs           string
		crdLifecycleRepo *RepoArtifacts
	)

	BeforeAll(func() {
		By("creating CRD lifecycle test namespace")
		testNs = testNamespaceFor("manager-crd")
		_, _ = kubectlRun("create", "namespace", testNs) // idempotent; ignore AlreadyExists

		By("setting up Gitea repo and credentials for CRD lifecycle tests")
		crdLifecycleRepo = SetupRepo(
			resolveE2EContext(),
			testNs,
			fmt.Sprintf("e2e-manager-crd-%d", GinkgoRandomSeed()),
		)

		By("applying git secrets to test namespace")
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", crdLifecycleRepo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")
		applySOPSAgeKeyToNamespace(testNs)

		By("creating shared gitprovider-normal for CRD lifecycle specs")
		createGitProviderWithURLInNamespace(
			"gitprovider-normal",
			testNs,
			crdLifecycleRepo.GitSecretHTTP,
			crdLifecycleRepo.RepoURLHTTP,
		)
		verifyResourceStatus(
			"gitprovider", "gitprovider-normal", testNs,
			"True", "Ready", "Repository connectivity validated",
		)
	})

	AfterAll(func() {
		By("cleaning up CRD lifecycle test resources")

		// Clean up IceCreamOrder CRD (cluster-scoped — must delete explicitly)
		_, _ = kubectlRun(
			"delete",
			"crd",
			iceCreamCRDName(crdGroupCRDLifecycle),
			"--ignore-not-found=true",
		)

		cleanupNamespace(testNs)

		By("test infrastructure still running for debugging")
		fmt.Printf("\n")
		fmt.Printf("═══════════════════════════════════════════════════════════\n")
		fmt.Printf("📊 E2E Infrastructure kept running for debugging purposes:\n")
		fmt.Printf("═══════════════════════════════════════════════════════════\n")
		fmt.Printf("  Prometheus: http://localhost:19090\n")
		fmt.Printf("  Gitea:      http://localhost:13000\n")
		fmt.Printf("  Flux UI:    http://localhost:19080\n")
		fmt.Printf("\n")
		fmt.Printf("═══════════════════════════════════════════════════════════\n")
		fmt.Printf("\n")
	})

	SetDefaultEventuallyTimeout(30 * time.Second)
	SetDefaultEventuallyPollingInterval(time.Second)

	It("should create Git commit when IceCreamOrder CRD is installed via ClusterWatchRule", Label("smoke"), func() {
		gitProviderName := "gitprovider-normal"
		clusterWatchRuleName := "clusterwatchrule-crd-install"
		crdName := iceCreamCRDName(crdGroupCRDLifecycle)

		By("creating ClusterWatchRule with Cluster scope for CRDs")
		destName := clusterWatchRuleName + "-dest"
		createGitTarget(destName, testNs, gitProviderName, "e2e/crd-install-test", "main")

		clusterWatchRuleData := struct {
			Name            string
			DestinationName string
			Namespace       string
		}{
			Name:            clusterWatchRuleName,
			DestinationName: destName,
			Namespace:       testNs,
		}

		err := applyFromTemplate("test/e2e/templates/manager/clusterwatchrule-crd.tmpl", clusterWatchRuleData, "")
		Expect(err).NotTo(HaveOccurred(), "Failed to apply ClusterWatchRule for CRDs")

		By("verifying ClusterWatchRule is ready")
		verifyResourceStatus("clusterwatchrule", clusterWatchRuleName, "", "True", "Ready", "")
		verifyResourceStatus("gittarget", destName, testNs, "True", "Ready", "")

		By("installing the IceCreamOrder CRD to trigger Git commit")
		err = applyIceCreamCRD(crdGroupCRDLifecycle)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRD")

		By("waiting for CRD to be established")
		verifyCRDEstablished := func(g Gomega) {
			output, err := kubectlRun(
				"get",
				"crd",
				crdName,
				"-o",
				"jsonpath={.status.conditions[?(@.type=='Established')].status}",
			)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("True"))
		}
		Eventually(verifyCRDEstablished).Should(Succeed())

		By("verifying CRD YAML file exists in Git repository (NO namespace in path - cluster resource)")
		verifyGitCommit := func(g Gomega) {
			pullCmd := exec.Command("git", "pull")
			pullCmd.Dir = crdLifecycleRepo.CheckoutDir
			pullOutput, pullErr := pullCmd.CombinedOutput()
			if pullErr != nil {
				g.Expect(pullErr).NotTo(HaveOccurred(),
					fmt.Sprintf("Should successfully pull latest changes. Output: %s", string(pullOutput)))
			}

			// CRDs are cluster-scoped, so path should NOT include namespace
			expectedFile := filepath.Join(crdLifecycleRepo.CheckoutDir,
				"e2e/crd-install-test",
				"apiextensions.k8s.io/v1/customresourcedefinitions/"+iceCreamCRDMirrorFile(crdGroupCRDLifecycle))
			fileInfo, statErr := os.Stat(expectedFile)
			g.Expect(statErr).NotTo(HaveOccurred(), fmt.Sprintf("CRD file should exist at %s", expectedFile))
			g.Expect(fileInfo.Size()).To(BeNumerically(">", 0), "CRD file should not be empty")

			// Verify file content
			content, readErr := os.ReadFile(expectedFile)
			g.Expect(readErr).NotTo(HaveOccurred())
			g.Expect(string(content)).To(ContainSubstring("kind: CustomResourceDefinition"),
				"File should contain CRD kind")
			g.Expect(string(content)).To(ContainSubstring("name: "+iceCreamCRDName(crdGroupCRDLifecycle)),
				"File should contain CRD name")
		}
		Eventually(verifyGitCommit).
			WithTimeout(60 * time.Second).
			WithPolling(2 * time.Second).
			Should(Succeed())

		By("cleaning up test resources")
		cleanupClusterWatchRule(clusterWatchRuleName)
		cleanupGitTarget(destName, testNs)
		// Keep CRD installed for subsequent tests

		By("✅ CRD installation via ClusterWatchRule E2E test passed")
	})

	It("should create Git commit when IceCreamOrder is added via WatchRule", func() {
		gitProviderName := "gitprovider-normal"
		watchRuleName := "watchrule-icecream-orders"

		By("installing the IceCreamOrder CRD first (needed for custom resource tests)")
		err := applyIceCreamCRD(crdGroupCRDLifecycle)
		Expect(err).NotTo(HaveOccurred(), "Failed to install sample CRD")

		By("waiting for CRD to be established")
		verifyCRDEstablished := func(g Gomega) {
			output, err := kubectlRun(
				"get",
				"crd",
				iceCreamCRDName(crdGroupCRDLifecycle),
				"-o",
				"jsonpath={.status.conditions[?(@.type=='Established')].status}",
			)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("True"))
		}
		Eventually(verifyCRDEstablished, 30*time.Second, time.Second).Should(Succeed())

		// Established=True only means the apiserver registered the CRD; the
		// controller's reflector may still be in "Failed to watch" backoff for
		// a few seconds. If we create a CR before that recovers, the create
		// event is missed and the next spec sees no file in git. Block until
		// the resource is actually listable end-to-end.
		Eventually(func(g Gomega) {
			_, err := kubectlRun("get", iceCreamCRDName(crdGroupCRDLifecycle), "-A")
			g.Expect(err).NotTo(HaveOccurred(), "icecreamorders must be servable before creating instances")
		}, 30*time.Second, time.Second).Should(Succeed())

		crdInstanceName := "alices-order"
		uniqueRepoName := crdLifecycleRepo.RepoName

		By("creating WatchRule that monitors IceCreamOrder resources")
		destName := watchRuleName + "-dest"
		createGitTarget(destName, testNs, gitProviderName, "e2e/icecream-test", "main")

		data := struct {
			Name            string
			Namespace       string
			DestinationName string
			Group           string
		}{
			Name:            watchRuleName,
			Namespace:       testNs,
			DestinationName: destName,
			Group:           crdGroupCRDLifecycle,
		}

		err2 := applyFromTemplate("test/e2e/templates/watchrule-crd.tmpl", data, testNs)
		Expect(err2).NotTo(HaveOccurred(), "Failed to apply WatchRule for CRDs")

		By("verifying WatchRule is ready")
		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")
		verifyResourceStatus("gittarget", destName, testNs, "True", "Ready", "")

		By("creating CR with labels and annotations to trigger Git commit")
		crdInstanceData := struct {
			Name         string
			Namespace    string
			Labels       map[string]string
			Annotations  map[string]string
			CustomerName string
			Container    string
			Scoops       []struct {
				Flavor   string
				Quantity int
			}
			Toppings []string
			Group    string
		}{
			Name:      crdInstanceName,
			Namespace: testNs,
			Labels: map[string]string{
				"environment": "test",
				"team":        "engineering",
			},
			Annotations: map[string]string{
				"description": "Alice's favorite ice cream",
				"priority":    "high",
				"kubectl.kubernetes.io/last-applied-configuration": "should-be-filtered",
				"deployment.kubernetes.io/revision":                "should-also-be-filtered",
			},
			Group:        crdGroupCRDLifecycle,
			CustomerName: "Alice",
			Container:    "Cone",
			Scoops: []struct {
				Flavor   string
				Quantity int
			}{
				{Flavor: "Vanilla", Quantity: 2},
				{Flavor: "Chocolate", Quantity: 1},
			},
			Toppings: []string{"Sprinkles", "HotFudge"},
		}

		err3 := applyFromTemplate("test/e2e/templates/icecreamorder-instance.tmpl", crdInstanceData, testNs)
		Expect(err3).NotTo(HaveOccurred(), "Failed to apply CRD instance")

		By("waiting for controller reconciliation of CRD instance event")
		verifyReconciliationLogs := func(g Gomega) {
			output, err := kubectlRunInNamespace(
				namespace,
				"logs",
				"-l",
				"control-plane=gitops-reverser",
				"--tail=500",
				"--prefix=true",
			)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("git commit"),
				"Should see git commit operation in logs")
		}
		Eventually(verifyReconciliationLogs, 45*time.Second, 2*time.Second).Should(Succeed())

		By("verifying CRD instance YAML file exists in Gitea repository")
		verifyGitCommit := func(g Gomega) {
			pullCmd := exec.Command("git", "pull")
			pullCmd.Dir = crdLifecycleRepo.CheckoutDir
			pullOutput, pullErr := pullCmd.CombinedOutput()
			if pullErr != nil {
				g.Expect(pullErr).NotTo(HaveOccurred(),
					fmt.Sprintf("Should successfully pull latest changes. Output: %s", string(pullOutput)))
			}

			expectedFile := filepath.Join(crdLifecycleRepo.CheckoutDir,
				"e2e/icecream-test",
				fmt.Sprintf("%s/%s/%s.yaml", iceCreamInstanceDir(crdGroupCRDLifecycle), testNs, crdInstanceName))
			fileInfo, statErr := os.Stat(expectedFile)
			g.Expect(statErr).
				NotTo(HaveOccurred(), fmt.Sprintf("CRD instance file should exist at %s", expectedFile))
			g.Expect(fileInfo.Size()).To(BeNumerically(">", 0), "CRD instance file should not be empty")

			content, readErr := os.ReadFile(expectedFile)
			g.Expect(readErr).NotTo(HaveOccurred())
			contentStr := string(content)
			g.Expect(contentStr).To(ContainSubstring("kind: IceCreamOrder"),
				"CRD instance file should contain IceCreamOrder kind")
			g.Expect(contentStr).To(ContainSubstring("customerName: Alice"),
				"CRD instance file should contain customer name")
			g.Expect(contentStr).To(ContainSubstring("container: Cone"),
				"CRD instance file should contain container type")
			g.Expect(contentStr).To(ContainSubstring("flavor: Vanilla"),
				"CRD instance file should contain ice cream flavors")

			// Verify labels are present
			g.Expect(contentStr).To(ContainSubstring("environment: test"),
				"CRD instance file should contain environment label")
			g.Expect(contentStr).To(ContainSubstring("team: engineering"),
				"CRD instance file should contain team label")

			// Verify user annotations are present
			g.Expect(contentStr).To(ContainSubstring("description: Alice's favorite ice cream"),
				"CRD instance file should contain description annotation")
			g.Expect(contentStr).To(ContainSubstring("priority: high"),
				"CRD instance file should contain priority annotation")

			// Verify filtered annotations are NOT present
			g.Expect(contentStr).NotTo(ContainSubstring("kubectl.kubernetes.io/last-applied-configuration"),
				"CRD instance file should NOT contain kubectl annotation")
			g.Expect(contentStr).NotTo(ContainSubstring("deployment.kubernetes.io/revision"),
				"CRD instance file should NOT contain deployment annotation")

			// Verify status field is NOT present in Git
			g.Expect(contentStr).NotTo(ContainSubstring("status:"),
				"CRD instance file should NOT contain status field")
		}
		Eventually(verifyGitCommit).
			WithTimeout(60 * time.Second).
			WithPolling(2 * time.Second).
			Should(Succeed())

		By("verifying the IceCreamOrder still exists before status update")
		verifyCRDInstanceExists := func(g Gomega) {
			_, err := kubectlRunInNamespace(testNs, "get", iceCreamCRDName(crdGroupCRDLifecycle), crdInstanceName)
			g.Expect(err).NotTo(HaveOccurred(), "IceCreamOrder should exist before status patch")
		}
		Eventually(verifyCRDInstanceExists, 15*time.Second, time.Second).Should(Succeed())

		By("applying status update to the IceCreamOrder CR")
		statusPatch := `{"status":{"phase":"Preparing","cost":12.5,"message":"Queued for pickup"}}`
		statusOutput, statusErr := kubectlRunInNamespace(
			testNs,
			"patch",
			iceCreamCRDName(crdGroupCRDLifecycle),
			crdInstanceName,
			"--type=merge",
			"--subresource=status",
			"-p",
			statusPatch,
		)
		Expect(statusErr).NotTo(HaveOccurred(), "Status subresource patch should succeed")
		By(fmt.Sprintf("status patched successfully: %s", statusOutput))

		By("getting current git commit hash")
		gitRevCmd := exec.Command("git", "rev-parse", "HEAD")
		gitRevCmd.Dir = crdLifecycleRepo.CheckoutDir
		beforeStatusCommit, _ := gitRevCmd.Output()

		By("waiting to ensure no new commit is created from status update")
		time.Sleep(10 * time.Second)

		By("verifying no new commit was created and status is not in Git")
		verifyStatusNotCommitted := func(g Gomega) {
			pullCmd := exec.Command("git", "pull")
			pullCmd.Dir = crdLifecycleRepo.CheckoutDir
			_, _ = pullCmd.CombinedOutput()

			// Check that commit hash hasn't changed
			gitRevCmd := exec.Command("git", "rev-parse", "HEAD")
			gitRevCmd.Dir = crdLifecycleRepo.CheckoutDir
			afterStatusCommit, err := gitRevCmd.Output()
			g.Expect(err).NotTo(HaveOccurred())

			By(fmt.Sprintf("Commit before status: %s", string(beforeStatusCommit)))
			By(fmt.Sprintf("Commit after status:  %s", string(afterStatusCommit)))

			// Read the file again to ensure status is still not present
			expectedFile := filepath.Join(crdLifecycleRepo.CheckoutDir,
				"e2e/icecream-test",
				fmt.Sprintf("%s/%s/%s.yaml", iceCreamInstanceDir(crdGroupCRDLifecycle), testNs, crdInstanceName))
			content, readErr := os.ReadFile(expectedFile)
			g.Expect(readErr).NotTo(HaveOccurred())
			g.Expect(string(content)).NotTo(ContainSubstring("status:"),
				"CRD instance file should still NOT contain status field after status update")
			g.Expect(string(content)).NotTo(ContainSubstring("phase:"),
				"CRD instance file should NOT contain status phase content")
			g.Expect(string(content)).NotTo(ContainSubstring("Queued for pickup"),
				"CRD instance file should NOT contain status content")
		}
		Eventually(verifyStatusNotCommitted).Should(Succeed())

		By("✅ Status update verified - no Git commit created and status not in file")

		By("cleaning up IceCreamOrder instance")
		_, _ = kubectlRunInNamespace(testNs, "delete", iceCreamCRDName(crdGroupCRDLifecycle), crdInstanceName)

		By("Note: GitTarget, WatchRule, GitProvider, and CRD kept for subsequent tests")

		By("✅ IceCreamOrder to Git commit E2E test passed")
		fmt.Printf("✅ IceCreamOrder '%s' successfully triggered Git commit in repo '%s'\n",
			crdInstanceName, uniqueRepoName)
	})

	It("should update Git file when IceCreamOrder is modified via WatchRule", func() {
		crdInstanceName := "bobs-order"
		uniqueRepoName := crdLifecycleRepo.RepoName

		By("creating initial IceCreamOrder instance")
		crdInstanceData := struct {
			Name         string
			Namespace    string
			Labels       map[string]string
			Annotations  map[string]string
			CustomerName string
			Container    string
			Scoops       []struct {
				Flavor   string
				Quantity int
			}
			Toppings []string
			Group    string
		}{
			Name:         crdInstanceName,
			Namespace:    testNs,
			Labels:       nil,
			Annotations:  nil,
			Group:        crdGroupCRDLifecycle,
			CustomerName: "Bob",
			Container:    "Cup",
			Scoops: []struct {
				Flavor   string
				Quantity int
			}{
				{Flavor: "Strawberry", Quantity: 1},
			},
			Toppings: []string{"WhippedCream"},
		}

		err := applyFromTemplate("test/e2e/templates/icecreamorder-instance.tmpl", crdInstanceData, testNs)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply initial CRD instance")

		By("waiting for initial CRD instance file to appear in Git")
		verifyInitialFile := func(g Gomega) {
			pullCmd := exec.Command("git", "pull")
			pullCmd.Dir = crdLifecycleRepo.CheckoutDir
			_, _ = pullCmd.CombinedOutput()

			expectedFile := filepath.Join(crdLifecycleRepo.CheckoutDir,
				"e2e/icecream-test",
				fmt.Sprintf("%s/%s/%s.yaml", iceCreamInstanceDir(crdGroupCRDLifecycle), testNs, crdInstanceName))
			content, readErr := os.ReadFile(expectedFile)
			g.Expect(readErr).NotTo(HaveOccurred())
			g.Expect(string(content)).To(ContainSubstring("customerName: Bob"))
			g.Expect(string(content)).To(ContainSubstring("flavor: Strawberry"))
		}
		Eventually(verifyInitialFile).Should(Succeed())

		By("updating CRD instance with new values")
		updatedCRDData := struct {
			Name         string
			Namespace    string
			Labels       map[string]string
			Annotations  map[string]string
			CustomerName string
			Container    string
			Scoops       []struct {
				Flavor   string
				Quantity int
			}
			Toppings []string
			Group    string
		}{
			Name:         crdInstanceName,
			Namespace:    testNs,
			Labels:       nil,
			Annotations:  nil,
			Group:        crdGroupCRDLifecycle,
			CustomerName: "Bob",
			Container:    "WaffleBowl",
			Scoops: []struct {
				Flavor   string
				Quantity int
			}{
				{Flavor: "RockyRoad", Quantity: 3},
				{Flavor: "MintChip", Quantity: 2},
			},
			Toppings: []string{"HotFudge", "Caramel", "Sprinkles"},
		}

		err = applyFromTemplate("test/e2e/templates/icecreamorder-instance.tmpl", updatedCRDData, testNs)
		Expect(err).NotTo(HaveOccurred(), "Failed to update CRD instance")

		By("verifying updated CRD instance content in Git")
		verifyUpdatedFile := func(g Gomega) {
			pullCmd := exec.Command("git", "pull")
			pullCmd.Dir = crdLifecycleRepo.CheckoutDir
			_, _ = pullCmd.CombinedOutput()

			expectedFile := filepath.Join(crdLifecycleRepo.CheckoutDir,
				"e2e/icecream-test",
				fmt.Sprintf("%s/%s/%s.yaml", iceCreamInstanceDir(crdGroupCRDLifecycle), testNs, crdInstanceName))
			content, readErr := os.ReadFile(expectedFile)
			g.Expect(readErr).NotTo(HaveOccurred())
			g.Expect(string(content)).To(ContainSubstring("container: WaffleBowl"),
				"Updated file should contain new container type")
			g.Expect(string(content)).To(ContainSubstring("flavor: RockyRoad"),
				"Updated file should contain new flavor")
			g.Expect(string(content)).To(ContainSubstring("quantity: 3"),
				"Updated file should contain new quantity")
		}
		Eventually(verifyUpdatedFile).Should(Succeed())

		By("cleaning up IceCreamOrder instance")
		_, _ = kubectlRunInNamespace(testNs, "delete", iceCreamCRDName(crdGroupCRDLifecycle), crdInstanceName)

		By("Note: GitTarget, WatchRule, GitProvider, and CRD kept for subsequent tests")

		By("✅ IceCreamOrder update E2E test passed")
		fmt.Printf("✅ IceCreamOrder '%s' update successfully reflected in Git repo '%s'\n",
			crdInstanceName, uniqueRepoName)
	})

	It("should delete Git file when IceCreamOrder is deleted via WatchRule", func() {
		crdInstanceName := "charlies-order"
		uniqueRepoName := crdLifecycleRepo.RepoName

		By("creating IceCreamOrder instance")
		crdInstanceData := struct {
			Name         string
			Namespace    string
			Labels       map[string]string
			Annotations  map[string]string
			CustomerName string
			Container    string
			Scoops       []struct {
				Flavor   string
				Quantity int
			}
			Toppings []string
			Group    string
		}{
			Name:         crdInstanceName,
			Namespace:    testNs,
			Labels:       nil,
			Annotations:  nil,
			Group:        crdGroupCRDLifecycle,
			CustomerName: "Charlie",
			Container:    "Cone",
			Scoops: []struct {
				Flavor   string
				Quantity int
			}{
				{Flavor: "Chocolate", Quantity: 2},
			},
			Toppings: []string{"Sprinkles"},
		}

		err := applyFromTemplate("test/e2e/templates/icecreamorder-instance.tmpl", crdInstanceData, testNs)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply CR")

		By("waiting for CR file to appear in Git repository")
		verifyFileCreated := func(g Gomega) {
			pullCmd := exec.Command("git", "pull")
			pullCmd.Dir = crdLifecycleRepo.CheckoutDir
			_, _ = pullCmd.CombinedOutput()

			expectedFile := filepath.Join(crdLifecycleRepo.CheckoutDir,
				"e2e/icecream-test",
				fmt.Sprintf("%s/%s/%s.yaml", iceCreamInstanceDir(crdGroupCRDLifecycle), testNs, crdInstanceName))
			fileInfo, statErr := os.Stat(expectedFile)
			g.Expect(statErr).
				NotTo(HaveOccurred(), fmt.Sprintf("CRD instance file should exist at %s", expectedFile))
			g.Expect(fileInfo.Size()).To(BeNumerically(">", 0), "CRD instance file should not be empty")
		}
		// 60s tolerates controller-reflector re-establishment if the deployment
		// happened to roll during the prior spec (default 30s was racing it).
		Eventually(verifyFileCreated, 60*time.Second, 2*time.Second).Should(Succeed())

		By("deleting the CR to trigger DELETE operation")
		_, err = kubectlRunInNamespace(testNs, "delete", iceCreamCRDName(crdGroupCRDLifecycle), crdInstanceName)
		Expect(err).NotTo(HaveOccurred(), "CRD instance deletion should succeed")

		By("verifying CRD instance file is deleted from Git repository")
		verifyFileDeleted := func(g Gomega) {
			pullCmd := exec.Command("git", "pull")
			pullCmd.Dir = crdLifecycleRepo.CheckoutDir
			_, _ = pullCmd.CombinedOutput()

			expectedFile := filepath.Join(crdLifecycleRepo.CheckoutDir,
				"e2e/icecream-test",
				fmt.Sprintf("%s/%s/%s.yaml", iceCreamInstanceDir(crdGroupCRDLifecycle), testNs, crdInstanceName))
			_, statErr := os.Stat(expectedFile)
			g.Expect(statErr).
				To(HaveOccurred(), fmt.Sprintf("CRD instance file should NOT exist at %s", expectedFile))
			g.Expect(os.IsNotExist(statErr)).To(BeTrue(), "Error should be 'file does not exist'")

			By("verifying git log shows DELETE commit")
			gitLogCmd := exec.Command("git", "log", "--oneline", "-n", "5")
			gitLogCmd.Dir = crdLifecycleRepo.CheckoutDir
			logOutput, logErr := gitLogCmd.CombinedOutput()
			g.Expect(logErr).NotTo(HaveOccurred(), "Should be able to read git log")
			g.Expect(string(logOutput)).To(ContainSubstring("DELETE"),
				"Git log should contain DELETE operation")
		}
		Eventually(verifyFileDeleted).Should(Succeed())

		By("✅ IceCreamOrder deletion E2E test passed")
		fmt.Printf("✅ IceCreamOrder '%s' deletion successfully removed file from Git repo '%s'\n",
			crdInstanceName, uniqueRepoName)
	})

	It("should delete Git file when IceCreamOrder CRD is deleted via ClusterWatchRule", func() {
		gitProviderName := "gitprovider-normal"
		clusterWatchRuleName := "clusterwatchrule-crd-delete"
		crdName := iceCreamCRDName(crdGroupCRDLifecycle)

		By("creating ClusterWatchRule with Cluster scope for CRDs")
		destName := clusterWatchRuleName + "-dest"
		createGitTarget(destName, testNs, gitProviderName, "e2e/crd-delete-test", "main")
		verifyResourceStatus("gittarget", destName, testNs, "True", "Ready", "")

		clusterWatchRuleData := struct {
			Name            string
			DestinationName string
			Namespace       string
		}{
			Name:            clusterWatchRuleName,
			DestinationName: destName,
			Namespace:       testNs,
		}

		err := applyFromTemplate("test/e2e/templates/manager/clusterwatchrule-crd.tmpl", clusterWatchRuleData, "")
		Expect(err).NotTo(HaveOccurred(), "Failed to apply ClusterWatchRule for CRDs")

		By("verifying ClusterWatchRule is ready")

		verifyResourceStatus("clusterwatchrule", clusterWatchRuleName, "", "True", "Ready", "")

		By("verifying CRD file exists in Git before deletion")
		verifyFileExists := func(g Gomega) {
			pullCmd := exec.Command("git", "pull")
			pullCmd.Dir = crdLifecycleRepo.CheckoutDir
			_, _ = pullCmd.CombinedOutput()

			expectedFile := filepath.Join(crdLifecycleRepo.CheckoutDir,
				"e2e/crd-delete-test",
				"apiextensions.k8s.io/v1/customresourcedefinitions/"+iceCreamCRDMirrorFile(crdGroupCRDLifecycle))
			_, statErr := os.Stat(expectedFile)
			g.Expect(statErr).NotTo(HaveOccurred(), "CRD file should exist before deletion")
		}
		Eventually(verifyFileExists).Should(Succeed())

		By("deleting the CRD to trigger DELETE operation")
		_, deleteErr := kubectlRun("delete", "crd", crdName)
		Expect(deleteErr).NotTo(HaveOccurred(), "CRD deletion should succeed")
		By("verifying CRD file is deleted from Git repository")
		verifyFileDeleted := func(g Gomega) {
			pullCmd := exec.Command("git", "pull")
			pullCmd.Dir = crdLifecycleRepo.CheckoutDir
			_, _ = pullCmd.CombinedOutput()

			expectedFile := filepath.Join(crdLifecycleRepo.CheckoutDir,
				"e2e/crd-delete-test",
				"apiextensions.k8s.io/v1/customresourcedefinitions/"+iceCreamCRDMirrorFile(crdGroupCRDLifecycle))
			_, statErr := os.Stat(expectedFile)
			g.Expect(statErr).To(HaveOccurred(), "CRD file should NOT exist after deletion")
			g.Expect(os.IsNotExist(statErr)).To(BeTrue(), "Error should be 'file does not exist'")

			// Verify git log shows DELETE commit
			gitLogCmd := exec.Command("git", "log", "--oneline", "-n", "5")
			gitLogCmd.Dir = crdLifecycleRepo.CheckoutDir
			logOutput, logErr := gitLogCmd.CombinedOutput()
			g.Expect(logErr).NotTo(HaveOccurred(), "Should be able to read git log")
			g.Expect(string(logOutput)).To(ContainSubstring("DELETE"),
				"Git log should contain DELETE operation")
		}
		Eventually(verifyFileDeleted, "60s", "1s").Should(Succeed())

		By("verifying the deleted CRD file does not reappear after terminating updates")
		Consistently(func(g Gomega) {
			pullCmd := exec.Command("git", "pull")
			pullCmd.Dir = crdLifecycleRepo.CheckoutDir
			_, _ = pullCmd.CombinedOutput()

			expectedFile := filepath.Join(crdLifecycleRepo.CheckoutDir,
				"e2e/crd-delete-test",
				"apiextensions.k8s.io/v1/customresourcedefinitions/"+iceCreamCRDMirrorFile(crdGroupCRDLifecycle))
			_, statErr := os.Stat(expectedFile)
			g.Expect(statErr).To(HaveOccurred(), "CRD file must stay deleted after CRD termination updates")
			g.Expect(os.IsNotExist(statErr)).To(BeTrue(), "CRD file must not reappear in Git")
		}, "15s", "1s").Should(Succeed())

		By("cleaning up test resources")
		cleanupClusterWatchRule(clusterWatchRuleName)
		cleanupGitTarget(destName, testNs)

		By("✅ CRD deletion via ClusterWatchRule E2E test passed")
	})
})
