package git

import (
	"fmt"

	"github.com/ConfigButler/gitops-reverser/internal/eventqueue"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// CommitFile represents a single file to be committed.
type CommitFile struct {
	Path    string
	Content []byte
}

// Repo represents a Git repository.
type Repo struct {
	*git.Repository
	path string
}

// Clone clones a Git repository to a temporary directory.
func Clone(url, path string, auth transport.AuthMethod) (*Repo, error) {
	// For now, we'll just return a dummy repo.
	fmt.Printf("Cloning %s to %s\n", url, path)
	return &Repo{path: path}, nil
}

// GetAuthMethod returns an SSH public key authentication method from a secret.
func GetAuthMethod(privateKey, password string) (transport.AuthMethod, error) {
	return ssh.NewPublicKeys("git", []byte(privateKey), password)
}

// Commit commits a set of files to the repository.
func (r *Repo) Commit(files []CommitFile, message string) error {
	// For now, we'll just log the commit.
	fmt.Printf("Committing %d files with message: %s\n", len(files), message)
	for _, f := range files {
		fmt.Printf("  - %s\n", f.Path)
	}
	return nil
}

// Push pushes the changes to the remote repository.
func (r *Repo) Push() error {
	// For now, we'll just log the push.
	fmt.Println("Pushing changes to remote")
	return nil
}

// GetFilePath returns the path to a file in the repository for a given object.
func GetFilePath(obj *unstructured.Unstructured) string {
	if obj.GetNamespace() != "" {
		return fmt.Sprintf("namespaces/%s/%s/%s.yaml", obj.GetNamespace(), obj.GetKind(), obj.GetName())
	}
	return fmt.Sprintf("cluster-scoped/%s/%s.yaml", obj.GetKind(), obj.GetName())
}

// GetCommitMessage returns a structured commit message for the given event.
func GetCommitMessage(event eventqueue.Event) string {
	return fmt.Sprintf("[%s] %s/%s in ns/%s by user/%s",
		event.Request.Operation,
		event.Object.GetKind(),
		event.Object.GetName(),
		event.Object.GetNamespace(),
		event.Request.UserInfo.Username,
	)
}
