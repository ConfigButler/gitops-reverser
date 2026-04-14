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
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/redis/go-redis/v9"
)

const (
	defaultAuditRedisStream  = "gitopsreverser.audit.events.v1"
	defaultE2EValkeyPassword = "e2e-valkey-password"
)

var _ = Describe("Audit Redis Queue", Label("audit-redis", "smoke"), Ordered, func() {
	var testNs string

	BeforeAll(func() {
		By("creating test namespace and applying git secrets")
		testNs = testNamespaceFor("audit-redis")
		_, _ = kubectlRun("create", "namespace", testNs) // idempotent; ignore AlreadyExists
		secretsYaml := strings.TrimSpace(os.Getenv("E2E_SECRETS_YAML"))
		Expect(secretsYaml).NotTo(BeEmpty(), "E2E_SECRETS_YAML must be set by BeforeSuite")
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", secretsYaml)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")
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
				clusterID, _ := entry.Values["cluster_id"].(string)
				payload, _ := entry.Values["payload_json"].(string)
				if resource == "configmaps" && name == testConfigMapName && clusterID == "kind-e2e" && payload != "" {
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
var _ = Describe("Audit Redis Consumer", Label("audit-redis", "smoke"), Ordered, func() {
	var (
		testNs        string
		valkeyClient  *redis.Client
		gitProvName   string
		gitTargetName string
		watchRuleName string
		cmName        string
		gitCheckout   string // local git checkout path; read from E2E_CHECKOUT_DIR env
	)

	BeforeAll(func() {
		By("resolving git checkout path from E2E_CHECKOUT_DIR")
		gitCheckout = strings.TrimSpace(os.Getenv("E2E_CHECKOUT_DIR"))
		Expect(gitCheckout).NotTo(BeEmpty(),
			"E2E_CHECKOUT_DIR must be set (run via task test-e2e or task test-e2e-audit-redis)")

		By("creating test namespace and applying git secrets")
		testNs = testNamespaceFor("audit-consumer")
		_, _ = kubectlRun("create", "namespace", testNs)
		secretsYaml := strings.TrimSpace(os.Getenv("E2E_SECRETS_YAML"))
		Expect(secretsYaml).NotTo(BeEmpty(), "E2E_SECRETS_YAML must be set by BeforeSuite")
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", secretsYaml)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")
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

		repoURL := fmt.Sprintf(giteaRepoURLTemplate, strings.TrimSpace(os.Getenv("E2E_REPO_NAME")))
		Expect(repoURL).NotTo(ContainSubstring("%!(EXTRA"), "E2E_REPO_NAME must be set")

		gitSecretName := strings.TrimSpace(os.Getenv("E2E_GIT_SECRET_HTTP"))
		if gitSecretName == "" {
			gitSecretName = "git-creds"
		}

		createGitProviderWithURLInNamespace(gitProvName, testNs, gitSecretName, repoURL)
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
		_, _ = kubectlRunInNamespace(testNs, "delete", "gitprovider", gitProvName, "--ignore-not-found=true")
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
		expectedFile := filepath.Join(gitCheckout,
			"e2e/audit-consumer-test",
			fmt.Sprintf("v1/configmaps/%s/%s.yaml", testNs, cmName))

		Eventually(func(g Gomega) {
			pullCmd := exec.Command("git", "pull")
			pullCmd.Dir = gitCheckout
			pullOut, pullErr := pullCmd.CombinedOutput()
			g.Expect(pullErr).NotTo(HaveOccurred(),
				fmt.Sprintf("git pull failed: %s", string(pullOut)))

			info, statErr := os.Stat(expectedFile)
			g.Expect(statErr).NotTo(HaveOccurred(),
				fmt.Sprintf("ConfigMap file should exist at %s", expectedFile))
			g.Expect(info.Size()).To(BeNumerically(">", 0))

			authorCmd := exec.Command("git", "log", "-1", "--pretty=%an")
			authorCmd.Dir = gitCheckout
			authorOut, authorErr := authorCmd.CombinedOutput()
			g.Expect(authorErr).NotTo(HaveOccurred(),
				fmt.Sprintf("git log author failed: %s", string(authorOut)))
			g.Expect(string(authorOut)).To(ContainSubstring("system:admin"))
		}, 3*time.Minute, 3*time.Second).Should(Succeed())

		By("cleaning up test ConfigMap")
		_, _ = kubectlRunInNamespace(testNs, "delete", "configmap", cmName, "--ignore-not-found=true")
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
