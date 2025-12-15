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
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // Ginkgo standard practice
	. "github.com/onsi/gomega"    //nolint:staticcheck // Ginkgo standard practice
	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	"github.com/ConfigButler/gitops-reverser/test/utils"
)

// namespace where the project is deployed in.
const namespace = "sut"

// metricsServiceName is the name of the metrics service of the project.
const metricsServiceName = "gitops-reverser-controller-manager-metrics-service"

// promAPI is the Prometheus API client instance
var promAPI v1.API //nolint:gochecknoglobals // Shared across test functions

// setupPrometheusClient initializes the Prometheus API client
func setupPrometheusClient() {
	By("setting up Prometheus API client")
	client, err := api.NewClient(api.Config{
		Address: getPrometheusURL(),
	})
	Expect(err).NotTo(HaveOccurred(), "Failed to create Prometheus client")
	promAPI = v1.NewAPI(client)
}

// verifyPrometheusAvailable checks if Prometheus is accessible and ready
// It also waits for Prometheus pods to be ready
func verifyPrometheusAvailable() {
	By("verifying Prometheus is available via port-forward")
	Eventually(func(g Gomega) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) //nolint:mnd // reasonable timeout
		defer cancel()
		_, err := promAPI.Config(ctx)
		g.Expect(err).NotTo(HaveOccurred(), "Prometheus should be accessible via port-forward")
	}, 30*time.Second, 2*time.Second).Should(Succeed()) //nolint:mnd // reasonable timeout and polling interval
	By("✅ Prometheus is available and ready")
}

// queryPrometheus executes a PromQL query and returns the first scalar value
// Returns 0 if no results found
func queryPrometheus(query string) (float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second) //nolint:mnd // reasonable query timeout
	defer cancel()

	result, _, err := promAPI.Query(ctx, query, time.Now())
	if err != nil {
		return 0, fmt.Errorf("prometheus query failed: %w", err)
	}

	switch v := result.(type) {
	case model.Vector:
		if len(v) == 0 {
			return 0, nil
		}
		return float64(v[0].Value), nil
	case *model.Scalar:
		return float64(v.Value), nil
	default:
		return 0, fmt.Errorf("unexpected result type: %T", result)
	}
}

// waitForMetric waits for a Prometheus metric query to satisfy a condition
func waitForMetric(query string, condition func(float64) bool, description string) {
	By(fmt.Sprintf("waiting for metric: %s", description))
	Eventually(func(g Gomega) {
		value, err := queryPrometheus(query)
		g.Expect(err).NotTo(HaveOccurred(), "Failed to query Prometheus")
		g.Expect(condition(value)).To(BeTrue(),
			fmt.Sprintf("%s (query: %s, value: %.2f)", description, query, value))
	}, 30*time.Second, 2*time.Second).Should(Succeed()) //nolint:mnd // reasonable timeout and polling interval
}

// getPrometheusURL returns the URL for accessing Prometheus UI
func getPrometheusURL() string {
	return "http://localhost:19090"
}

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
// extraArgs allows passing additional kubectl arguments (e.g., "--dry-run", "--server-side")
func applyFromTemplate(templatePath string, data interface{}, namespace string, extraArgs ...string) error {
	yamlContent, err := renderTemplate(templatePath, data)
	if err != nil {
		return err
	}

	// 1. Define the binary explicitly
	const binary = "kubectl"

	args := []string{
		"apply", "-f", "-",
	}

	// Add extra arguments
	args = append(args, extraArgs...)

	if namespace != "" {
		args = append(args, "-n", namespace)
	}

	ctx := context.Background()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdin = strings.NewReader(yamlContent)
	_, err = utils.Run(cmd)
	return err
}

// waitForCertificateSecrets waits for cert-manager to create the required certificate secrets
// This prevents race conditions where controller pods try to mount secrets before they exist
func waitForCertificateSecrets() {
	By("waiting for webhook certificate secret to be created by cert-manager")
	Eventually(func(g Gomega) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) //nolint:mnd // reasonable timeout
		defer cancel()
		cmd := exec.CommandContext(ctx, "kubectl", "get", "secret", "webhook-server-cert", "-n", namespace)
		_, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred(), "webhook-server-cert secret should exist")
	}, 60*time.Second, 2*time.Second).Should(Succeed()) //nolint:mnd // reasonable timeout for cert-manager

	By("waiting for metrics certificate secret to be created by cert-manager")
	Eventually(func(g Gomega) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) //nolint:mnd // reasonable timeout
		defer cancel()
		cmd := exec.CommandContext(ctx, "kubectl", "get", "secret", "metrics-server-cert", "-n", namespace)
		_, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred(), "metrics-server-cert secret should exist")
	}, 60*time.Second, 2*time.Second).Should(Succeed()) //nolint:mnd // reasonable timeout for cert-manager

	By("✅ All certificate secrets are ready")
}

// getBaseFolder returns the baseFolder used by GitDestination in e2e tests.
// Must satisfy the CRD validation (POSIX-like relative path, no traversal).
func getBaseFolder() string {
	return "e2e"
}

// createGitTarget creates a GitTarget that binds a GitProvider, branch and baseFolder.
//
//nolint:unparam // in e2e helpers we accept constant namespace ("sut"); keep signature for clarity in template calls
func createGitTarget(name, namespace, repoConfigName, baseFolder, branch string) {
	By(fmt.Sprintf("creating GitTarget '%s' in ns '%s' for GitProvider '%s' with baseFolder '%s'",
		name, namespace, repoConfigName, baseFolder))

	data := struct {
		Name                string
		Namespace           string
		RepoConfigName      string
		RepoConfigNamespace string
		Branch              string
		BaseFolder          string
	}{
		Name:                name,
		Namespace:           namespace,
		RepoConfigName:      repoConfigName,
		RepoConfigNamespace: namespace,
		Branch:              branch,
		BaseFolder:          baseFolder,
	}

	err := applyFromTemplate("test/e2e/templates/gittarget.tmpl", data, namespace)

	Expect(err).NotTo(HaveOccurred(), "Failed to apply GitTarget")
}

// cleanupGitTarget deletes a GitTarget resource.
//
//nolint:unparam // in e2e helpers we accept constant namespace ("sut"); keep signature for clarity
func cleanupGitTarget(name, namespace string) {
	By(fmt.Sprintf("cleaning up GitTarget '%s' in ns '%s'", name, namespace))
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "kubectl", "delete", "gittarget", name,
		"-n", namespace, "--ignore-not-found=true")
	_, _ = utils.Run(cmd)
}

// cleanupWatchRule deletes a WatchRule resource.
//
//nolint:unparam // in e2e helpers we accept constant namespace ("sut"); keep signature for clarity
func cleanupWatchRule(name, namespace string) {
	By(fmt.Sprintf("cleaning up WatchRule '%s' in ns '%s'", name, namespace))
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "kubectl", "delete", "watchrule", name,
		"-n", namespace, "--ignore-not-found=true")
	_, _ = utils.Run(cmd)
}

// cleanupClusterWatchRule deletes a ClusterWatchRule resource.
func cleanupClusterWatchRule(name string) {
	By(fmt.Sprintf("cleaning up ClusterWatchRule '%s'", name))
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "kubectl", "delete", "clusterwatchrule", name,
		"--ignore-not-found=true")
	_, _ = utils.Run(cmd)
}
