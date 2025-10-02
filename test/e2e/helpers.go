/*
Package e2e provides helper functions for end-to-end testing of the GitOps Reverser controller.
It includes utilities for template rendering, kubectl operations, and metrics validation.
*/
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"text/template"

	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // Ginkgo standard practice
	. "github.com/onsi/gomega"    //nolint:staticcheck // Ginkgo standard practice

	"github.com/ConfigButler/gitops-reverser/test/utils"
)

// namespace where the project is deployed in.
const namespace = "sut"

// serviceAccountName created for the project.
const serviceAccountName = "gitops-reverser-controller-manager"

// metricsServiceName is the name of the metrics service of the project.
const metricsServiceName = "gitops-reverser-controller-manager-metrics-service"

// renderTemplate loads and executes a Go template file with the given data
// Returns the rendered string or an error if loading or execution fails.
func renderTemplate(templatePath string, data interface{}) (string, error) {
	tmpl, err := template.ParseFiles(templatePath)
	if err != nil {
		return "", fmt.Errorf("failed to parse template %s: %w", templatePath, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute template %s: %w", templatePath, err)
	}
	return buf.String(), nil
}

// applyFromTemplate renders a template with data and applies it via kubectl using stdin streaming
// Returns an error if rendering or kubectl execution fails.
func applyFromTemplate(templatePath string, data interface{}, namespace string) error {
	yamlContent, err := renderTemplate(templatePath, data)
	if err != nil {
		return err
	}

	ctx := context.Background()
	if namespace != "" {
		cmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-", "-n", namespace)
		cmd.Stdin = strings.NewReader(yamlContent)
		_, err = utils.Run(cmd)
		return err
	}
	cmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(yamlContent)
	_, err = utils.Run(cmd)
	return err
}

// createMetricsCurlPod creates a curl pod to fetch metrics from the metrics endpoint.
func createMetricsCurlPod(podName, token string) {
	By(fmt.Sprintf("creating curl pod '%s' to access metrics endpoint", podName))
	data := struct {
		PodName            string
		Token              string
		ServiceName        string
		Namespace          string
		ServiceAccountName string
	}{
		PodName:            podName,
		Token:              token,
		ServiceName:        metricsServiceName,
		Namespace:          namespace,
		ServiceAccountName: serviceAccountName,
	}

	err := applyFromTemplate("test/e2e/templates/curl-pod.yaml.tmpl", data, namespace)
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to create curl pod %s", podName))
}

// waitForMetricsCurlCompletion waits for the specified curl pod to complete.
func waitForMetricsCurlCompletion(podName string) {
	By(fmt.Sprintf("waiting for curl pod '%s' to complete", podName))
	ctx := context.Background()
	verifyCurlComplete := func(g Gomega) {
		cmd := exec.CommandContext(ctx, "kubectl", "get", "pods", podName,
			"-o", "jsonpath={.status.phase}",
			"-n", namespace)
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(Equal("Succeeded"), fmt.Sprintf("curl pod %s should complete successfully", podName))
	}
	Eventually(verifyCurlComplete).Should(Succeed())
}

// getMetricsFromCurlPod retrieves and returns the metrics output from the specified curl pod.
func getMetricsFromCurlPod(podName string) string {
	By(fmt.Sprintf("getting metrics output from curl pod '%s'", podName))
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "kubectl", "logs", podName, "-n", namespace)
	metricsOutput, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to retrieve logs from curl pod %s", podName))
	Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"), "Metrics endpoint should respond successfully")
	return metricsOutput
}

// cleanupPod deletes the specified curl pod.
func cleanupPod(podName string) {
	By(fmt.Sprintf("cleaning up curl pod %s", podName))
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "kubectl", "delete", "pod", podName, "--namespace", namespace)
	if output, err := utils.Run(cmd); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Warning: failed to delete pod %s: %v\nOutput: %s\n", podName, err, output)
	}
}

// fetchMetricsOverHTTPS creates a curl pod, fetches metrics over HTTPS, and returns the output.
func fetchMetricsOverHTTPS(token string) string {
	const podName = "curl-metrics"
	createMetricsCurlPod(podName, token)
	waitForMetricsCurlCompletion(podName)
	By("getting the metrics by checking curl-metrics logs")
	result := getMetricsFromCurlPod(podName)
	defer cleanupPod(podName) // Ensure the pod is cleaned up after fetching metrics

	return result
}
