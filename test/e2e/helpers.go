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
	"os"
	"sort"
	"strings"
	"text/template"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	"github.com/ConfigButler/gitops-reverser/test/utils"
)

// namespace where the project is deployed in.
var namespace = resolveE2ENamespace() //nolint:gochecknoglobals // used across e2e tests
const metricWaitDefaultTimeout = 30 * time.Second
const e2eEncryptionRefName = "sops-age-key"

// controllerServiceName is the single Service name used by the controller.
const controllerServiceName = "gitops-reverser-service"
const controllerPodLabelSelector = "control-plane=gitops-reverser"

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
	waitForMetricWithTimeout(query, condition, description, metricWaitDefaultTimeout)
}

// waitForMetricWithTimeout waits for a Prometheus metric query with a custom timeout.
func waitForMetricWithTimeout(
	query string,
	condition func(float64) bool,
	description string,
	timeout time.Duration,
) {
	By(fmt.Sprintf("waiting for metric: %s", description))
	Eventually(func(g Gomega) {
		value, err := queryPrometheus(query)
		g.Expect(err).NotTo(HaveOccurred(), "Failed to query Prometheus")
		g.Expect(condition(value)).To(BeTrue(),
			fmt.Sprintf("%s (query: %s, value: %.2f)", description, query, value))
	}, timeout, 2*time.Second).Should(Succeed()) //nolint:mnd // reasonable polling interval
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

	args := append([]string{"apply", "-f", "-"}, extraArgs...)
	_, err = kubectlRunWithStdin(namespace, yamlContent, args...)
	return err
}

// getBaseFolder returns the baseFolder used by GitDestination in e2e tests.
// Must satisfy the CRD validation (POSIX-like relative path, no traversal).
func getBaseFolder() string {
	return "e2e"
}

// createGitTarget creates a GitTarget that binds a GitProvider, branch and path.
func createGitTarget(name, namespace, providerName, path, branch string) {
	createGitTargetWithEncryptionOptions(
		name,
		namespace,
		providerName,
		path,
		branch,
		e2eEncryptionRefName,
		false,
	)
}

func createGitTargetWithEncryptionOptions(
	name,
	namespace,
	providerName,
	path,
	branch,
	encryptionSecretName string,
	generateWhenMissing bool,
) {
	By(fmt.Sprintf("creating GitTarget '%s' in ns '%s' for GitProvider '%s' with path '%s'",
		name, namespace, providerName, path))

	data := struct {
		Name                 string
		Namespace            string
		ProviderName         string
		Branch               string
		Path                 string
		EncryptionSecretName string
		GenerateWhenMissing  bool
	}{
		Name:                 name,
		Namespace:            namespace,
		ProviderName:         providerName,
		Branch:               branch,
		Path:                 path,
		EncryptionSecretName: encryptionSecretName,
		GenerateWhenMissing:  generateWhenMissing,
	}

	err := applyFromTemplate("test/e2e/templates/gittarget.tmpl", data, namespace)

	Expect(err).NotTo(HaveOccurred(), "Failed to apply GitTarget")
}

// applySOPSAgeKeyToNamespace copies the sops-age-key secret into the given namespace
// so that GitTarget resources created there can reference it for encryption.
// The key data is read directly from E2E_AGE_KEY_FILE to avoid cross-namespace copy issues.
func applySOPSAgeKeyToNamespace(ns string) {
	By(fmt.Sprintf("applying sops-age-key to test namespace '%s'", ns))
	ageKeyFile := strings.TrimSpace(os.Getenv("E2E_AGE_KEY_FILE"))
	Expect(ageKeyFile).NotTo(BeEmpty(), "E2E_AGE_KEY_FILE must be set by BeforeSuite")
	out, err := kubectlRunInNamespace(ns, "create", "secret", "generic", e2eEncryptionRefName,
		"--from-file=identity.agekey="+ageKeyFile,
		"--dry-run=client", "-o", "yaml")
	Expect(err).NotTo(HaveOccurred(), "failed to generate sops-age-key manifest")
	_, err = kubectlRunWithStdin(ns, out, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred(), "failed to apply sops-age-key to test namespace")
}

// cleanupGitTarget deletes a GitTarget resource.
func cleanupGitTarget(name, namespace string) {
	if skipCleanupBecauseResourcesArePreserved(fmt.Sprintf("GitTarget %s/%s", namespace, name)) {
		return
	}
	By(fmt.Sprintf("cleaning up GitTarget '%s' in ns '%s'", name, namespace))
	_, _ = kubectlRunInNamespace(namespace, "delete", "gittarget", name, "--ignore-not-found=true")
}

// cleanupWatchRule deletes a WatchRule resource.
func cleanupWatchRule(name, namespace string) {
	if skipCleanupBecauseResourcesArePreserved(fmt.Sprintf("WatchRule %s/%s", namespace, name)) {
		return
	}
	By(fmt.Sprintf("cleaning up WatchRule '%s' in ns '%s'", name, namespace))
	_, _ = kubectlRunInNamespace(namespace, "delete", "watchrule", name, "--ignore-not-found=true")
}

// cleanupClusterWatchRule deletes a ClusterWatchRule resource.
func cleanupClusterWatchRule(name string) {
	if skipCleanupBecauseResourcesArePreserved(fmt.Sprintf("ClusterWatchRule %s", name)) {
		return
	}
	By(fmt.Sprintf("cleaning up ClusterWatchRule '%s'", name))
	_, _ = kubectlRun("delete", "clusterwatchrule", name, "--ignore-not-found=true")
}

func cleanupNamespace(name string) {
	if skipCleanupBecauseResourcesArePreserved(fmt.Sprintf("namespace %s", name)) {
		return
	}
	By(fmt.Sprintf("deleting test namespace '%s'", name))
	_, _ = kubectlRun("delete", "namespace", name, "--ignore-not-found=true")
}

func cleanupNamespacedResource(namespace, resource, name string) {
	if skipCleanupBecauseResourcesArePreserved(fmt.Sprintf("%s %s/%s", resource, namespace, name)) {
		return
	}
	_, _ = kubectlRunInNamespace(namespace, "delete", resource, name, "--ignore-not-found=true")
}

func cleanupClusterResource(resource, name string) {
	if skipCleanupBecauseResourcesArePreserved(fmt.Sprintf("%s %s", resource, name)) {
		return
	}
	_, _ = kubectlRun("delete", resource, name, "--ignore-not-found=true")
}

func controllerPodNames() ([]string, error) {
	output, err := kubectlRunInNamespace(
		namespace,
		"get",
		"pods",
		"-l",
		controllerPodLabelSelector,
		"-o",
		"go-template={{ range .items }}{{ if not .metadata.deletionTimestamp }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}{{ end }}",
	)
	if err != nil {
		return nil, err
	}

	podNames := utils.GetNonEmptyLines(output)
	sort.Strings(podNames)
	return podNames, nil
}

func dumpFailureDiagnostics() {
	if !CurrentSpecReport().Failed() {
		return
	}

	By("Fetching Kubernetes events")
	eventsOutput, err := kubectlRunInNamespace(
		namespace,
		"get",
		"events",
		"--sort-by=.metadata.creationTimestamp",
	)
	if err == nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events (newest last):\n%s", eventsOutput)
	} else {
		_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s\n", err)
	}

	podNames, err := controllerPodNames()
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get controller pod names: %s\n", err)
		return
	}
	if len(podNames) == 0 {
		_, _ = fmt.Fprintf(
			GinkgoWriter,
			"No controller pods found with selector %q in namespace %q\n",
			controllerPodLabelSelector,
			namespace,
		)
		return
	}

	for _, podName := range podNames {
		By(fmt.Sprintf("Fetching controller manager pod logs for %s", podName))
		controllerLogs, logsErr := kubectlRunInNamespace(
			namespace,
			"logs",
			podName,
			"--tail=200",
		)
		if logsErr == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "Last 200 lines of controller logs for %s:\n%s", podName, controllerLogs)
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get controller logs for %s: %s\n", podName, logsErr)
		}

		By(fmt.Sprintf("Fetching controller manager pod description for %s", podName))
		podDescription, describeErr := kubectlRunInNamespace(namespace, "describe", "pod", podName)
		if describeErr == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "Pod description for %s:\n%s", podName, podDescription)
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "Failed to describe controller pod %s: %s\n", podName, describeErr)
		}
	}
}
