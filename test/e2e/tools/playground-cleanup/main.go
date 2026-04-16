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

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ConfigButler/gitops-reverser/internal/giteaclient"
)

const cleanupTimeout = 30 * time.Second

const (
	playgroundGiteaAdminPass = "giteapassword123"
	playgroundGiteaAdminUser = "giteaadmin"
	playgroundGiteaOrg       = "testorg"
	playgroundGiteaURL       = "http://localhost:13000/api/v1"
	playgroundNamespace      = "tilt-playground"
	playgroundRepoName       = "playground"
)

type cleanupOptions struct {
	adminPass  string
	adminUser  string
	context    string
	giteaURL   string
	namespace  string
	orgName    string
	projectDir string
	repoName   string
}

func main() {
	opts := cleanupOptions{}

	flag.StringVar(&opts.context, "context", envOr("CTX", "k3d-gitops-reverser-test-e2e"), "kubectl context")
	flag.StringVar(&opts.namespace, "namespace", playgroundNamespace, "namespace")
	flag.StringVar(&opts.repoName, "repo", playgroundRepoName, "repo name")
	flag.StringVar(&opts.orgName, "org", playgroundGiteaOrg, "Gitea organization")
	flag.StringVar(
		&opts.giteaURL,
		"gitea-url",
		playgroundGiteaURL,
		"Gitea API base URL",
	)
	flag.StringVar(&opts.adminUser, "admin-user", playgroundGiteaAdminUser, "Gitea admin user")
	flag.StringVar(
		&opts.adminPass,
		"admin-pass",
		playgroundGiteaAdminPass,
		"Gitea admin password",
	)
	flag.StringVar(&opts.projectDir, "project-dir", "", "project root (default: current working directory)")
	flag.Parse()

	if err := runCleanup(opts); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "playground cleanup failed: %v\n", err)
		os.Exit(1)
	}
}

func runCleanup(opts cleanupOptions) error {
	projectDir := strings.TrimSpace(opts.projectDir)
	if projectDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("get working directory: %w", err)
		}
		projectDir = wd
	}

	if err := deleteNamespace(projectDir, opts.context, opts.namespace); err != nil {
		return err
	}

	client := giteaclient.New(opts.giteaURL, opts.adminUser, opts.adminPass)
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	if err := client.DeleteRepo(ctx, opts.orgName, opts.repoName); err != nil {
		return fmt.Errorf("delete Gitea repo %s/%s: %w", opts.orgName, opts.repoName, err)
	}

	key, found, err := client.FindUserKeyByTitle(ctx, opts.adminUser, transportSSHKeyTitle(opts.repoName))
	if err != nil {
		return fmt.Errorf("find Gitea admin SSH key for repo %s: %w", opts.repoName, err)
	}
	if found {
		if err := client.DeleteUserKeyAsAdmin(ctx, opts.adminUser, key.ID); err != nil {
			return fmt.Errorf("delete Gitea admin SSH key %d: %w", key.ID, err)
		}
	}

	if err := os.RemoveAll(filepath.Join(projectDir, ".stamps", "repos", opts.repoName)); err != nil {
		return fmt.Errorf("remove repo checkout: %w", err)
	}
	if err := os.RemoveAll(
		filepath.Join(projectDir, ".stamps", "e2e-repo-artifacts", opts.namespace, opts.repoName),
	); err != nil {
		return fmt.Errorf("remove repo artifacts: %w", err)
	}

	_, _ = fmt.Fprintf(
		os.Stdout,
		"Playground cleanup complete: namespace=%s repo=%s projectDir=%s\n",
		opts.namespace,
		opts.repoName,
		projectDir,
	)
	return nil
}

func deleteNamespace(projectDir, kubeContext, namespace string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	args := []string{}
	if strings.TrimSpace(kubeContext) != "" {
		args = append(args, "--context", kubeContext)
	}
	args = append(args, "delete", "namespace", namespace, "--ignore-not-found=true")

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Dir = projectDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("kubectl delete namespace %s: %w: %s", namespace, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func transportSSHKeyTitle(repoName string) string {
	return "E2E Transport Key " + strings.TrimSpace(repoName)
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
