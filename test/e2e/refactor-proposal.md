# E2E Test Refactoring Proposal: Separate Templated YAML Files

## Current Issues
The current e2e tests in [`e2e_test.go`](e2e_test.go) use inline `fmt.Sprintf` to generate YAML and JSON for Kubernetes resources. This leads to:
- Poor readability due to long multi-line strings scattered in test functions.
- Difficulty in maintaining or validating YAML syntax.
- Hard to reuse resource definitions across tests.
- No separation between test logic and resource templates.

Examples:
- GitRepoConfig YAML in `createGitRepoConfigWithURL` (lines 600-611).
- WatchRule YAML in reconciliation test (lines 347-360).
- ConfigMap YAML in commit test (lines 444-451).
- ClusterRoleBinding YAML in `setupMetricsAccess` (lines 670-683).
- JSON pod spec in `createMetricsCurlPod` (lines 705-727).

## Proposed Solution
Move all inline YAML/JSON to separate template files in a new `test/e2e/templates/` directory. Use Go's `text/template` package to render these templates with dynamic values (e.g., names, namespaces, URLs). This improves:
- **Readability**: Clean test code focused on logic; templates are human-readable YAML with placeholders.
- **Maintainability**: Edit YAML in dedicated files; easy to validate syntax (e.g., with yamllint).
- **Reusability**: Share templates across tests.
- **Testability**: Unit test template rendering separately if needed.

### Step 1: Directory Structure
Create `test/e2e/templates/` with:
- `gitrepoconfig.tmpl`: For GitRepoConfig CRs.
- `watchrule.tmpl`: For WatchRule CRs (with variants for different rules).
- `configmap.tmpl`: For test ConfigMaps.
- `clusterrolebinding.tmpl`: For metrics RBAC.
- `curl-pod.json.tmpl`: For curl pod JSON overrides.

### Step 2: Template Examples
#### gitrepoconfig.tmpl
```
apiVersion: configbutler.ai/v1alpha1
kind: GitRepoConfig
metadata:
  name: {{.Name}}
  namespace: {{.Namespace}}
spec:
  repoUrl: {{.RepoURL}}
  branch: {{.Branch}}
  secretRef:
    name: {{.SecretName}}
```

#### watchrule.tmpl (for reconciliation test)
```
apiVersion: configbutler.ai/v1alpha1
kind: WatchRule
metadata:
  name: {{.Name}}
  namespace: {{.Namespace}}
spec:
  gitRepoConfigRef: {{.GitRepoConfigRef}}
  excludeLabels:
    matchExpressions:
    - key: "configbutler.ai/ignore"
      operator: Exists
  rules:
  - resources: ["deployments", "services", "configmaps", "secrets"]
  - resources: ["ingresses.*"]
```

#### watchrule-configmap.tmpl (simpler variant)
```
apiVersion: configbutler.ai/v1alpha1
kind: WatchRule
metadata:
  name: {{.Name}}
  namespace: {{.Namespace}}
spec:
  gitRepoConfigRef: {{.GitRepoConfigRef}}
  rules:
  - resources: ["configmaps"]
```

#### configmap.tmpl
```
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{.Name}}
  namespace: {{.Namespace}}
data:
  test-key: test-value
  another-key: another-value
```

#### clusterrolebinding.tmpl
```
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: {{.Name}}
subjects:
- kind: ServiceAccount
  name: {{.ServiceAccountName}}
  namespace: {{.Namespace}}
roleRef:
  kind: ClusterRole
  name: gitops-reverser-metrics-reader
  apiGroup: rbac.authorization.k8s.io
```

#### curl-pod.json.tmpl
```
{
  "spec": {
    "containers": [{
      "name": "curl",
      "image": "curlimages/curl:latest",
      "command": ["/bin/sh", "-c"],
      "args": ["curl -v -k -H 'Authorization: Bearer {{.Token}}' https://{{.ServiceName}}.{{.Namespace}}.svc.cluster.local:8443/metrics"],
      "securityContext": {
        "readOnlyRootFilesystem": true,
        "allowPrivilegeEscalation": false,
        "capabilities": {
          "drop": ["ALL"]
        },
        "runAsNonRoot": true,
        "runAsUser": 1000,
        "seccompProfile": {
          "type": "RuntimeDefault"
        }
      }
    }],
    "serviceAccountName": "{{.ServiceAccountName}}"
  }
}
```

### Step 3: Update Test Code
- Import `text/template` and `io/ioutil` (or `os` for file reading).
- Create a helper function to load and render templates, e.g.:
  ```go
  func renderTemplate(templatePath string, data interface{}) (string, error) {
    tmpl, err := template.ParseFiles(templatePath)
    if err != nil {
      return "", err
    }
    var buf bytes.Buffer
    if err := tmpl.Execute(&buf, data); err != nil {
      return "", err
    }
    return buf.String(), nil
  }
  ```
- In tests, e.g., for GitRepoConfig:
  ```go
  data := struct {
    Name        string
    Namespace   string
    RepoURL     string
    Branch      string
    SecretName  string
  }{
    Name:       name,
    Namespace:  namespace,
    RepoURL:    repoURL,
    Branch:     branch,
    SecretName: secretName,
  }
  yamlContent, err := renderTemplate("test/e2e/templates/gitrepoconfig.tmpl", data)
  // Then use yamlContent with kubectl apply
  ```
- Apply similarly to other functions/tests. For JSON (curl pod), parse and use the rendered string in `--overrides`.

### Step 4: Implementation Steps
1. Create `test/e2e/templates/` directory and add .tmpl files as above.
2. Add template rendering helper to e2e_test.go or utils.go.
3. Refactor each inline YAML/JSON usage to use renderTemplate.
4. Remove old fmt.Sprintf blocks.
5. Run `make test-e2e` to verify no regressions.
6. Update documentation in TESTING.md if needed.

### Benefits
- Templates are valid YAML/JSON files that can be linted independently.
- Easier to add new resource variants without bloating test files.
- Improves diff visibility in Git for template changes.
- Reduces cognitive load when reading/modifying tests.

### Potential Drawbacks
- Adds dependency on template files; tests must ensure templates exist.
- Slight performance overhead (negligible for e2e).
- Need to handle template errors gracefully in tests.

This proposal maintains full functionality while significantly improving code quality. Ready for implementation after review.