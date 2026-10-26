// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"os"
	"testing"

	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// TestMain gives this binary its own worker state root: the WorkerManagers these tests start sweep
// every directory under it that their GitBranches do not name (git.SetWorkerStateRoot).
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "gitops-reverser-workers-")
	if err != nil {
		panic(err)
	}
	git.SetWorkerStateRoot(root)

	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}
