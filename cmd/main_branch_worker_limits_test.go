// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/git"
)

func parseLimitsArgs(t *testing.T, args ...string) (appConfig, error) {
	t.Helper()
	return parseFlagsWithArgs(flag.NewFlagSet("manager", flag.ContinueOnError), args)
}

// The queue depth was a compile-time constant, and the deployments that overrun it are the
// ones whose shape the constant was never sized against: a branch worker is shared by every
// GitTarget on a (GitProvider, branch), so the headroom an operator needs is a property of
// their cluster, not of this binary. Assert the knob exists and carries a value through.
func TestParseFlags_BranchWorkerQueueDepthIsConfigurable(t *testing.T) {
	cfg, err := parseLimitsArgs(t, "--branch-worker-queue-depth=4096")
	require.NoError(t, err)
	require.Equal(t, 4096, cfg.branchWorkerLimits.QueueDepth)
}

func TestParseFlags_BranchWorkerLimitsDefault(t *testing.T) {
	cfg, err := parseLimitsArgs(t)
	require.NoError(t, err)
	require.Equal(t, git.DefaultBranchWorkerQueueDepth, cfg.branchWorkerLimits.QueueDepth,
		"an unset queue size must take the package default, not zero")
	require.Equal(t, git.DefaultBranchBufferMaxBytes, cfg.branchWorkerLimits.MaxBufferBytes)
}

// Zero is refused rather than defaulted. An unbuffered channel drops every write that does
// not find the event loop already waiting, which presents as near-total silent loss in the
// watch path rather than as the number the operator typed.
func TestParseFlags_BranchWorkerQueueDepthRejectsNonPositive(t *testing.T) {
	for _, arg := range []string{"--branch-worker-queue-depth=0", "--branch-worker-queue-depth=-1"} {
		_, err := parseLimitsArgs(t, arg)
		require.ErrorContains(t, err, "--branch-worker-queue-depth must be > 0", "arg %q", arg)
	}
}

// The env-var seam is how the container image carries a default without the chart having to
// render the flag, mirroring BRANCH_BUFFER_MAX_SIZE.
func TestParseFlags_BranchWorkerQueueDepthFromEnv(t *testing.T) {
	t.Setenv("BRANCH_WORKER_QUEUE_DEPTH", "2500")
	cfg, err := parseLimitsArgs(t)
	require.NoError(t, err)
	require.Equal(t, 2500, cfg.branchWorkerLimits.QueueDepth)

	t.Setenv("BRANCH_WORKER_QUEUE_DEPTH", "not-a-number")
	_, err = parseLimitsArgs(t)
	require.ErrorContains(t, err, "invalid BRANCH_WORKER_QUEUE_DEPTH")

	// An explicit flag still beats the environment.
	t.Setenv("BRANCH_WORKER_QUEUE_DEPTH", "2500")
	cfg, err = parseLimitsArgs(t, "--branch-worker-queue-depth=77")
	require.NoError(t, err)
	require.Equal(t, 77, cfg.branchWorkerLimits.QueueDepth)
}
