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
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var _ = Describe("Manager WatchRule ConfigMap and Secret", Label("manager"), Ordered, func() {
	var (
		testNs        string
		watchRuleRepo *RepoArtifacts
	)

	BeforeAll(func() {
		By("creating WatchRule ConfigMap/Secret test namespace")
		testNs = testNamespaceFor("manager-watchrule")
		_, _ = kubectlRun("create", "namespace", testNs) // idempotent; ignore AlreadyExists

		By("setting up Gitea repo and credentials for WatchRule tests")
		watchRuleRepo = SetupRepo(
			resolveE2EContext(),
			testNs,
			fmt.Sprintf("e2e-manager-watchrule-%d", GinkgoRandomSeed()),
		)

		By("applying git secrets to test namespace")
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", watchRuleRepo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")
		applySOPSAgeKeyToNamespace(testNs)

		By("creating shared gitprovider-normal for WatchRule specs")
		createGitProviderWithURLInNamespace(
			"gitprovider-normal",
			testNs,
			watchRuleRepo.GitSecretHTTP,
			watchRuleRepo.RepoURLHTTP,
		)
		verifyResourceStatus(
			"gitprovider", "gitprovider-normal", testNs,
			"True", "Ready", "Repository connectivity validated",
		)
	})

	AfterAll(func() {
		cleanupClusterResource("crd", iceCreamCRDName(crdGroupWildcardRule))
		cleanupNamespace(testNs)
	})

	SetDefaultEventuallyTimeout(30 * time.Second)
	SetDefaultEventuallyPollingInterval(time.Second)

	It("should handle a normal and healthy GitProvider", func() {
		// gitprovider-normal is created and verified in BeforeAll; this spec
		// re-asserts it stays Ready without re-creating it.
		verifyResourceStatus(
			"gitprovider", "gitprovider-normal", testNs, "True", "Ready", "Repository connectivity validated",
		)
	})

	It("should reconcile a WatchRule CR", func() {
		gitProviderName := "gitprovider-normal"
		watchRuleName := "watchrule-test"

		By("creating a WatchRule that references the working GitProvider")
		destName := watchRuleName + "-dest"
		createGitTarget(destName, testNs, gitProviderName, getBaseFolder(), "main")

		data := struct {
			Name            string
			Namespace       string
			DestinationName string
		}{
			Name:            watchRuleName,
			Namespace:       testNs,
			DestinationName: destName,
		}

		err := applyFromTemplate("test/e2e/templates/watchrule.tmpl", data, testNs)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply WatchRule")

		By("verifying the WatchRule is reconciled")
		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")

		By("cleaning up test resources")
		cleanupWatchRule(watchRuleName, testNs)
		cleanupGitTarget(destName, testNs)
	})

	It("should expand wildcard resources across core and custom namespaced APIs", func() {
		gitProviderName := "gitprovider-normal"
		watchRuleName := "watchrule-wildcard-expansion-test"
		destName := watchRuleName + "-dest"
		gitTargetPath := "e2e/wildcard-expansion-test"
		configMapName := "wildcard-config"
		orderName := "wildcard-order"

		By("installing the wildcard e2e IceCreamOrder CRD")
		Expect(applyIceCreamCRD(crdGroupWildcardRule)).To(Succeed(), "failed to install IceCreamOrder CRD")
		Eventually(func(g Gomega) {
			output, getErr := kubectlRun(
				"get", "crd", iceCreamCRDName(crdGroupWildcardRule),
				"-o", "jsonpath={.status.conditions[?(@.type=='Established')].status}",
			)
			g.Expect(getErr).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).To(Equal("True"))
		}).Should(Succeed())

		By("creating existing resources before the wildcard WatchRule")
		_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", configMapName, "--ignore-not-found=true")
		_, err := kubectlRunInNamespace(
			testNs,
			"create",
			"configmap",
			configMapName,
			"--from-literal=flavor=vanilla",
		)
		Expect(err).NotTo(HaveOccurred(), "ConfigMap creation should succeed")
		createIceCreamOrder(crdGroupWildcardRule, testNs, orderName)

		By("creating a GitTarget and WatchRule with wildcard group, version and resource selectors")
		createGitTarget(destName, testNs, gitProviderName, gitTargetPath, "main")
		watchRuleManifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha1
kind: WatchRule
metadata:
  name: %s
  namespace: %s
spec:
  targetRef:
    kind: GitTarget
    name: %s
  rules:
    - apiGroups: ["*"]
      apiVersions: ["*"]
      resources: ["*"]
`, watchRuleName, testNs, destName)
		_, err = kubectlRunWithStdin(testNs, watchRuleManifest, "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred(), "Failed to apply wildcard WatchRule")

		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")
		Eventually(func(g Gomega) {
			output, getErr := kubectlRunInNamespace(
				testNs,
				"get",
				"watchrule",
				watchRuleName,
				"-o",
				"jsonpath={.status.conditions[?(@.type=='ResourcesResolved')].message}",
			)
			g.Expect(getErr).NotTo(HaveOccurred())
			// Status reports only what the rule watches: a wildcard rule resolves to the
			// followable types in the cluster (a non-zero count).
			g.Expect(output).To(ContainSubstring("watching "))
			g.Expect(output).NotTo(ContainSubstring("watching 0 resource type(s)"))
		}, 90*time.Second, 2*time.Second).Should(Succeed())

		By("verifying the initial wildcard snapshot committed core and custom resources")
		expectedConfigMap := filepath.Join(
			watchRuleRepo.CheckoutDir,
			gitTargetPath,
			fmt.Sprintf("v1/configmaps/%s/%s.yaml", testNs, configMapName),
		)
		expectedOrder := filepath.Join(
			watchRuleRepo.CheckoutDir,
			gitTargetPath,
			iceCreamInstanceDir(crdGroupWildcardRule),
			testNs,
			orderName+".yaml",
		)
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, watchRuleRepo.CheckoutDir)
			_, configMapErr := os.Stat(expectedConfigMap)
			g.Expect(configMapErr).NotTo(HaveOccurred(), "ConfigMap file must exist at %s", expectedConfigMap)
			_, orderErr := os.Stat(expectedOrder)
			g.Expect(orderErr).NotTo(HaveOccurred(), "IceCreamOrder file must exist at %s", expectedOrder)
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("cleaning up wildcard test resources")
		_, _ = kubectlRunInNamespace(
			testNs,
			"delete",
			iceCreamCRDName(crdGroupWildcardRule),
			orderName,
			"--ignore-not-found=true",
		)
		_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", configMapName, "--ignore-not-found=true")
		cleanupWatchRule(watchRuleName, testNs)
		cleanupGitTarget(destName, testNs)
	})

	It("should commit encrypted Secret manifests when WatchRule includes secrets", func() {
		gitProviderName := "gitprovider-normal"
		watchRuleName := "watchrule-secret-encryption-test"
		secretName := "test-secret-encryption"

		By("creating WatchRule that includes secrets")
		destName := watchRuleName + "-dest"
		createGitTarget(destName, testNs, gitProviderName, "e2e/secret-encryption-test", "main")

		data := struct {
			Name            string
			Namespace       string
			DestinationName string
		}{
			Name:            watchRuleName,
			Namespace:       testNs,
			DestinationName: destName,
		}

		err := applyFromTemplate("test/e2e/templates/watchrule-secret.tmpl", data, testNs)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply WatchRule")
		verifyResourceStatus("gittarget", destName, testNs, "True", "Ready", "")
		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")
		verifyResourceStatus("gittarget", destName, testNs, "True", "Ready", "")

		By("creating Secret in watched namespace")
		_, _ = kubectlRunInNamespace(testNs, "delete", "secret", secretName, "--ignore-not-found=true")

		_, err = kubectlRunInNamespace(
			testNs,
			"create",
			"secret",
			"generic",
			secretName,
			"--from-literal=password=do-not-commit",
		)
		Expect(err).NotTo(HaveOccurred(), "Secret creation should succeed")

		By("patching Secret once to avoid informer start race and force an update event")
		_, err = kubectlRunInNamespace(
			testNs,
			"patch",
			"secret",
			secretName,
			"--type=merge",
			"--patch",
			`{"stringData":{"password":"never-commit-this"}}`,
		)
		Expect(err).NotTo(HaveOccurred(), "Secret patch should succeed")

		By("verifying Secret file is committed and does not contain plaintext data")
		verifyEncryptedSecretCommitted := func(g Gomega) {
			pullLatestRepoState(g, watchRuleRepo.CheckoutDir)

			expectedFile := filepath.Join(watchRuleRepo.CheckoutDir,
				"e2e/secret-encryption-test",
				fmt.Sprintf("v1/secrets/%s/%s.sops.yaml", testNs, secretName))
			content, readErr := os.ReadFile(expectedFile)
			g.Expect(readErr).NotTo(HaveOccurred(), fmt.Sprintf("Secret file must exist at %s", expectedFile))
			g.Expect(string(content)).To(ContainSubstring("sops:"))
			g.Expect(string(content)).NotTo(ContainSubstring("do-not-commit"))

			bootstrapSOPSFile := filepath.Join(watchRuleRepo.CheckoutDir, "e2e/secret-encryption-test", ".sops.yaml")
			bootstrapContent, bootstrapErr := os.ReadFile(bootstrapSOPSFile)
			g.Expect(bootstrapErr).NotTo(
				HaveOccurred(),
				fmt.Sprintf(".sops.yaml must exist at %s", bootstrapSOPSFile),
			)
			ageKey, ageKeyErr := readSOPSAgeKeyFromFile(getE2EAgeKeyPath())
			g.Expect(ageKeyErr).NotTo(HaveOccurred(), "Should read age private key")
			recipient, recipientErr := deriveAgeRecipient(ageKey)
			g.Expect(recipientErr).NotTo(HaveOccurred(), "Should derive age recipient")
			g.Expect(string(bootstrapContent)).To(ContainSubstring(recipient))

			decryptedOutput, decryptErr := decryptWithControllerSOPS(content, ageKey)
			g.Expect(decryptErr).NotTo(HaveOccurred(), "Should decrypt committed secret via controller sops binary")
			g.Expect(decryptedOutput).To(ContainSubstring("bmV2ZXItY29tbWl0LXRoaXM="))
		}
		Eventually(verifyEncryptedSecretCommitted, "30s", "2s").Should(Succeed())

		By("cleaning up test resources")
		_, _ = kubectlRunInNamespace(testNs, "delete", "secret", secretName, "--ignore-not-found=true")
		cleanupWatchRule(watchRuleName, testNs)
		cleanupGitTarget(destName, testNs)
	})

	It("should generate missing SOPS age secret when age.recipients.generateWhenMissing is enabled", func() {
		gitProviderName := "gitprovider-normal"
		watchRuleName := "watchrule-secret-autogen-test"
		secretName := "test-secret-autogen"
		generatedSecretName := "sops-age-key-autogen"

		By("ensuring generated encryption secret does not exist before test")
		_, _ = kubectlRunInNamespace(
			testNs,
			"delete",
			"secret",
			generatedSecretName,
			"--ignore-not-found=true",
		)

		By("creating GitTarget with age recipient auto-generation enabled")
		destName := watchRuleName + "-dest"
		createGitTargetWithEncryptionOptions(
			destName,
			testNs,
			gitProviderName,
			"e2e/secret-autogen-test",
			"main",
			generatedSecretName,
			true,
		)

		data := struct {
			Name            string
			Namespace       string
			DestinationName string
		}{
			Name:            watchRuleName,
			Namespace:       testNs,
			DestinationName: destName,
		}

		err := applyFromTemplate("test/e2e/templates/watchrule-secret.tmpl", data, testNs)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply WatchRule")
		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")

		By("validating generated encryption secret has recipient and warning annotations")
		var generatedAgeKey string
		var generatedRecipient string
		Eventually(func(g Gomega) {
			output, getErr := kubectlRunInNamespace(
				testNs,
				"get",
				"secret",
				generatedSecretName,
				"-o",
				"json",
			)
			g.Expect(getErr).NotTo(HaveOccurred())

			var secretObj map[string]interface{}
			unmarshalErr := json.Unmarshal([]byte(output), &secretObj)
			g.Expect(unmarshalErr).NotTo(HaveOccurred())

			annotations, _, annoErr := unstructured.NestedStringMap(secretObj, "metadata", "annotations")
			g.Expect(annoErr).NotTo(HaveOccurred())
			g.Expect(annotations).To(HaveKey("configbutler.ai/age-recipient"))
			g.Expect(annotations["configbutler.ai/age-recipient"]).To(HavePrefix("age1"))
			g.Expect(annotations).To(HaveKeyWithValue("configbutler.ai/backup-warning", "REMOVE_AFTER_BACKUP"))
			generatedRecipient = annotations["configbutler.ai/age-recipient"]

			secretData, found, keyErr := unstructured.NestedStringMap(secretObj, "data")
			g.Expect(keyErr).NotTo(HaveOccurred())
			g.Expect(found).To(BeTrue())
			var sopsAgeKeyB64 string
			for key, value := range secretData {
				if strings.HasSuffix(key, ".agekey") {
					sopsAgeKeyB64 = value
					break
				}
			}
			g.Expect(sopsAgeKeyB64).NotTo(BeEmpty())

			keyBytes, decodeErr := base64.StdEncoding.DecodeString(sopsAgeKeyB64)
			g.Expect(decodeErr).NotTo(HaveOccurred())
			generatedAgeKey = strings.TrimSpace(string(keyBytes))
			g.Expect(generatedAgeKey).To(HavePrefix("AGE-SECRET-KEY-"))
		}, "30s", "2s").Should(Succeed())

		By("waiting for auto-generated target bootstrap file to be present")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, watchRuleRepo.CheckoutDir)

			bootstrapSOPSFile := filepath.Join(watchRuleRepo.CheckoutDir, "e2e/secret-autogen-test", ".sops.yaml")
			bootstrapContent, bootstrapErr := os.ReadFile(bootstrapSOPSFile)
			g.Expect(bootstrapErr).NotTo(HaveOccurred(),
				fmt.Sprintf(".sops.yaml must exist at %s", bootstrapSOPSFile))
			g.Expect(string(bootstrapContent)).To(ContainSubstring(generatedRecipient))
		}, "30s", "2s").Should(Succeed())

		By("creating Secret in watched namespace")
		_, _ = kubectlRunInNamespace(testNs, "delete", "secret", secretName, "--ignore-not-found=true")
		_, err = kubectlRunInNamespace(
			testNs,
			"create",
			"secret",
			"generic",
			secretName,
			"--from-literal=password=do-not-commit",
		)
		Expect(err).NotTo(HaveOccurred(), "Secret creation should succeed")

		By("patching Secret once to avoid informer start race and force an update event")
		_, err = kubectlRunInNamespace(
			testNs,
			"patch",
			"secret",
			secretName,
			"--type=merge",
			"--patch",
			`{"stringData":{"password":"autogen-never-commit-this"}}`,
		)
		Expect(err).NotTo(HaveOccurred(), "Secret patch should succeed")

		By("verifying committed secret is encrypted and decryptable with generated key")
		verifyEncryptedSecretCommitted := func(g Gomega) {
			pullLatestRepoState(g, watchRuleRepo.CheckoutDir)

			expectedFile := filepath.Join(watchRuleRepo.CheckoutDir,
				"e2e/secret-autogen-test",
				fmt.Sprintf("v1/secrets/%s/%s.sops.yaml", testNs, secretName))
			content, readErr := os.ReadFile(expectedFile)
			g.Expect(readErr).NotTo(HaveOccurred(), fmt.Sprintf("Secret file must exist at %s", expectedFile))
			g.Expect(string(content)).To(ContainSubstring("sops:"))
			g.Expect(string(content)).NotTo(ContainSubstring("autogen-never-commit-this"))

			bootstrapSOPSFile := filepath.Join(watchRuleRepo.CheckoutDir, "e2e/secret-autogen-test", ".sops.yaml")
			bootstrapContent, bootstrapErr := os.ReadFile(bootstrapSOPSFile)
			g.Expect(bootstrapErr).NotTo(
				HaveOccurred(),
				fmt.Sprintf(".sops.yaml must exist at %s", bootstrapSOPSFile),
			)
			g.Expect(string(bootstrapContent)).To(ContainSubstring(generatedRecipient))

			decryptedOutput, decryptErr := decryptWithControllerSOPS(content, generatedAgeKey)
			g.Expect(decryptErr).NotTo(HaveOccurred(), "Should decrypt committed secret via generated age key")
			g.Expect(decryptedOutput).To(ContainSubstring("YXV0b2dlbi1uZXZlci1jb21taXQtdGhpcw=="))
		}
		Eventually(verifyEncryptedSecretCommitted, "30s", "2s").Should(Succeed())

		By("cleaning up test resources")
		_, _ = kubectlRunInNamespace(testNs, "delete", "secret", secretName, "--ignore-not-found=true")
		_, _ = kubectlRunInNamespace(
			testNs,
			"delete",
			"secret",
			generatedSecretName,
			"--ignore-not-found=true",
		)
		cleanupWatchRule(watchRuleName, testNs)
		cleanupGitTarget(destName, testNs)
	})

	It("should create Git commit when ConfigMap is added via WatchRule", func() {
		gitProviderName := "gitprovider-normal"
		watchRuleName := "watchrule-configmap-test"
		configMapName := "test-configmap"
		uniqueRepoName := watchRuleRepo.RepoName

		By("creating WatchRule that monitors ConfigMaps")
		destName := watchRuleName + "-dest"
		createGitTarget(destName, testNs, gitProviderName, "e2e/configmap-test", "main")

		data := struct {
			Name            string
			Namespace       string
			DestinationName string
		}{
			Name:            watchRuleName,
			Namespace:       testNs,
			DestinationName: destName,
		}

		err2 := applyFromTemplate("test/e2e/templates/manager/watchrule-configmap.tmpl", data, testNs)
		Expect(err2).NotTo(HaveOccurred(), "Failed to apply WatchRule")

		By("verifying WatchRule is ready")
		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")
		verifyResourceStatus("gittarget", destName, testNs, "True", "Ready", "")

		// Let any in-flight reconciles from prior specs drain before triggering
		// our event. Without this, a stale WatchRule still in informer cache can
		// double-commit the same ConfigMap under a different commitPath and end
		// up on HEAD, masking our commit.
		time.Sleep(5 * time.Second)

		By("creating test ConfigMap to trigger Git commit")
		configMapData := struct {
			Name      string
			Namespace string
		}{
			Name:      configMapName,
			Namespace: testNs,
		}

		err3 := applyFromTemplate(
			"test/e2e/templates/manager/configmap.tmpl",
			configMapData,
			testNs,
			"--as=jane@acme.com", // Important: we validate later if the user is included in the git commit!
		)
		Expect(err3).NotTo(HaveOccurred(), "Failed to apply ConfigMap")

		By("waiting for controller reconciliation of ConfigMap event")
		verifyReconciliationLogs := func(g Gomega) {
			output, err := controllerLogs(500)
			g.Expect(err).NotTo(HaveOccurred())

			// Check for git commit operation in logs
			g.Expect(output).To(ContainSubstring("git commit"),
				"Should see git commit operation in controller logs")
		}
		Eventually(verifyReconciliationLogs).Should(Succeed())

		By("verifying ConfigMap YAML file exists in Gitea repository")
		verifyGitCommit := func(g Gomega) {
			// Use the pre-checked out repository directory
			By("using pre-checked out repository for verification")

			By("pulling latest changes from remote repository")
			pullLatestRepoState(g, watchRuleRepo.CheckoutDir)

			// Check for the expected ConfigMap file (new API-aligned path)
			expectedFile := filepath.Join(watchRuleRepo.CheckoutDir,
				"e2e/configmap-test",
				fmt.Sprintf("v1/configmaps/%s/%s.yaml", testNs, configMapName))
			fileInfo, statErr := os.Stat(expectedFile)
			g.Expect(statErr).NotTo(HaveOccurred(), fmt.Sprintf("ConfigMap file should exist at %s", expectedFile))
			g.Expect(fileInfo.Size()).To(BeNumerically(">", 0), "ConfigMap file should not be empty")

			expectedRepoPath := path.Join(
				"e2e/configmap-test",
				fmt.Sprintf("v1/configmaps/%s/%s.yaml", testNs, configMapName),
			)
			assertLatestCommitForPathTouchesOnlyWithOptional(
				g,
				watchRuleRepo.CheckoutDir,
				expectedRepoPath,
				[]string{expectedRepoPath},
				[]string{
					path.Join("e2e/configmap-test", "README.md"),
					path.Join("e2e/configmap-test", ".sops.yaml"),
				},
			)
			assertLatestCommitTouchesNoNamespaces(
				g,
				watchRuleRepo.CheckoutDir,
				"e2e/configmap-test/v1/configmaps",
				[]string{
					"gitops-reverser",
					"flux-system",
					"kube-system",
					"tilt-playground",
				},
			)

			// Verify file content contains expected ConfigMap data
			content, readErr := os.ReadFile(expectedFile)
			g.Expect(readErr).NotTo(HaveOccurred())
			g.Expect(string(content)).To(ContainSubstring("test-key: test-value"),
				"ConfigMap file should contain expected data")

			// Verify latest commit message contains operation, resource path
			gitLogCmd := exec.Command("git", "log", "-1", "--pretty=%B")
			gitLogCmd.Dir = watchRuleRepo.CheckoutDir
			commitMsg, commitErr := gitLogCmd.CombinedOutput()

			if commitErr != nil {
				g.Expect(commitErr).NotTo(HaveOccurred(),
					fmt.Sprintf("Should read latest commit message. Output: %s", string(commitMsg)))
			}

			msg := string(commitMsg)
			g.Expect(msg).To(ContainSubstring("[CREATE]"),
				"Latest commit message should include operation [CREATE]")
			g.Expect(msg).To(ContainSubstring(fmt.Sprintf("v1/configmaps/%s", configMapName)),
				"Latest commit message should include resource path")

			gitLogCmd = exec.Command("git", "log", "-1", "--pretty=%an")
			gitLogCmd.Dir = watchRuleRepo.CheckoutDir
			authorMsg, commitErr := gitLogCmd.CombinedOutput()
			if commitErr != nil {
				g.Expect(commitErr).NotTo(HaveOccurred(),
					fmt.Sprintf("Should read latest commit author. Output: %s", string(authorMsg)))
			}

			author := string(authorMsg)
			g.Expect(author).To(ContainSubstring("jane@acme.com"))
		}
		Eventually(verifyGitCommit).Should(Succeed())

		By("cleaning up test resources")
		_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", configMapName, "--ignore-not-found=true")
		cleanupWatchRule(watchRuleName, testNs)
		cleanupGitTarget(destName, testNs)

		By("✅ ConfigMap to Git commit E2E test passed - verified actual file creation and commit")
		fmt.Printf("✅ ConfigMap '%s' successfully triggered Git commit with YAML file in repo '%s'\n",
			configMapName, uniqueRepoName)
	})

	// Regression net for the rule-add backfill gap: a ConfigMap that
	// already exists in the cluster *before* a WatchRule is applied must
	// land in git once the rule becomes Ready. Today this fails because
	// ReconcileForRuleChange takes the early-return at manager.go:660
	// when the new rule's GVR is already covered by other rules (and on
	// fresh-target installs the snapshot output can be dropped because no
	// FolderReconciler is registered yet). The unit-level coverage lives
	// in internal/watch/rule_change_snapshot_test.go; this spec locks the
	// observable in at the user-visible layer so a future revamp of the
	// snapshot trigger logic can't silently regress it.
	//
	It("should backfill pre-existing ConfigMap when WatchRule is added afterwards", func() {
		gitProviderName := "gitprovider-normal"
		watchRuleName := "watchrule-backfill-test"
		configMapName := "preexisting-configmap"
		uniqueRepoName := watchRuleRepo.RepoName

		By("creating GitTarget but no WatchRule yet")
		destName := watchRuleName + "-dest"
		createGitTarget(destName, testNs, gitProviderName, "e2e/backfill-rule-add", "main")
		verifyResourceStatus("gittarget", destName, testNs, "True", "Ready", "")

		By("creating the ConfigMap BEFORE the rule that should select it")
		configMapData := struct {
			Name      string
			Namespace string
		}{
			Name:      configMapName,
			Namespace: testNs,
		}
		err := applyFromTemplate(
			"test/e2e/templates/manager/configmap.tmpl",
			configMapData,
			testNs,
			"--as=jane@acme.com",
		)
		Expect(err).NotTo(HaveOccurred(), "Failed to pre-create ConfigMap")

		// Confirm nothing committed yet — no rule exists, so the ConfigMap
		// must not appear in the repo before we apply the WatchRule.
		expectedRepoPath := path.Join(
			"e2e/backfill-rule-add",
			fmt.Sprintf("v1/configmaps/%s/%s.yaml", testNs, configMapName),
		)
		expectedFile := filepath.Join(watchRuleRepo.CheckoutDir, expectedRepoPath)

		By("applying the WatchRule (rule arrives after the resource)")
		watchRuleData := struct {
			Name            string
			Namespace       string
			DestinationName string
		}{
			Name:            watchRuleName,
			Namespace:       testNs,
			DestinationName: destName,
		}
		err = applyFromTemplate("test/e2e/templates/manager/watchrule-configmap.tmpl", watchRuleData, testNs)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply WatchRule")
		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")

		By("waiting for the pre-existing ConfigMap to be backfilled into git")
		verifyBackfill := func(g Gomega) {
			pullLatestRepoState(g, watchRuleRepo.CheckoutDir)

			fileInfo, statErr := os.Stat(expectedFile)
			g.Expect(statErr).NotTo(HaveOccurred(),
				fmt.Sprintf("Pre-existing ConfigMap must be backfilled at %s after the rule lands", expectedFile))
			g.Expect(fileInfo.Size()).To(BeNumerically(">", 0), "Backfilled ConfigMap file should not be empty")

			content, readErr := os.ReadFile(expectedFile)
			g.Expect(readErr).NotTo(HaveOccurred())
			g.Expect(string(content)).To(ContainSubstring("test-key: test-value"),
				"Backfilled file should contain the ConfigMap data that existed before the rule")
		}
		Eventually(verifyBackfill).
			WithTimeout(60 * time.Second).
			WithPolling(2 * time.Second).
			Should(Succeed())

		By("cleaning up test resources")
		_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", configMapName, "--ignore-not-found=true")
		cleanupWatchRule(watchRuleName, testNs)
		cleanupGitTarget(destName, testNs)

		By("✅ rule-add backfill E2E test passed")
		fmt.Printf("✅ Pre-existing ConfigMap '%s' was backfilled to repo '%s' after the WatchRule landed\n",
			configMapName, uniqueRepoName)
	})

	It("should delete Git file when ConfigMap is deleted via WatchRule", func() {
		gitProviderName := "gitprovider-normal"
		watchRuleName := "watchrule-delete-test"
		configMapName := "test-configmap-to-delete"
		uniqueRepoName := watchRuleRepo.RepoName

		By("creating WatchRule that monitors ConfigMaps")
		destName := watchRuleName + "-dest"
		createGitTarget(destName, testNs, gitProviderName, "e2e/delete-test", "main")
		data := struct {
			Name            string
			Namespace       string
			DestinationName string
		}{
			Name:            watchRuleName,
			Namespace:       testNs,
			DestinationName: destName,
		}

		err2 := applyFromTemplate("test/e2e/templates/manager/watchrule-configmap.tmpl", data, testNs)
		Expect(err2).NotTo(HaveOccurred(), "Failed to apply WatchRule")

		By("verifying WatchRule is ready")
		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")
		verifyResourceStatus("gittarget", destName, testNs, "True", "Ready", "")

		// See "should create Git commit when ConfigMap is added": prior specs'
		// WatchRules can still be in the controller's event router and pile on
		// extra commits at unrelated commitPaths, knocking HEAD off our commit.
		time.Sleep(5 * time.Second)

		By("creating test ConfigMap to trigger Git commit")
		configMapData := struct {
			Name      string
			Namespace string
		}{
			Name:      configMapName,
			Namespace: testNs,
		}

		err3 := applyFromTemplate("test/e2e/templates/manager/configmap.tmpl", configMapData, testNs)
		Expect(err3).NotTo(HaveOccurred(), "Failed to apply ConfigMap")

		By("waiting for ConfigMap file to appear in Git repository")
		verifyFileCreated := func(g Gomega) {
			// Pull latest changes from the remote repository
			pullLatestRepoState(g, watchRuleRepo.CheckoutDir)

			// Check for the expected ConfigMap file (new API-aligned path)
			expectedFile := filepath.Join(watchRuleRepo.CheckoutDir,
				"e2e/delete-test",
				fmt.Sprintf("v1/configmaps/%s/%s.yaml", testNs, configMapName))
			fileInfo, statErr := os.Stat(expectedFile)
			g.Expect(statErr).NotTo(HaveOccurred(), fmt.Sprintf("ConfigMap file should exist at %s", expectedFile))
			g.Expect(fileInfo.Size()).To(BeNumerically(">", 0), "ConfigMap file should not be empty")
		}
		Eventually(verifyFileCreated).Should(Succeed())

		By("deleting the ConfigMap to trigger DELETE operation")
		_, err := kubectlRunInNamespace(testNs, "delete", "configmap", configMapName)
		Expect(err).NotTo(HaveOccurred(), "ConfigMap deletion should succeed")

		By("verifying ConfigMap file is deleted from Git repository")
		verifyFileDeleted := func(g Gomega) {
			// Pull latest changes from the remote repository
			By("pulling latest changes after deletion")
			pullLatestRepoState(g, watchRuleRepo.CheckoutDir)

			// Check that the ConfigMap file no longer exists (new API-aligned path)
			expectedRelativePath := path.Join(
				"e2e/delete-test",
				fmt.Sprintf("v1/configmaps/%s/%s.yaml", testNs, configMapName),
			)
			expectedFile := filepath.Join(watchRuleRepo.CheckoutDir, expectedRelativePath)
			_, statErr := os.Stat(expectedFile)
			g.Expect(statErr).To(HaveOccurred(), fmt.Sprintf("ConfigMap file should NOT exist at %s", expectedFile))
			g.Expect(os.IsNotExist(statErr)).To(BeTrue(), "Error should be 'file does not exist'")

			assertLatestCommitForPathTouchesOnlyWithOptional(
				g,
				watchRuleRepo.CheckoutDir,
				expectedRelativePath,
				[]string{expectedRelativePath},
				nil,
			)
			assertLatestCommitTouchesNoNamespaces(
				g,
				watchRuleRepo.CheckoutDir,
				"e2e/delete-test/v1/configmaps",
				[]string{
					"gitops-reverser",
					"flux-system",
					"kube-system",
					"tilt-playground",
				},
			)

			// Verify git log shows DELETE commit
			By("verifying git log shows DELETE operation")
			gitLogCmd := exec.Command("git", "log", "--oneline", "-n", "5")
			gitLogCmd.Dir = watchRuleRepo.CheckoutDir
			logOutput, logErr := gitLogCmd.CombinedOutput()
			g.Expect(logErr).NotTo(HaveOccurred(), "Should be able to read git log")
			g.Expect(string(logOutput)).To(ContainSubstring("DELETE"),
				"Git log should contain DELETE operation")
		}
		// 60s (vs the 30s default) tolerates a busier shared controller under
		// Ginkgo parallelism: the DELETE commit can lag when other processes are
		// driving reconciles concurrently.
		Eventually(verifyFileDeleted, 60*time.Second, 2*time.Second).Should(Succeed())

		By("cleaning up test resources")
		cleanupWatchRule(watchRuleName, testNs)
		cleanupGitTarget(destName, testNs)

		By("✅ ConfigMap deletion E2E test passed - verified file removal from Git")
		fmt.Printf("✅ ConfigMap '%s' deletion successfully triggered Git commit removing file from repo '%s'\n",
			configMapName, uniqueRepoName)
	})
})
