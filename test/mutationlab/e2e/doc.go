// SPDX-License-Identifier: Apache-2.0

// Package e2e drives the mutation-capture lab against a live cluster: it runs
// each scenario, drains the lab's records API, and compares (or, with
// MUTATIONLAB_UPDATE=1, rewrites) the golden corpus.
//
// The driver is behind the `mutationlab_e2e` build tag so it never compiles into
// the default `go test ./...` unit lane (which has no cluster). This file is the
// untagged placeholder that keeps the package buildable when the tag is absent;
// `task lab-e2e` builds it with -tags mutationlab_e2e.
package e2e
