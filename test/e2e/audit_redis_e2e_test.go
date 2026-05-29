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
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/redis/go-redis/v9"
)

const (
	defaultAuditRedisStream  = "gitopsreverser.audit.events.v1"
	defaultE2EValkeyPassword = "e2e-valkey-password"
)

var (
	// auditRedisRepo holds the file-local repo fixtures for both audit-redis Describe blocks.
	auditRedisRepo     *RepoArtifacts
	auditRedisRepoOnce sync.Once
)

func ensureAuditRedisRepo() *RepoArtifacts {
	GinkgoHelper()

	auditRedisRepoOnce.Do(func() {
		consumerNs := testNamespaceFor("audit-consumer")

		By("creating the audit consumer namespace for shared repo fixtures")
		_, _ = kubectlRun("create", "namespace", consumerNs)

		By("setting up the shared Gitea repo for audit-redis tests")
		auditRedisRepo = SetupRepo(
			resolveE2EContext(),
			consumerNs,
			fmt.Sprintf("e2e-audit-redis-%d", GinkgoRandomSeed()),
		)
	})

	Expect(auditRedisRepo).NotTo(BeNil(), "expected audit Redis repo fixtures to be initialised")
	return auditRedisRepo
}

// Serial: shares the single global audit pipeline (audit webhook → Redis stream
// → consumer) with every other audit-redis spec; concurrent audit traffic
// pollutes its exclusivity assertions. See docs/design/e2e-serial-registry.md.
var _ = Describe("Audit Redis Queue", Label("audit-redis", "smoke"), Serial, Ordered, func() {
	var testNs string

	BeforeAll(func() {
		By("creating producer test namespace")
		testNs = testNamespaceFor("audit-redis")
		_, _ = kubectlRun("create", "namespace", testNs) // idempotent; ignore AlreadyExists

		repo := ensureAuditRedisRepo()

		By("applying git secrets to producer test namespace")
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to producer test namespace")
	})

	AfterAll(func() {
		cleanupNamespace(testNs)
	})

	It("should enqueue incoming audit webhook events into a Redis stream", func() {
		streamName := defaultAuditRedisStream
		testConfigMapName := fmt.Sprintf("audit-redis-test-cm-%d", GinkgoRandomSeed())

		By("connecting to Valkey through the e2e port-forward")
		client := redis.NewClient(&redis.Options{
			Addr:     valkeyPortForwardAddr(),
			Password: valkeyPortForwardPassword(),
		})
		Eventually(func(g Gomega) {
			g.Expect(client.Ping(context.Background()).Err()).NotTo(HaveOccurred())
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		By("triggering kube-apiserver audit events with a ConfigMap create/update")
		_, err := kubectlRunInNamespace(
			testNs,
			"delete",
			"configmap",
			testConfigMapName,
			"--ignore-not-found=true",
		)
		Expect(err).NotTo(HaveOccurred())

		_, err = kubectlRunInNamespace(
			testNs,
			"create",
			"configmap",
			testConfigMapName,
			"--from-literal=test=audit-redis",
		)
		Expect(err).NotTo(HaveOccurred())

		_, err = kubectlRunInNamespace(
			testNs,
			"patch",
			"configmap",
			testConfigMapName,
			"--type=merge",
			"--patch",
			`{"data":{"test":"audit-redis-updated"}}`,
		)
		Expect(err).NotTo(HaveOccurred())

		By("verifying Redis stream receives an entry for the ConfigMap audit event")
		Eventually(func(g Gomega) {
			entries, readErr := client.XRange(context.Background(), streamName, "-", "+").Result()
			g.Expect(readErr).NotTo(HaveOccurred())
			g.Expect(entries).NotTo(BeEmpty())

			found := false
			for _, entry := range entries {
				resource, _ := entry.Values["resource"].(string)
				name, _ := entry.Values["name"].(string)
				payload, _ := entry.Values["payload_json"].(string)
				if resource == "configmaps" && name == testConfigMapName && payload != "" {
					found = true
					break
				}
			}
			g.Expect(found).To(BeTrue(), "expected ConfigMap audit event to be enqueued in Redis stream")
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("cleaning up test ConfigMap")
		_, _ = kubectlRunInNamespace(
			testNs,
			"delete",
			"configmap",
			testConfigMapName,
			"--ignore-not-found=true",
		)
	})
})

// auditConsumerDescribe holds the consumer-path e2e test.
// Label: audit-redis (runs together with the producer test via task test-e2e-audit-redis).
// Serial: shares the single global audit pipeline with every other audit-redis
// spec. See docs/design/e2e-serial-registry.md.
var _ = Describe("Audit Redis Consumer", Label("audit-redis", "smoke"), Serial, Ordered, func() {
	var (
		testNs        string
		valkeyClient  *redis.Client
		gitProvName   string
		gitTargetName string
		watchRuleName string
		cmName        string
	)

	BeforeAll(func() {
		By("creating consumer test namespace and applying git secrets")
		testNs = testNamespaceFor("audit-consumer")
		repo := ensureAuditRedisRepo()
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to consumer test namespace")
		applySOPSAgeKeyToNamespace(testNs)

		By("connecting to Valkey through the e2e port-forward")
		valkeyClient = redis.NewClient(&redis.Options{
			Addr:     valkeyPortForwardAddr(),
			Password: valkeyPortForwardPassword(),
		})
		Eventually(func(g Gomega) {
			g.Expect(valkeyClient.Ping(context.Background()).Err()).NotTo(HaveOccurred())
		}, 30*time.Second, 2*time.Second).Should(Succeed())

		By("setting up GitProvider, GitTarget and WatchRule for consumer test")
		seed := GinkgoRandomSeed()
		gitProvName = fmt.Sprintf("audit-consumer-gitprovider-%d", seed)
		gitTargetName = fmt.Sprintf("audit-consumer-gittarget-%d", seed)
		watchRuleName = fmt.Sprintf("audit-consumer-watchrule-%d", seed)
		cmName = fmt.Sprintf("audit-consumer-cm-%d", seed)

		createGitProviderWithURLInNamespace(
			gitProvName,
			testNs,
			auditRedisRepo.GitSecretHTTP,
			auditRedisRepo.RepoURLHTTP,
		)
		verifyResourceStatus("gitprovider", gitProvName, testNs, "True", "Ready", "")

		createGitTarget(gitTargetName, testNs, gitProvName, "e2e/audit-consumer-test", "main")
		verifyResourceStatus("gittarget", gitTargetName, testNs, "True", "Ready", "")

		data := struct {
			Name            string
			Namespace       string
			DestinationName string
		}{
			Name:            watchRuleName,
			Namespace:       testNs,
			DestinationName: gitTargetName,
		}
		err = applyFromTemplate("test/e2e/templates/manager/watchrule-configmap.tmpl", data, testNs)
		Expect(err).NotTo(HaveOccurred(), "failed to apply WatchRule")
		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")
	})

	AfterAll(func() {
		cleanupWatchRule(watchRuleName, testNs)
		cleanupGitTarget(gitTargetName, testNs)
		cleanupNamespacedResource(testNs, "gitprovider", gitProvName)
		cleanupNamespace(testNs)
		if valkeyClient != nil {
			_ = valkeyClient.Close()
		}
	})

	It("should produce a Git commit from an audit stream event consumed by the consumer", func() {
		streamName := defaultAuditRedisStream

		By("creating a ConfigMap to produce an audit event")
		cmData := struct {
			Name      string
			Namespace string
		}{Name: cmName, Namespace: testNs}
		err := applyFromTemplate("test/e2e/templates/manager/configmap.tmpl", cmData, testNs)
		Expect(err).NotTo(HaveOccurred(), "failed to create test ConfigMap")

		By("waiting for the audit stream entry to appear (producer path)")
		// cmName includes GinkgoRandomSeed so it uniquely identifies this test run's entry.
		Eventually(func(g Gomega) {
			entries, readErr := valkeyClient.XRange(context.Background(), streamName, "-", "+").Result()
			g.Expect(readErr).NotTo(HaveOccurred())
			found := false
			for _, entry := range entries {
				resource, _ := entry.Values["resource"].(string)
				name, _ := entry.Values["name"].(string)
				if resource == "configmaps" && name == cmName {
					found = true
					break
				}
			}
			g.Expect(found).To(BeTrue(), "audit stream entry for ConfigMap should appear")
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("waiting for the Git commit to appear (consumer path)")
		expectedFile := filepath.Join(auditRedisRepo.CheckoutDir,
			"e2e/audit-consumer-test",
			fmt.Sprintf("v1/configmaps/%s/%s.yaml", testNs, cmName))

		Eventually(func(g Gomega) {
			pullLatestRepoState(g, auditRedisRepo.CheckoutDir)

			info, statErr := os.Stat(expectedFile)
			g.Expect(statErr).NotTo(HaveOccurred(),
				fmt.Sprintf("ConfigMap file should exist at %s", expectedFile))
			g.Expect(info.Size()).To(BeNumerically(">", 0))

			expectedRepoPath := path.Join(
				"e2e/audit-consumer-test",
				fmt.Sprintf("v1/configmaps/%s/%s.yaml", testNs, cmName),
			)
			assertLatestCommitTouchesOnly(g, auditRedisRepo.CheckoutDir, []string{expectedRepoPath})
			assertLatestCommitTouchesNoNamespaces(
				g,
				auditRedisRepo.CheckoutDir,
				"e2e/audit-consumer-test/v1/configmaps",
				[]string{
					"gitops-reverser",
					"flux-system",
					"kube-system",
					"tilt-playground",
				},
			)

			authorCmd := exec.Command("git", "log", "-1", "--pretty=%an")
			authorCmd.Dir = auditRedisRepo.CheckoutDir
			authorOut, authorErr := authorCmd.CombinedOutput()
			g.Expect(authorErr).NotTo(HaveOccurred(),
				fmt.Sprintf("git log author failed: %s", string(authorOut)))
			g.Expect(string(authorOut)).To(ContainSubstring("system:admin"))
		}, 3*time.Minute, 3*time.Second).Should(Succeed())

		By("cleaning up test ConfigMap")
		_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", cmName, "--ignore-not-found=true")
	})

	It("should attribute the commit to the OIDC display name and email from user.extra", func() {
		const (
			oidcDisplayName = "Simon Koudijs"
			oidcEmail       = "something@configbutler.ai"
		)
		oidcCMName := fmt.Sprintf("audit-oidc-cm-%d", GinkgoRandomSeed())

		By("creating a ConfigMap while impersonating an OIDC user carrying name/email extras")
		// Impersonation makes the real kube-apiserver emit an audit event whose
		// impersonatedUser.Extra carries these keys, mimicking the claims a
		// structured authentication config maps into user.extra for an OIDC
		// login. system:masters keeps the impersonated create authorized
		// without provisioning per-user RBAC.
		err := createConfigMapAsImpersonatedUser(
			testNs,
			oidcCMName,
			"oidc-simon",
			[]string{"system:masters"},
			map[string][]string{
				"configbutler.ai/claims/display-name": {oidcDisplayName},
				"configbutler.ai/claims/email":        {oidcEmail},
			},
		)
		Expect(err).NotTo(HaveOccurred(), "failed to create impersonated ConfigMap")

		repoPath := path.Join(
			"e2e/audit-consumer-test",
			fmt.Sprintf("v1/configmaps/%s/%s.yaml", testNs, oidcCMName),
		)
		expectedFile := filepath.Join(auditRedisRepo.CheckoutDir, repoPath)

		By("waiting for the commit and asserting its author is the OIDC identity")
		Eventually(func(g Gomega) {
			pullLatestRepoState(g, auditRedisRepo.CheckoutDir)

			info, statErr := os.Stat(expectedFile)
			g.Expect(statErr).NotTo(HaveOccurred(),
				fmt.Sprintf("ConfigMap file should exist at %s", expectedFile))
			g.Expect(info.Size()).To(BeNumerically(">", 0))

			// Scope the log to this run's unique file path so the author read
			// back is unambiguously the commit produced by the impersonated
			// create, regardless of commit-window coalescing.
			authorCmd := exec.Command(
				"git", "log", "-1", "--pretty=%an <%ae>", "--", repoPath,
			)
			authorCmd.Dir = auditRedisRepo.CheckoutDir
			authorOut, authorErr := authorCmd.CombinedOutput()
			g.Expect(authorErr).NotTo(HaveOccurred(),
				fmt.Sprintf("git log author failed: %s", string(authorOut)))
			g.Expect(strings.TrimSpace(string(authorOut))).To(
				Equal(fmt.Sprintf("%s <%s>", oidcDisplayName, oidcEmail)),
				"commit author should be the OIDC display name and email from user.extra")
		}, 3*time.Minute, 3*time.Second).Should(Succeed())

		By("cleaning up the test ConfigMap")
		_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", oidcCMName, "--ignore-not-found=true")
	})
})

func valkeyPortForwardAddr() string {
	port := strings.TrimSpace(os.Getenv("VALKEY_PORT"))
	if port == "" {
		port = "16379"
	}
	return "127.0.0.1:" + port
}

func valkeyPortForwardPassword() string {
	password := strings.TrimSpace(os.Getenv("E2E_VALKEY_PASSWORD"))
	if password == "" {
		return defaultE2EValkeyPassword
	}
	return password
}
