// SPDX-License-Identifier: Apache-2.0

// manifest-analyzer is a standalone, read-only CLI that analyzes a folder of
// Kubernetes manifests. It is the proof-of-concept consumer of the
// internal/manifestanalyzer library described in
// docs/design/manifest/current-manifest-support-review.md. It writes nothing; it
// only reports what it finds.
//
// Usage:
//
//	manifest-analyzer [flags] <dir>
//	manifest-analyzer --mode discovery [flags]
//
//	--mode   analyze|scan|discovery  what to produce (default analyze)
//	                                 analyze: the structural report (files, GVK inventory)
//	                                 scan:    the adoption dry-run (acceptance + plan), the
//	                                          shared scan-mode pipeline with no flush
//	                                 discovery: raw Kubernetes API discovery dump
//	--format text|json     output format (default text)
//	--policy report|refuse
//	                       report: always exit 0 (analysis only)
//	                       refuse: exit 1 when the folder would be refused
//	                               (analyze: any acceptance issue; scan: not accepted)
//
// The tool is structure-only and needs no cluster: it reports duplicate
// identities, KRM vs. non-KRM classification, multi-document files, and the
// inventory of every GVK found. Scan mode additionally applies the non-API KRM
// allowlist (kustomization.yaml is retained, not flagged), runs the full adoption
// acceptance gate, and renders the plan — which is empty here because no cluster
// state is available to compare against.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
)

// Process exit codes.
const (
	exitOK      = 0 // success
	exitRefused = 1 // acceptance issues found under --policy refuse
	exitUsage   = 2 // usage or I/O error
)

type discoveryClient interface {
	ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error)
}

type discoveryDump struct {
	Groups              []*metav1.APIGroup        `json:"groups"`
	Resources           []*metav1.APIResourceList `json:"resources"`
	FailedGroupVersions map[string]string         `json:"failedGroupVersions,omitempty"`
	Error               string                    `json:"error,omitempty"`
}

type discoveryClientFactory func(kubeconfig, contextName string) (discoveryClient, error)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable entry point. It returns one of the exit* codes.
func run(args []string, stdout, stderr io.Writer) int {
	return runWithDiscoveryClientFactory(args, stdout, stderr, newKubeDiscoveryClient)
}

func runWithDiscoveryClientFactory(
	args []string,
	stdout, stderr io.Writer,
	newClient discoveryClientFactory,
) int {
	fs := flag.NewFlagSet("manifest-analyzer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	mode := fs.String("mode", "analyze", "what to produce: analyze|scan|discovery")
	format := fs.String("format", "text", "output format: text|json")
	policy := fs.String("policy", "report", "adoption policy: report|refuse")
	kubeconfig := fs.String(
		"kubeconfig",
		"",
		"kubeconfig path for --mode discovery (default: standard loading rules)",
	)
	contextName := fs.String("context", "", "kubeconfig context for --mode discovery")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: manifest-analyzer [flags] <dir>")
		fmt.Fprintln(stderr, "       manifest-analyzer --mode discovery [flags]")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *mode != "analyze" && *mode != "scan" && *mode != "discovery" {
		fmt.Fprintf(stderr, "error: unknown mode %q (want analyze|scan|discovery)\n", *mode)
		return exitUsage
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "error: unknown format %q (want text|json)\n", *format)
		return exitUsage
	}
	if *policy != "report" && *policy != "refuse" {
		fmt.Fprintf(stderr, "error: unknown policy %q (want report|refuse)\n", *policy)
		return exitUsage
	}
	if *mode == "discovery" {
		if fs.NArg() != 0 {
			fmt.Fprintln(stderr, "error: discovery mode does not accept a directory argument")
			fs.Usage()
			return exitUsage
		}
		return runDiscovery(*kubeconfig, *contextName, stdout, stderr, newClient)
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "error: exactly one directory argument is required")
		fs.Usage()
		return exitUsage
	}

	if *mode == "scan" {
		return runScan(fs.Arg(0), *format, *policy, stdout, stderr)
	}
	return runAnalyze(fs.Arg(0), *format, *policy, stdout, stderr)
}

// runAnalyze renders the structural report and applies the refuse policy over its
// acceptance issues.
func runAnalyze(dir, format, policy string, stdout, stderr io.Writer) int {
	rep, err := manifestanalyzer.AnalyzeDir(dir)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitUsage
	}

	if format == "json" {
		if err := manifestanalyzer.RenderJSON(stdout, rep); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return exitUsage
		}
	} else {
		manifestanalyzer.RenderText(stdout, rep)
	}

	if policy == "refuse" && len(rep.Issues) > 0 {
		return exitRefused
	}
	return exitOK
}

func runDiscovery(
	kubeconfig, contextName string,
	stdout, stderr io.Writer,
	newClient discoveryClientFactory,
) int {
	client, err := newClient(kubeconfig, contextName)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitUsage
	}
	groups, resources, err := client.ServerGroupsAndResources()
	dump := discoveryDump{
		Groups:    groups,
		Resources: resources,
	}
	if err != nil {
		failed, ok := discovery.GroupDiscoveryFailedErrorGroups(err)
		if !ok {
			fmt.Fprintf(stderr, "error: discover API resources: %v\n", err)
			return exitUsage
		}
		dump.FailedGroupVersions = failedGroupVersions(failed)
		dump.Error = err.Error()
	}
	if err := json.NewEncoder(stdout).Encode(dump); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitUsage
	}
	return exitOK
}

func newKubeDiscoveryClient(kubeconfig, contextName string) (discoveryClient, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	client, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create discovery client: %w", err)
	}
	return client, nil
}

func failedGroupVersions(failed map[schema.GroupVersion]error) map[string]string {
	out := make(map[string]string, len(failed))
	for gv, err := range failed {
		out[gv.String()] = err.Error()
	}
	return out
}

// runScan runs the adoption dry-run (the shared scan-mode pipeline) and applies the
// refuse policy over the acceptance decision. It is structure-only: no cluster
// state, so the plan is empty, but the acceptance gate is the full one — it applies
// the non-API KRM allowlist and the impure-managed-file / mixed-file refusals.
func runScan(dir, format, policy string, stdout, stderr io.Writer) int {
	scanPolicy := manifestanalyzer.ScanPolicy{
		Acceptance: manifestanalyzer.AcceptancePolicy{Allowlist: manifestanalyzer.DefaultAllowlist()},
	}
	result, err := manifestanalyzer.ScanDir(context.Background(), dir, nil, nil, scanPolicy)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitUsage
	}

	if format == "json" {
		if err := manifestanalyzer.RenderScanJSON(stdout, result); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return exitUsage
		}
	} else {
		manifestanalyzer.RenderScanText(stdout, result)
	}

	if policy == "refuse" && !result.Acceptance.Accepted {
		return exitRefused
	}
	return exitOK
}
