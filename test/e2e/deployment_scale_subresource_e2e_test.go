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
	"path/filepath"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

var _ = Describe("Deployment scale subresource", Label("manager", "subresource"), func() {
	It("mirrors kubectl scale through the parent Deployment watch", func() {
		testNs := testNamespaceFor("deployment-scale")
		repoName := fmt.Sprintf("e2e-deployment-scale-%d", GinkgoRandomSeed())
		providerName := "deployment-scale-provider"
		targetName := "deployment-scale-target"
		watchRuleName := "deployment-scale-watchrule"
		targetPath := "e2e/deployment-scale"
		deploymentName := "scale-target"

		By("creating the deployment-scale test namespace")
		_, _ = kubectlRun("create", "namespace", testNs)

		By("setting up a dedicated Gitea repo and credentials")
		repo := SetupRepo(resolveE2EContext(), testNs, repoName)
		_, err := kubectlRunInNamespace(testNs, "apply", "-f", repo.SecretsYAML)
		Expect(err).NotTo(HaveOccurred(), "failed to apply git secrets to test namespace")
		applySOPSAgeKeyToNamespace(testNs)

		defer cleanupNamespace(testNs)

		By("creating GitProvider, GitTarget and a Deployment WatchRule")
		createGitProviderWithURLInNamespace(providerName, testNs, repo.GitSecretHTTP, repo.RepoURLHTTP)
		createGitTarget(targetName, testNs, providerName, targetPath, "main")
		applyDeploymentWatchRule(testNs, watchRuleName, targetName)
		verifyResourceStatus("gitprovider", providerName, testNs, "True", "Ready", "")
		verifyResourceStatus("gittarget", targetName, testNs, "True", "Ready", "")
		verifyResourceStatus("watchrule", watchRuleName, testNs, "True", "Ready", "")

		By("creating a Deployment with replicas=1")
		applyScaleTestDeployment(testNs, deploymentName, 1)
		deploymentFile := filepath.Join(
			repo.CheckoutDir,
			targetPath,
			fmt.Sprintf("apps/v1/deployments/%s/%s.yaml", testNs, deploymentName),
		)
		Eventually(func(g Gomega) {
			g.Expect(committedDeploymentReplicas(g, repo.CheckoutDir, deploymentFile)).To(Equal(int64(1)))
		}, 90*time.Second, 2*time.Second).Should(Succeed())

		By("scaling the Deployment through the deployments/scale subresource")
		_, err = kubectlRunInNamespace(testNs, "scale", "deployment", deploymentName, "--replicas=3")
		Expect(err).NotTo(HaveOccurred(), "kubectl scale should succeed")

		By("verifying the parent Deployment manifest is updated in git")
		Eventually(func(g Gomega) {
			g.Expect(committedDeploymentReplicas(g, repo.CheckoutDir, deploymentFile)).To(Equal(int64(3)))
		}, 90*time.Second, 2*time.Second).Should(Succeed())

		cleanupWatchRule(watchRuleName, testNs)
		cleanupGitTarget(targetName, testNs)
	})
})

func applyDeploymentWatchRule(namespace, name, targetName string) {
	manifest := fmt.Sprintf(`apiVersion: configbutler.ai/v1alpha2
kind: WatchRule
metadata:
  name: %s
  namespace: %s
spec:
  targetRef:
    kind: GitTarget
    name: %s
  rules:
  - apiGroups: ["apps"]
    apiVersions: ["v1"]
    resources: ["deployments"]
`, name, namespace, targetName)
	_, err := kubectlRunWithStdin(namespace, manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to apply Deployment WatchRule")
}

func applyScaleTestDeployment(namespace, name string, replicas int64) {
	manifest := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
spec:
  replicas: %d
  selector:
    matchLabels:
      app.kubernetes.io/name: %s
  template:
    metadata:
      labels:
        app.kubernetes.io/name: %s
    spec:
      containers:
      - name: pause
        image: registry.k8s.io/pause:3.10
`, name, namespace, replicas, name, name)
	_, err := kubectlRunWithStdin(namespace, manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to apply scale test Deployment")
}

func committedDeploymentReplicas(g Gomega, checkoutDir, deploymentFile string) int64 {
	GinkgoHelper()

	pullLatestRepoState(g, checkoutDir)
	content, err := os.ReadFile(deploymentFile)
	g.Expect(err).NotTo(HaveOccurred(), "Deployment file should exist at %s", deploymentFile)

	var obj unstructured.Unstructured
	g.Expect(yaml.Unmarshal(content, &obj.Object)).To(Succeed(), "Deployment YAML should parse")
	value, found, err := unstructured.NestedFieldNoCopy(obj.Object, "spec", "replicas")
	g.Expect(err).NotTo(HaveOccurred(), "Deployment spec.replicas should be readable")
	g.Expect(found).To(BeTrue(), "Deployment spec.replicas should be present")
	replicas, err := replicasAsInt64(value)
	g.Expect(err).NotTo(HaveOccurred(), "Deployment spec.replicas should be numeric")
	return replicas
}

func replicasAsInt64(value interface{}) (int64, error) {
	switch v := value.(type) {
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	case float64:
		return int64(v), nil
	case string:
		return strconv.ParseInt(v, 10, 64)
	default:
		return 0, fmt.Errorf("unsupported replicas value %T", value)
	}
}
