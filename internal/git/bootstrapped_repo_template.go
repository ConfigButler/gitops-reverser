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

package git

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

const (
	sopsConfigFileName         = ".sops.yaml"
	bootstrapTemplateDir       = "bootstrapped-repo-template"
	bootstrapCommitMessageRoot = "chore(bootstrap): initialize path"
	bootstrapCommitAuthorName  = "GitOps Reverser"
	bootstrapCommitAuthorEmail = "noreply@configbutler.ai"
	bootstrapTemplateFilePerm  = 0600
)

//go:embed bootstrapped-repo-template/* bootstrapped-repo-template/.sops.yaml
var bootstrapTemplateFS embed.FS

var errFoundFileInPath = errors.New("found file in path")

type bootstrapTemplateData struct {
	AgeRecipient string
}

type pathBootstrapOptions struct {
	TemplateData      bootstrapTemplateData
	IncludeSOPSConfig bool
}

func commitPathBootstrapTemplateIfNeeded(
	ctx context.Context,
	repo *gogit.Repository,
	branch plumbing.ReferenceName,
	targetPath string,
	options pathBootstrapOptions,
	auth transport.AuthMethod,
) (plumbing.Hash, bool, error) {
	worktree, err := repo.Worktree()
	if err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("failed to get worktree: %w", err)
	}

	if err := stageBootstrapTemplateInPath(worktree, targetPath, options); err != nil {
		return plumbing.ZeroHash, false, err
	}

	status, err := worktree.Status()
	if err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("failed to get worktree status: %w", err)
	}
	if status.IsClean() {
		return plumbing.ZeroHash, false, nil
	}

	baseHash, err := TryReference(repo, branch)
	if err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("failed to resolve branch reference: %w", err)
	}

	hash, err := worktree.Commit(bootstrapCommitMessage(targetPath), &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  bootstrapCommitAuthorName,
			Email: bootstrapCommitAuthorEmail,
			When:  time.Now(),
		},
		Committer: &object.Signature{
			Name:  bootstrapCommitAuthorName,
			Email: bootstrapCommitAuthorEmail,
			When:  time.Now(),
		},
	})
	if err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("failed to commit bootstrap files: %w", err)
	}

	if err := PushAtomic(ctx, repo, baseHash, branch, auth); err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("failed to push bootstrap commit: %w", err)
	}

	return hash, true, nil
}

func bootstrapCommitMessage(targetPath string) string {
	if targetPath == "" {
		return bootstrapCommitMessageRoot + " <root>"
	}
	return fmt.Sprintf("%s %s", bootstrapCommitMessageRoot, targetPath)
}

func pathHasAnyFile(repoPath, targetPath string) (bool, error) {
	basePath := repoPath
	if targetPath != "" {
		basePath = filepath.Join(repoPath, targetPath)
	}

	err := filepath.WalkDir(basePath, func(currentPath string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if filepath.Clean(currentPath) == filepath.Join(repoPath, ".git") {
				return filepath.SkipDir
			}
			return nil
		}
		return errFoundFileInPath
	})
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		if errors.Is(err, errFoundFileInPath) {
			return true, nil
		}
		return false, err
	}

	return false, nil
}

func stageBootstrapTemplateInPath(worktree *gogit.Worktree, targetPath string, options pathBootstrapOptions) error {
	entries, err := fs.ReadDir(bootstrapTemplateFS, bootstrapTemplateDir)
	if err != nil {
		return fmt.Errorf("failed to read bootstrap template directory: %w", err)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	targetDir, err := bootstrapTargetDirectory(worktree, targetPath)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if shouldSkipBootstrapEntry(entry, options) {
			continue
		}
		if err := stageBootstrapTemplateEntry(worktree, targetPath, targetDir, entry.Name(), options); err != nil {
			return err
		}
	}

	return nil
}

func bootstrapTargetDirectory(worktree *gogit.Worktree, targetPath string) (string, error) {
	root := worktree.Filesystem.Root()
	if targetPath == "" {
		return root, nil
	}

	targetDir := filepath.Join(root, targetPath)
	if err := os.MkdirAll(targetDir, 0750); err != nil {
		return "", fmt.Errorf("failed to create bootstrap target path %s: %w", targetPath, err)
	}

	return targetDir, nil
}

func shouldSkipBootstrapEntry(entry fs.DirEntry, options pathBootstrapOptions) bool {
	if entry.IsDir() {
		return true
	}
	return entry.Name() == sopsConfigFileName && !options.IncludeSOPSConfig
}

func stageBootstrapTemplateEntry(
	worktree *gogit.Worktree,
	targetPath string,
	targetDir string,
	entryName string,
	options pathBootstrapOptions,
) error {
	content, err := readBootstrapTemplateContent(entryName, options)
	if err != nil {
		return err
	}

	destinationPath := filepath.Join(targetDir, entryName)
	if err := writeBootstrapFileIfMissing(destinationPath, entryName, content); err != nil {
		return err
	}

	return stageBootstrapFile(worktree, targetPath, entryName)
}

func readBootstrapTemplateContent(entryName string, options pathBootstrapOptions) ([]byte, error) {
	templatePath := path.Join(bootstrapTemplateDir, entryName)
	content, err := bootstrapTemplateFS.ReadFile(templatePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read bootstrap template %s: %w", entryName, err)
	}

	if entryName != sopsConfigFileName {
		return content, nil
	}

	rendered, err := renderSOPSBootstrapTemplate(content, options.TemplateData)
	if err != nil {
		return nil, err
	}

	return rendered, nil
}

func writeBootstrapFileIfMissing(destinationPath, entryName string, content []byte) error {
	if _, err := os.Stat(destinationPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to stat bootstrap target %s: %w", entryName, err)
	}

	if err := os.WriteFile(destinationPath, content, bootstrapTemplateFilePerm); err != nil {
		return fmt.Errorf("failed to write bootstrap file %s: %w", entryName, err)
	}

	return nil
}

func stageBootstrapFile(worktree *gogit.Worktree, targetPath, entryName string) error {
	gitPath := entryName
	if targetPath != "" {
		gitPath = filepath.ToSlash(filepath.Join(targetPath, entryName))
	}

	if _, err := worktree.Add(gitPath); err != nil {
		return fmt.Errorf("failed to stage bootstrap file %s: %w", entryName, err)
	}

	return nil
}

func renderSOPSBootstrapTemplate(raw []byte, data bootstrapTemplateData) ([]byte, error) {
	if strings.TrimSpace(data.AgeRecipient) == "" {
		return nil, fmt.Errorf("failed to render bootstrap file %s: missing age recipient", sopsConfigFileName)
	}

	tmpl, err := template.New(sopsConfigFileName).Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("failed to parse bootstrap template %s: %w", sopsConfigFileName, err)
	}

	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, data); err != nil {
		return nil, fmt.Errorf("failed to render bootstrap template %s: %w", sopsConfigFileName, err)
	}

	return rendered.Bytes(), nil
}
