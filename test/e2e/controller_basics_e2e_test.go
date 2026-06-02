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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ConfigButler/gitops-reverser/test/utils"
)

var _ = Describe("Manager Controller Basics", Label("manager"), Ordered, func() {
	var controllerPodName string // Name of first controller pod for logging

	BeforeAll(func() {
		By("setting up Prometheus client for metrics testing")
		setupPrometheusClient()
		verifyPrometheusAvailable()
	})

	SetDefaultEventuallyTimeout(30 * time.Second)
	SetDefaultEventuallyPollingInterval(time.Second)

	It("should run successfully", Label("smoke"), func() {
		By("validating that the gitops-reverser pods are running as expected")
		verifyControllerUp := func(g Gomega) {
			// Get the names of the gitops-reverser pods
			podOutput, err := kubectlRunInNamespace(
				namespace,
				"get",
				"pods",
				"-l",
				"control-plane=gitops-reverser",
				"-o", "go-template={{ range .items }}"+
					"{{ if not .metadata.deletionTimestamp }}"+
					"{{ .metadata.name }}"+
					"{{ \"\\n\" }}{{ end }}{{ end }}",
			)
			g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve gitops-reverser pod information")
			podNames := utils.GetNonEmptyLines(podOutput)
			g.Expect(podNames).To(HaveLen(1), "expected exactly 1 controller pod running")
			controllerPodName = podNames[0] // Use first pod for logging
			g.Expect(controllerPodName).To(ContainSubstring("gitops-reverser"))

			// Validate all pods' status
			for _, podName := range podNames {
				output, err := kubectlRunInNamespace(
					namespace,
					"get",
					"pods",
					podName,
					"-o",
					"jsonpath={.status.phase}",
				)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), fmt.Sprintf("Incorrect status for pod %s", podName))
			}
		}
		Eventually(verifyControllerUp).Should(Succeed())
	})

	It("should expose the controller service", Label("smoke"), func() {
		By("verifying controller service exists")
		_, err := kubectlRunInNamespace(namespace, "get", "svc", controllerServiceName)
		Expect(err).NotTo(HaveOccurred(), "Controller service should exist")

		By("verifying controller service routes to the controller pod")
		Eventually(func(g Gomega) {
			expectServiceRoutesToPod(g, controllerServiceName, controllerPodName)
		}, 30*time.Second).Should(Succeed())
	})

	It("should expose the audit service separately", func() {
		By("verifying audit service exists")
		_, err := kubectlRunInNamespace(namespace, "get", "svc", auditServiceName)
		Expect(err).NotTo(HaveOccurred(), "Audit service should exist")

		By("verifying audit service routes to the controller pod")
		Eventually(func(g Gomega) {
			expectServiceRoutesToPod(g, auditServiceName, controllerPodName)
		}, 30*time.Second).Should(Succeed())
	})

	It("should ensure the metrics endpoint is serving metrics", Label("smoke"), func() {
		By("validating that the controller service is available for metrics")
		_, err := kubectlRunInNamespace(namespace, "get", "service", controllerServiceName)
		Expect(err).NotTo(HaveOccurred(), "Controller service should exist")

		By("waiting for the metrics endpoint to be ready")
		verifyMetricsEndpointReady := func(g Gomega) {
			output, err := kubectlRunInNamespace(namespace, "get", "endpoints", controllerServiceName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("8443"), "Metrics endpoint is not ready")
		}
		Eventually(verifyMetricsEndpointReady).Should(Succeed())

		By("verifying that the controller manager is serving the metrics server")
		verifyMetricsServerStarted := func(g Gomega) {
			output, err := kubectlRunInNamespace(namespace, "logs", controllerPodName)
			g.Expect(err).NotTo(HaveOccurred())
			jsonMetricsLogLine := "\"logger\":\"controller-runtime.metrics\"," +
				"\"msg\":\"Serving metrics server\""
			g.Expect(output).To(
				SatisfyAny(
					ContainSubstring("controller-runtime.metrics\tServing metrics server"),
					ContainSubstring(jsonMetricsLogLine),
				),
				"Metrics server not yet started",
			)
		}
		Eventually(verifyMetricsServerStarted).Should(Succeed())

		By("waiting for Prometheus to scrape controller metrics")
		waitForMetric("sum(up{job='gitops-reverser'})",
			func(v float64) bool { return v == 1 },
			"metrics endpoint should be up")

		By("verifying basic process metrics are exposed")
		waitForMetric("sum(process_cpu_seconds_total{job='gitops-reverser'})",
			func(v float64) bool { return v > 0 },
			"process metrics should exist")

		By("verifying metrics from the controller pod")
		podCount, err := queryPrometheus("sum(up{job='gitops-reverser'})")
		Expect(err).NotTo(HaveOccurred())
		Expect(podCount).To(BeEquivalentTo(1), "Should scrape from 1 controller pod")

		fmt.Printf("✅ Metrics collection verified from %.0f pods\n", podCount)
		fmt.Printf("📊 Inspect metrics: %s\n", getPrometheusURL())
	})

	It("should receive audit webhook events from kube-apiserver", Label("smoke"), func() {
		By("recording baseline audit event count")
		baselineAuditEvents, err := queryPrometheus("sum(gitopsreverser_audit_events_received_total) or vector(0)")
		Expect(err).NotTo(HaveOccurred())
		fmt.Printf("📊 Baseline audit events: %.0f\n", baselineAuditEvents)
		baselineOfficialAuditEvents, err := queryPrometheus(
			"sum(gitopsreverser_audit_events_received_total{source='official'}) or vector(0)",
		)
		Expect(err).NotTo(HaveOccurred())
		fmt.Printf("📊 Baseline official audit events: %.0f\n", baselineOfficialAuditEvents)

		By("creating a ConfigMap to trigger audit events")
		_, err = kubectlRunInNamespace(
			namespace,
			"create",
			"configmap",
			"audit-test-cm",
			"--from-literal=test=audit",
		)
		Expect(err).NotTo(HaveOccurred(), "ConfigMap creation should succeed")
		_, err = kubectlRunInNamespace(
			namespace,
			"patch",
			"configmap",
			"audit-test-cm",
			"--type=merge",
			"--patch",
			`{"data":{"test":"audit-updated"}}`,
		)
		Expect(err).NotTo(HaveOccurred(), "ConfigMap update should succeed")

		By("waiting for audit event metric to increment")
		waitForMetricWithTimeout("sum(gitopsreverser_audit_events_received_total) or vector(0)",
			func(v float64) bool { return v > baselineAuditEvents },
			"audit events should increment", 2*time.Minute)
		waitForMetricWithTimeout(
			"sum(gitopsreverser_audit_events_received_total{source='official'}) or vector(0)",
			func(v float64) bool { return v > baselineOfficialAuditEvents },
			"audit events should increment for source=official", 2*time.Minute,
		)

		By("verifying audit events were received")
		currentAuditEvents, err := queryPrometheus("sum(gitopsreverser_audit_events_received_total) or vector(0)")
		Expect(err).NotTo(HaveOccurred())
		Expect(currentAuditEvents).To(BeNumerically(">", baselineAuditEvents),
			"Should have received audit events from kube-apiserver")

		By("verifying the EventList ingress metric appears for source=official")
		waitForMetricWithTimeout(
			"sum(gitopsreverser_audit_eventlists_total{source='official'}) or vector(0)",
			func(v float64) bool { return v > 0 },
			"EventList ingress requests should be counted for source=official", 2*time.Minute,
		)

		By("verifying no removed cluster/gvr/action label remains on any audit series")
		for _, removed := range []string{"cluster", "gvr", "action"} {
			stale, queryErr := queryPrometheus(fmt.Sprintf(
				"sum({__name__=~'gitopsreverser_audit_.+', %s=~'.+'}) or vector(0)", removed))
			Expect(queryErr).NotTo(HaveOccurred())
			Expect(stale).To(BeNumerically("==", 0),
				fmt.Sprintf("no audit series should still carry the %q label", removed))
		}

		newEvents := currentAuditEvents - baselineAuditEvents
		fmt.Printf("✅ Received %.0f new audit events from kube-apiserver\n", newEvents)
		fmt.Printf("📊 Total audit events: %.0f\n", currentAuditEvents)

		By("cleaning up audit test resources")
		_, _ = kubectlRunInNamespace(
			namespace,
			"delete",
			"configmap",
			"audit-test-cm",
			"--ignore-not-found=true",
		)
	})
})
