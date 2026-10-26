// SPDX-License-Identifier: Apache-2.0

package git

import (
	"os"
	"path"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderSOPSBootstrapTemplate_MultipleRecipients(t *testing.T) {
	raw, err := bootstrapTemplateFS.ReadFile(path.Join(bootstrapTemplateDir, sopsConfigFileName))
	require.NoError(t, err)

	rendered, err := renderSOPSBootstrapTemplate(raw, bootstrapTemplateData{
		AgeRecipients: []string{
			"age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq7k8m6",
			"age1yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyv0r2a",
		},
	})
	require.NoError(t, err)
	assert.Contains(t, string(rendered), "age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq7k8m6")
	assert.Contains(t, string(rendered), "age1yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyv0r2a")
}

func TestRenderSOPSBootstrapTemplate_MissingRecipients(t *testing.T) {
	raw, err := bootstrapTemplateFS.ReadFile(path.Join(bootstrapTemplateDir, sopsConfigFileName))
	require.NoError(t, err)

	_, err = renderSOPSBootstrapTemplate(raw, bootstrapTemplateData{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing age recipients")
}

// bootstrapRepo is an empty local repository to stage a folder's bootstrap files into.
func bootstrapRepo(t *testing.T) (*gogit.Repository, string) {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	require.NoError(t, err)
	return repo, dir
}

// The folder bootstrap the write path stages before a GitTarget's first document: a README and the
// commented .gittargetignore always, a .sops.yaml only once encryption resolves recipients (it is
// added on a later write, not withheld forever), every recipient rendered, and a file somebody
// already wrote never overwritten. Staging twice is a no-op.
func TestEnsureBootstrapTemplateInPath_StagesTheFolderFiles(t *testing.T) {
	repo, dir := bootstrapRepo(t)
	folder := filepath.Join(dir, "clusters/dev")
	require.NoError(t, os.MkdirAll(folder, 0o750))
	custom := "# custom readme\n"
	require.NoError(t, os.WriteFile(filepath.Join(folder, "README.md"), []byte(custom), 0o600))

	plain := buildBootstrapOptions(nil)
	require.NoError(t, ensureBootstrapTemplateInPath(repo, "clusters/dev", plain))
	require.NoError(t, ensureBootstrapTemplateInPath(repo, "clusters/dev", plain))

	readme, err := os.ReadFile(filepath.Join(folder, "README.md"))
	require.NoError(t, err)
	assert.Equal(t, custom, string(readme), "an existing file is never overwritten")
	_, err = os.Stat(filepath.Join(folder, gitTargetIgnoreFileName))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(folder, sopsConfigFileName))
	assert.True(t, os.IsNotExist(err), "no .sops.yaml before encryption resolves recipients")

	recipients := []string{
		"age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq7k8m6",
		"age1yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyv0r2a",
	}
	encrypted := buildBootstrapOptions(&ResolvedEncryptionConfig{AgeRecipients: recipients})
	require.NoError(t, ensureBootstrapTemplateInPath(repo, "clusters/dev", encrypted))

	sops, err := os.ReadFile(filepath.Join(folder, sopsConfigFileName))
	require.NoError(t, err, "encryption configured later adds the .sops.yaml")
	for _, recipient := range recipients {
		assert.Contains(t, string(sops), recipient)
	}
}

// A disabled bootstrap stages nothing at all.
func TestEnsureBootstrapTemplateInPath_DisabledStagesNothing(t *testing.T) {
	repo, dir := bootstrapRepo(t)
	require.NoError(t, ensureBootstrapTemplateInPath(repo, "clusters/dev", pathBootstrapOptions{}))
	_, err := os.Stat(filepath.Join(dir, "clusters/dev"))
	assert.True(t, os.IsNotExist(err))
}
