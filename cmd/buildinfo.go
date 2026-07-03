// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"runtime"
)

// Build information, injected via -ldflags "-X main.<var>=<value>" at build
// time (see Dockerfile and the docker-build task). The defaults below are what
// a plain `go build`/`go run` produces, which is useful to spot a non-release
// binary at a glance.
var (
	version   = "dev"
	gitCommit = "unknown"
	gitDirty  = "0"
	buildDate = "unknown"
)

// buildInfo is the build metadata reported at startup and by the /build-info
// endpoint. It lets an operator confirm a running pod is the build they expect.
type buildInfo struct {
	Version         string `json:"version"`
	GitCommit       string `json:"gitCommit"`
	IsDirty         bool   `json:"isDirty"`
	CommitWithDirty string `json:"commitWithDirty"`
	BuildDate       string `json:"buildDate"`
	GoVersion       string `json:"goVersion"`
}

// currentBuildInfo assembles the build metadata from the ldflags-injected vars.
func currentBuildInfo() buildInfo {
	dirty := gitDirty == "1"
	commitWithDirty := gitCommit
	if dirty {
		commitWithDirty = gitCommit + "-dirty"
	}
	return buildInfo{
		Version:         version,
		GitCommit:       gitCommit,
		IsDirty:         dirty,
		CommitWithDirty: commitWithDirty,
		BuildDate:       buildDate,
		GoVersion:       runtime.Version(),
	}
}

// buildInfoHandler serves the build metadata as JSON on GET requests. It is
// registered as an extra handler on the metrics server (see main).
func buildInfoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(currentBuildInfo())
	})
}
