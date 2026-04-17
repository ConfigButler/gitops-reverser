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
	"encoding/json"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var _ = Describe("Aggregated API server", Label("aggregated-api"), Ordered, func() {
	var testNs string

	BeforeAll(func() {
		testNs = testNamespaceFor("aggregated-api")
		_, _ = kubectlRun("create", "namespace", testNs)
	})

	AfterAll(func() {
		cleanupNamespace(testNs)
	})

	SetDefaultEventuallyTimeout(30 * time.Second)
	SetDefaultEventuallyPollingInterval(time.Second)

	It("should install and serve flunders through the aggregation layer", Label("smoke"), func() {
		By("waiting for the wardle APIService to report available")
		Eventually(func(g Gomega) {
			output, err := kubectlRun(
				"get",
				"apiservice",
				"v1alpha1.wardle.example.com",
				"-o",
				"jsonpath={.status.conditions[?(@.type=='Available')].status}",
			)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).To(Equal("True"))
		}, 180*time.Second, 2*time.Second).Should(Succeed())

		By("verifying wardle resources are discoverable")
		Eventually(func(g Gomega) {
			output, err := kubectlRun("api-resources", "--api-group=wardle.example.com")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("flunders"))
			g.Expect(output).To(ContainSubstring("fischers"))
		}, 30*time.Second, time.Second).Should(Succeed())

		flunderName := fmt.Sprintf("install-smoke-%d", GinkgoRandomSeed())
		flunderManifest := fmt.Sprintf(`apiVersion: wardle.example.com/v1alpha1
kind: Flunder
metadata:
  name: %s
  namespace: %s
spec:
  reference: smoke-reference
`, flunderName, testNs)

		By("creating a Flunder via the aggregated API")
		_, err := kubectlRunWithStdin(testNs, flunderManifest, "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())

		DeferCleanup(func() {
			if skipCleanupBecauseResourcesArePreserved(fmt.Sprintf("Flunder %s/%s", testNs, flunderName)) {
				return
			}
			_, _ = kubectlRunInNamespace(testNs, "delete", "flunder", flunderName, "--ignore-not-found=true")
		})

		By("reading the Flunder back through kubectl discovery")
		Eventually(func(g Gomega) {
			output, err := kubectlRunInNamespace(testNs, "get", "flunder", flunderName, "-o", "json")
			g.Expect(err).NotTo(HaveOccurred())

			var obj unstructured.Unstructured
			g.Expect(json.Unmarshal([]byte(stripKubectlWarnings(output)), &obj.Object)).To(Succeed())

			g.Expect(obj.GetAPIVersion()).To(Equal("wardle.example.com/v1alpha1"))
			g.Expect(obj.GetKind()).To(Equal("Flunder"))
			g.Expect(obj.GetName()).To(Equal(flunderName))
			g.Expect(obj.GetNamespace()).To(Equal(testNs))

			reference, found, nestedErr := unstructured.NestedString(obj.Object, "spec", "reference")
			g.Expect(nestedErr).NotTo(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(reference).To(Equal("smoke-reference"))
		}, 30*time.Second, time.Second).Should(Succeed())
	})
})
