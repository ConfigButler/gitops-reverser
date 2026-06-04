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

// manifest-analyzer is a standalone, read-only CLI that analyzes a folder of
// Kubernetes manifests. It is the proof-of-concept consumer of the
// internal/manifestanalyzer library described in
// docs/design/manifest/current-manifest-support-review.md. It writes nothing; it
// only reports what it finds.
//
// Usage:
//
//	manifest-analyzer [flags] <dir>
//
//	--format text|json   output format (default text)
//	--policy report|refuse
//	                     report: always exit 0 (analysis only)
//	                     refuse: exit 1 when any acceptance issue is found
//	--watched g/v/k,...  comma-separated watched GVKs; enables watched vs
//	                     unwatched classification. Without it, KRM is "unknown".
//
// Without --watched the tool runs structure-only: duplicate detection, KRM vs
// non-KRM classification, multi-document inspection, and a GVK inventory all
// work with no cluster access.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
)

// Process exit codes.
const (
	exitOK      = 0 // success
	exitRefused = 1 // acceptance issues found under --policy refuse
	exitUsage   = 2 // usage or I/O error
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable entry point. It returns one of the exit* codes.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("manifest-analyzer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	format := fs.String("format", "text", "output format: text|json")
	policy := fs.String("policy", "report", "adoption policy: report|refuse")
	watched := fs.String("watched", "", "comma-separated watched GVKs (group/version/kind)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: manifest-analyzer [flags] <dir>")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
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
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "error: exactly one directory argument is required")
		fs.Usage()
		return exitUsage
	}

	opts, err := buildOptions(*watched)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitUsage
	}

	rep, err := manifestanalyzer.AnalyzeDir(fs.Arg(0), opts)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitUsage
	}

	if *format == "json" {
		if err := manifestanalyzer.RenderJSON(stdout, rep); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return exitUsage
		}
	} else {
		manifestanalyzer.RenderText(stdout, rep)
	}

	if *policy == "refuse" && len(rep.Issues) > 0 {
		return exitRefused
	}
	return exitOK
}

// buildOptions turns the --watched flag into analyzer options. An empty flag
// means no WatchSource (structure-only analysis).
func buildOptions(watched string) (manifestanalyzer.Options, error) {
	watched = strings.TrimSpace(watched)
	if watched == "" {
		return manifestanalyzer.Options{}, nil
	}
	var gvks []manifestanalyzer.GVK
	for _, ref := range strings.Split(watched, ",") {
		if strings.TrimSpace(ref) == "" {
			continue
		}
		g, err := manifestanalyzer.ParseGVKRef(ref)
		if err != nil {
			return manifestanalyzer.Options{}, err
		}
		gvks = append(gvks, g)
	}
	return manifestanalyzer.Options{Watch: manifestanalyzer.NewStaticWatchSource(gvks)}, nil
}
