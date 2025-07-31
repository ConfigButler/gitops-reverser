# Product Requirements Document (PRD)
## ConfigButler GitOps Reverser

### 1. Introduction & Vision

**Vision:** The ConfigButler ecosystem aims to simplify configuration management by creating a seamless experience for humans, GitOps workflows, and AI agents. The core of this is a declarative state, version-controlled in Git, serving as the single source of truth.

However, in real-world scenarios, "configuration drift" is common. Emergency hotfixes, manual debugging, or changes from non-integrated tools can alter a cluster's live state, making it diverge from the state declared in Git. This breaks the GitOps model and undermines the source of truth.

The **GitOps Reverser** is a lightweight, open-source Go utility that addresses this problem. It acts as a sentinel within a Kubernetes cluster, watching for live changes and automatically committing them back to a designated Git repository. This process of "Reversed GitOps" ensures that the Git repository always reflects the cluster's actual state, maintaining it as the true source of truth.

This tool is a foundational component of the ConfigButler philosophy: it bridges the gap between declarative ideals and operational realities, ensuring that every change is captured, versioned, and auditable.

The tool will become opensource from day 1: and it's going to be distributed from https://github.com/ConfigButler/gitops-reverser

---

### 2. Problem Statement

DevOps and SRE teams practicing GitOps face a persistent challenge:

1.  **Manual Changes Cause Drift:** Developers or operators sometimes apply changes directly to the cluster using `kubectl edit` or `kubectl apply`. These changes are not reflected in the Git repository, leading to configuration drift.
2.  **Lack of Auditability for Imperative Changes:** A direct `kubectl` command is not part of a version-controlled review process and is not easily visible to the wider team.
3.  **Incomplete Picture for AI Agents:** For an AI agent to safely manage a cluster, it needs a perfect, real-time understanding of the cluster's state. If the state in Git is unreliable, the agent's decisions will be based on flawed data.

The GitOps Reverser solves this by making the cluster itself the source of change events, ensuring that any modification, regardless of its origin, is systematically pushed back to Git.

---

### 3. Goals & Objectives (MVP)

The primary goal is to create a focused, reliable, and easy-to-use tool that performs the core task of reversing GitOps drift.

* **Reliable Change Detection:** To reliably detect `CREATE`, `UPDATE`, and `DELETE` operations on specified Kubernetes resources in real-time.
* **Automated Git Commits:** To automatically commit a clean, GitOps-compatible YAML manifest of the affected resource to a specified Git repository.
* **Informative & Actionable History:** To create a clean, understandable Git history where each commit represents a single change.
* **High Performance & Availability:** To operate with minimal latency and support a highly available, zero-downtime deployment model.
* **Production-Grade Observability:** To export key performance and behavior metrics via the OpenTelemetry Protocol (OTLP).
* **Granular, Multi-Level Configurability:** To allow different teams to manage their own settings via a two-tiered CRD system.

---

### 4. Core Features & Functionality

#### Feature: Event-Driven Change Capture

* **Description:** The tool will use a Kubernetes `ValidatingAdmissionWebhook` to intercept requests to the Kubernetes API server.

#### Feature: Git Integration & Committing

* **Description:** The tool will be configured with a Git repository URL, branch, and credentials. Upon detecting a change, it will commit the relevant YAML file to a well-defined path in the repository.

#### Feature: Informative Commit Messages

* **Description:** The tool will generate structured commit messages for auditability.
* **Format:** `[OPERATION] {kind}/{name} in ns/{namespace} by user/{username}`

#### Feature: Two-Tier Configuration via CRDs

* **Description:** Configuration is split into two CRDs to provide a clean separation of concerns between platform infrastructure and application-level settings.

1.  **`GitRepoConfig` (Cluster-Scoped):** Defines the "where" and "how" - the Git repository connection details and push strategy.
    ```yaml
    # Example GitRepoConfig CRD
    apiVersion: configbutler.ai/v1alpha1
    kind: GitRepoConfig
    metadata:
      name: primary-cluster-state-repo
    spec:
      repoUrl: "git@github.com:my-org/cluster-state.git"
      branch: "main"
      secretName: "git-ssh-key"
      secretNamespace: "configbutler-system"
      
      # Defines the strategy for pushing commits to the remote.
      push:
        # Push all queued commits at this interval.
        interval: "1m" # Default: "1m"
        # Push when the number of queued commits reaches this threshold,
        # whichever comes first (interval or maxCommits).
        maxCommits: 20 # Default: 20
    ```

2.  **`WatchRule` (Namespaced):** Defines the "what" - the rules for which resources to monitor.
    ```yaml
    # Example WatchRule CRD
    apiVersion: configbutler.ai/v1alpha1
    kind: WatchRule
    metadata:
      name: my-app-rules
      namespace: my-app-ns
    spec:
      gitRepoConfigRef: "primary-cluster-state-repo"
      excludeLabels:
        matchExpressions:
          - {key: "configbutler.ai/ignore", operator: Exists}
      rules:
        - resources: ["deployments", "services", "configmaps", "secrets"]
        - resources: ["ingresses.*"]
    ```

#### Feature: Observability via OTLP Metrics

* **Description:** The tool will expose metrics via an OTLP endpoint.
* **Key Metrics:** `gitopsreverser_events_received_total`, `gitopsreverser_events_processed_total`, `gitopsreverser_git_operations_total`, `gitopsreverser_git_push_duration_seconds`, `gitopsreverser_git_commit_queue_size`.

---

### 5. Technical Implementation

* **Language:** **Go**.
* **Event Sourcing:** **Dynamic Admission Webhook**.

#### Webhook Configuration

The tool's Helm chart will install a `ValidatingWebhookConfiguration` resource. This configuration is intentionally broad, sending all `CREATE`, `UPDATE`, and `DELETE` events to the `gitops-reverser` service. The service itself is responsible for applying the fine-grained filtering logic defined in the `WatchRule` CRs.

```yaml
# Example ValidatingWebhookConfiguration
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: gitops-reverser-webhook
webhooks:
- name: gitops-reverser.configbutler.ai
  clientConfig:
    service:
      name: gitops-reverser-service
      namespace: configbutler-system
      path: "/validate"
  # ...
  failurePolicy: Ignore # Critical for cluster stability
  # ...
```

#### Architecture & Performance

1. The `gitops-reverser` controller starts and watches for `GitRepoConfig` and `WatchRule` resources, building an in-memory model of all active rules.

2. The webhook's logic **synchronously** checks requests against the rule model, approves the request, and passes matched events to a local, in-memory queue.

3. A separate **asynchronous** worker processes the queue, decoupling Git operations from the API request path. Commits are **batched** based on the strategy defined in the `GitRepoConfig`.

#### Manifest Sanitization

The tool will only record the *desired state*. It will **Preserve** `apiVersion`, `kind`, `metadata` (name, namespace, labels, annotations), and the `spec`/`data` block. It will **Remove** the `status` block and all other server-generated fields.

#### Git Repository Structure

* **Namespaced Resources:** `namespaces/{namespace}/{kind}/{name}.yaml`

* **Cluster-Scoped Resources:** `cluster-scoped/{kind}/{name}.yaml`

#### High Availability (HA)

The tool must be designed for zero-downtime operation. This is achieved through a standard leader election pattern that guarantees a single writer and prevents event loss.

* **Leader Election:** All pods will participate in a leader election process using a Kubernetes `Lease` resource.

* **Active-Passive Model with Service Routing:** The system will operate in an active-passive mode. The Kubernetes `Service` will use a label selector (e.g., `role: leader`) to exclusively target the leader pod. When a pod becomes the leader, it adds this label to itself. Non-leader pods are hot standbys and receive no traffic.

* **Security & Permissions:** The tool's ServiceAccount requires RBAC permission to `get` and `patch` its own `Pod` resource for this HA mechanism.

* **Zero-Downtime Upgrades & Leader Transition:** The `Deployment` will use a `RollingUpdate` strategy. The application must handle `SIGTERM` gracefully, attempting to drain its in-memory queue of pending commits before exiting.

### 6. Key Assumptions & Limitations

* **Works with Rendered Manifests Only:** This tool captures the final state of a Kubernetes object. It does not work on templated files.

### 7. Competitive Landscape & Differentiation

| **Tool** | **How it Works** | **Key Difference from GitOps Reverser** | 
|---|---|---|
| **`bpineau/katafygio`** | Periodically scans the cluster and dumps all resources to a Git repository. | **Event-Driven vs. Snapshot:** Katafygio is a backup tool. GitOps Reverser is event-driven, providing a real-time audit trail. | 
| **`RichardoC/kube-audit-rest`** | An admission webhook that receives audit events and exposes them over a REST API. | **Action vs. Transport:** `kube-audit-rest` is a transport layer. GitOps Reverser is an *action* layer that consumes the event and commits it to Git. | 
| **`robusta-dev/robusta`** | A broad observability and automation platform. | **Focused Tool vs. Broad Platform:** Robusta is a large platform. GitOps Reverser is a small, single-purpose utility focused on simplicity and low overhead. | 

**Our Unique Value Proposition:** A standalone, lightweight, HA-ready, and observable tool that is laser-focused on solving the GitOps drift problem in real-time, using a cloud-native webhook architecture and a granular, security-focused configuration model.

### 8. Out of Scope

* **Pull Request Creation:** This tool will **never** create Pull Requests.

* **GUI/Dashboard:** This is a backend, API-driven tool.

* **Automatic Rollback:** The tool's purpose is to record changes, not prevent them.

* **Support for non-YAML formats:** The output will be YAML.

### 9. Success Metrics

* **Adoption:** GitHub stars, forks, and container downloads.

* **Community:** Active issues and pull requests.

* **Stability:** Low rate of bugs and performance issues.

* **Integration:** Being used in blog posts, tutorials, and other projects.

### 10. Development & Quality Standards

To ensure the project is maintainable, secure, and welcoming to contributors, the following standards will be upheld.

#### Testing Strategy

A robust testing suite is non-negotiable. The project must include:

* **Unit Tests:** All significant functions, especially pure logic like manifest sanitization and rule matching, must have comprehensive unit tests. The Kubernetes client will be mocked to test controller logic in isolation. A high level of code coverage is expected.

* **Integration Tests:** The interaction between controllers and the webhook will be tested. This will use the `envtest` package from `controller-runtime` to run tests against a real, temporary `etcd` and `kube-apiserver` binary, providing a high-fidelity testing environment without the overhead of a full cluster.

#### Code Quality & Conventions

* **Linting:** All code must pass checks from a standardized `golangci-lint` configuration. This will be enforced automatically in CI.

* **Formatting:** Code must be formatted with `goimports`.

* **Dependency Management:** The project will use Go Modules. Dependencies should be kept up-to-date to avoid security vulnerabilities.

* **Structured Logging:** The application will use a structured logging library (e.g., `logr`) to produce machine-readable logs, which is essential for debugging in a production environment.

* **Documentation:** Public functions and types must have clear, `godoc`-compatible comments. Complex logic blocks should be explained with inline comments.

#### Community & Contribution Workflow

* **Contribution Guide:** A `CONTRIBUTING.md` file will be created, explaining how to set up the development environment, run the test suite, and submit a pull request.

* **CI/CD Pipeline:** A GitHub Actions workflow will be established to run on every pull request. This pipeline will automatically run the linter, execute all unit and integration tests, and build the container image to ensure that contributions meet the project's quality standards before being merged.
