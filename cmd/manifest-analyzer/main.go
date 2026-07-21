// SPDX-License-Identifier: Apache-2.0

// manifest-analyzer is a standalone, read-only CLI that analyzes a folder of
// Kubernetes manifests. It is the proof-of-concept consumer of the
// internal/manifestanalyzer library described in
// docs/spec/current-manifest-support-review.md. It writes nothing; it
// only reports what it finds.
//
// Usage:
//
//	manifest-analyzer [flags] <dir>
//	manifest-analyzer --mode scan-repo [flags] <repo-root>
//	manifest-analyzer --mode discovery [flags]
//
//	--mode   analyze|scan-folder|scan-repo|discovery  what to produce (default analyze)
//	                                 analyze:     the structural report (files, GVK inventory)
//	                                 scan-folder: may THIS folder become a GitTarget? The
//	                                              adoption dry-run (acceptance + plan), the
//	                                              shared scan pipeline with no flush
//	                                 scan-repo:   which folders under this repo root could
//	                                              become GitTargets? Classifies every
//	                                              candidate. Report-only: --policy is not
//	                                              applied (the repo-level refuse gate is
//	                                              deferred)
//	                                 discovery:   raw Kubernetes API discovery dump
//	--format text|json     output format (default text)
//	--policy report|refuse
//	                       report: always exit 0 (analysis only)
//	                       refuse: exit 1 when the folder would be refused
//	                               (analyze: any acceptance issue; scan-folder: not accepted)
//
// The tool is structure-only and needs no cluster: it reports duplicate
// identities, KRM vs. non-KRM classification, multi-document files, and the
// inventory of every GVK found. scan-folder additionally applies the non-API KRM
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
	publicanalyzer "github.com/ConfigButler/gitops-reverser/pkg/manifestanalyzer"
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
	mode := fs.String("mode", "analyze", "what to produce: analyze|scan-folder|scan-repo|discovery")
	format := fs.String("format", "text", "output format: text|json")
	policy := fs.String("policy", "report", "adoption policy: report|refuse (scan-repo is report-only)")
	kubeconfig := fs.String(
		"kubeconfig",
		"",
		"kubeconfig path for --mode discovery (default: standard loading rules)",
	)
	contextName := fs.String("context", "", "kubeconfig context for --mode discovery")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: manifest-analyzer [flags] <dir>")
		fmt.Fprintln(stderr, "       manifest-analyzer --mode scan-repo [flags] <repo-root>")
		fmt.Fprintln(stderr, "       manifest-analyzer --mode discovery [flags]")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if !validChoices(*mode, *format, *policy, stderr) {
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
	return runDirMode(*mode, fs.Arg(0), *format, *policy, stdout, stderr)
}

// validChoices validates the mode/format/policy enum flags, reporting the first bad one
// to stderr. Splitting it out of run keeps the top-level dispatch simple.
func validChoices(mode, format, policy string, stderr io.Writer) bool {
	switch {
	case mode != "analyze" && mode != "scan-folder" && mode != "scan-repo" && mode != "discovery":
		fmt.Fprintf(stderr, "error: unknown mode %q (want analyze|scan-folder|scan-repo|discovery)\n", mode)
	case format != "text" && format != "json":
		fmt.Fprintf(stderr, "error: unknown format %q (want text|json)\n", format)
	case policy != "report" && policy != "refuse":
		fmt.Fprintf(stderr, "error: unknown policy %q (want report|refuse)\n", policy)
	default:
		return true
	}
	return false
}

// runDirMode dispatches the directory-argument modes (everything but discovery).
func runDirMode(mode, dir, format, policy string, stdout, stderr io.Writer) int {
	switch mode {
	case "scan-folder":
		return runScanFolder(dir, format, policy, stdout, stderr)
	case "scan-repo":
		return runScanRepo(dir, format, stdout, stderr)
	default:
		return runAnalyze(dir, format, policy, stdout, stderr)
	}
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

// runScanFolder runs the adoption dry-run (the shared scan pipeline) and applies the
// refuse policy over the acceptance decision. It is structure-only: no cluster
// state, so the plan is empty, but the acceptance gate is the full one — it applies
// the non-API KRM allowlist and the impure-managed-file / mixed-file refusals.
//
// --format json goes through pkg/manifestanalyzer, so the CLI's machine-readable output
// and the published Go contract are the same document and cannot drift. Text output stays
// on the internal renderer, which can show the plan the public report deliberately omits.
func runScanFolder(dir, format, policy string, stdout, stderr io.Writer) int {
	if format == "json" {
		report, err := publicanalyzer.ScanFolder(context.Background(), dir)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return exitUsage
		}
		if err := report.WriteJSON(stdout); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return exitUsage
		}
		return scanExitCode(policy, report.Accepted)
	}

	scanPolicy := manifestanalyzer.ScanPolicy{
		Acceptance: manifestanalyzer.AcceptancePolicy{Allowlist: manifestanalyzer.DefaultAllowlist()},
		Plan:       manifestanalyzer.FolderScanPlanPolicy(),
	}
	result, err := manifestanalyzer.ScanDir(context.Background(), dir, nil, nil, scanPolicy)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitUsage
	}
	manifestanalyzer.RenderScanText(stdout, result)
	return scanExitCode(policy, result.Acceptance.Accepted)
}

func scanExitCode(policy string, accepted bool) int {
	if policy == "refuse" && !accepted {
		return exitRefused
	}
	return exitOK
}

// runScanRepo runs the whole-repo onboarding scan: walk every folder, enumerate
// candidate GitTarget subtrees, classify each one's layout and acceptance, and emit the
// report. It is read-only and needs no cluster. Exit codes stay simple for this cut
// (exitOK, or exitUsage on an I/O error); the repo-level --policy refuse gate is
// deferred per the design doc.
func runScanRepo(root, format string, stdout, stderr io.Writer) int {
	if format == "json" {
		report, err := publicanalyzer.ScanRepo(context.Background(), root)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return exitUsage
		}
		if err := report.WriteJSON(stdout); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return exitUsage
		}
		return exitOK
	}

	rep, err := manifestanalyzer.ScanRepo(context.Background(), root)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitUsage
	}
	manifestanalyzer.RenderRepoText(stdout, rep)
	return exitOK
}
