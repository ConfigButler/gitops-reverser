# Makefile Target & Dependency Reference

Rendered with Mermaid (supported in GitHub, VSCode with Markdown Preview Mermaid extension, and most modern docs platforms).

---

## Design Principles

| Principle | How it is implemented |
|---|---|
| **Never delete on abort** | Tests are run against live stamps; nothing is cleaned automatically. State survives for investigation. |
| **Reuse everything possible** | All expensive work (cluster, installed services, image, install) is tracked by stamp files under `.stamps/`. A target only re-runs if its inputs changed. |
| **Mimic real user usage** | Three install paths (`helm`, `config-dir`, `plain-manifests-file`) each apply the controller exactly as a user would, not via shortcuts. |

---

## Quick Reference

### User-facing targets

| Target | Category | What it does |
|---|---|---|
| `build` | Dev | Compile `bin/manager` |
| `run` | Dev | Run controller locally (no container) |
| `test` | Dev | Unit tests with envtest |
| `lint` / `lint-fix` | Dev | golangci-lint |
| `generate` | Codegen | Generate `zz_generated.deepcopy.go` |
| `manifests` | Codegen | Generate CRDs, RBAC, webhook manifests |
| `helm-sync` | Packaging | Sync manifests into Helm chart |
| `docker-build` | Image | Build `IMG` for local use |
| `docker-buildx` | Image | Cross-platform push |
| `install` | E2E | Deploy controller (uses `INSTALL_MODE`) |
| `prepare-e2e` | E2E | Full environment prep (called by Go BeforeSuite) |
| `test-e2e` | E2E | Run full e2e suite |
| `test-e2e-quickstart-helm` | E2E | Quickstart smoke test (Helm) |
| `test-e2e-quickstart-manifest` | E2E | Quickstart smoke test (manifest) |
| `e2e-gitea-bootstrap` | E2E | Bootstrap Gitea org (cluster-scoped, reused) |
| `e2e-gitea-run-setup` | E2E | Create repo + credentials (run-scoped) |
| `portforward-ensure` | E2E | Start/verify port-forwards |
| `setup-envtest` | Tools | Download envtest binaries |
| `clean` | Cleanup | Remove `bin/`, `dist/`, `.stamps/` |
| `clean-installs` | Cleanup | Delete controller namespace + CRDs |
| `clean-port-forwards` | Cleanup | Kill port-forward processes |
| `clean-cluster` | Cleanup | Tear down k3d cluster |

### Key variables

| Variable | Default | Purpose |
|---|---|---|
| `CTX` | `k3d-gitops-reverser-test-e2e` | kubeconfig context; parameterises all stamp paths |
| `NAMESPACE` | `gitops-reverser` | Target namespace for controller install |
| `INSTALL_MODE` | `config-dir` | One of `helm` \| `config-dir` \| `plain-manifests-file` |
| `PROJECT_IMAGE` | `gitops-reverser:e2e-local` | Image to deploy; set externally in CI to skip local build |
| `GITEA_PORT` | `13000` | Local port for Gitea port-forward |
| `PROMETHEUS_PORT` | `19090` | Local port for Prometheus port-forward |

---

## Diagrams

> **Conventions used in all diagrams**
>
> | Shape | Meaning |
> |---|---|
> | Yellow parallelogram | Source file / input |
> | Blue rounded rectangle | Phony Make target (user-facing) |
> | Green rectangle | Generated file or stamp |
> | Red cylinder | `.stamps/` marker file |
> | `&:` label | GNU Make grouped target — one recipe writes all listed outputs |

---

### 1. Code Generation Pipeline

How Go source files flow through `controller-gen` into all generated artifacts.

```mermaid
flowchart LR
    classDef src fill:#fff2cc,stroke:#d6b656,color:#000
    classDef phony fill:#dae8fc,stroke:#6c8ebf,color:#000
    classDef gen fill:#d5e8d4,stroke:#82b366,color:#000

    api(["api/v1alpha1/*.go"]):::src
    impl(["cmd/ internal/*.go"]):::src
    boiler(["hack/boilerplate.go.txt"]):::src
    chart_tmpl(["charts/gitops-reverser/\ntemplates/ Chart.yaml values.yaml"]):::src

    deepcopy["api/v1alpha1/\nzz_generated.deepcopy.go"]:::gen
    crds["config/crd/bases/*.yaml"]:::gen
    rbac["config/rbac/role.yaml"]:::gen
    webhook["config/webhook/manifests.yaml"]:::gen
    helm_crds["charts/gitops-reverser/crds/*.yaml\ncharts/.../config/role.yaml"]:::gen
    dist["dist/install.yaml"]:::gen

    generate(generate):::phony
    manifests(manifests):::phony
    helmsync(helm-sync):::phony

    api --> deepcopy
    boiler --> deepcopy
    deepcopy -.->|alias| generate

    api --> crds
    impl --> crds
    api --> rbac
    impl --> rbac
    api --> webhook
    impl --> webhook
    crds -.->|alias| manifests
    rbac -.->|alias| manifests
    webhook -.->|alias| manifests

    note_m["&: grouped target\n(one controller-gen run)"]
    crds & rbac & webhook --- note_m

    crds --> helm_crds
    rbac --> helm_crds
    helm_crds -.->|alias| helmsync

    note_h["&: grouped target\n(one cp run)"]
    helm_crds --- note_h

    helm_crds --> dist
    chart_tmpl --> dist
```

---

### 2. Developer Daily Workflow

Targets used for local development. All depend on generated artifacts.

```mermaid
flowchart LR
    classDef src fill:#fff2cc,stroke:#d6b656,color:#000
    classDef phony fill:#dae8fc,stroke:#6c8ebf,color:#000
    classDef gen fill:#d5e8d4,stroke:#82b366,color:#000
    classDef stamp fill:#f8cecc,stroke:#b85450,color:#000

    generate(generate):::phony
    manifests(manifests):::phony
    fmt(fmt):::phony
    vet(vet):::phony
    setup_envtest(setup-envtest):::phony
    build(build):::phony
    run(run):::phony
    test(test):::phony
    lint(lint):::phony

    envtest_stamp[".stamps/envtest-X.Y.ready"]:::stamp
    binary["bin/manager"]:::gen

    generate --> build & run & test
    manifests --> build & run & test
    fmt --> build & run & test
    vet --> build & run & test

    setup_envtest --> test
    envtest_stamp --> setup_envtest

    build --> binary

    lint -.->|independent| build
```

---

### 3. E2E Cluster & Services Bootstrap

How the k3d cluster and its shared services come to life. These stamps are **cluster-scoped** and reused across test runs.

```mermaid
flowchart TD
    classDef src fill:#fff2cc,stroke:#d6b656,color:#000
    classDef phony fill:#dae8fc,stroke:#6c8ebf,color:#000
    classDef stamp fill:#f8cecc,stroke:#b85450,color:#000

    start_sh(["test/e2e/cluster/start-cluster.sh"]):::src
    gitea_vals(["test/e2e/gitea-values.yaml"]):::src
    prom_sh(["hack/e2e/ensure-prometheus-operator.sh"]):::src
    prom_manifests(["test/e2e/setup/prometheus/*.yaml"]):::src

    cluster_ready[".stamps/cluster/CTX/ready"]:::stamp
    cert_installed[".stamps/cluster/CTX/\ncert-manager.installed"]:::stamp
    gitea_installed[".stamps/cluster/CTX/\ngitea.installed"]:::stamp
    prom_installed[".stamps/cluster/CTX/\nprometheus.installed"]:::stamp
    services_ready[".stamps/cluster/CTX/\nservices.ready"]:::stamp

    portforward(portforward-ensure):::phony

    start_sh --> cluster_ready

    cluster_ready --> cert_installed
    cluster_ready --> gitea_installed
    gitea_vals --> gitea_installed
    cluster_ready --> prom_installed
    prom_sh --> prom_installed
    prom_manifests --> prom_installed

    cert_installed --> services_ready
    gitea_installed --> services_ready
    prom_installed --> services_ready

    services_ready --> portforward
```

---

### 4. Controller Image & Installation

How the controller image is built (or pulled), loaded into k3d, and deployed — one path per install mode.

```mermaid
flowchart TD
    classDef src fill:#fff2cc,stroke:#d6b656,color:#000
    classDef phony fill:#dae8fc,stroke:#6c8ebf,color:#000
    classDef stamp fill:#f8cecc,stroke:#b85450,color:#000
    classDef gen fill:#d5e8d4,stroke:#82b366,color:#000

    go_src(["cmd/ internal/ api/*.go\ngo.mod go.sum"]):::src
    dockerfile(["Dockerfile"]):::src
    manifests_out["config/crd/bases/*.yaml\nconfig/rbac/role.yaml ..."]:::gen
    helm_out["charts/gitops-reverser/crds/ ..."]:::gen
    dist_yaml["dist/install.yaml"]:::gen

    ctrl_id[".stamps/image/controller.id\n(local build)"]:::stamp
    project_img[".stamps/image/project-image.ready"]:::stamp
    img_loaded[".stamps/cluster/CTX/image.loaded"]:::stamp
    cluster_ready[".stamps/cluster/CTX/ready"]:::stamp
    services_ready[".stamps/cluster/CTX/services.ready"]:::stamp

    install_helm[".stamps/cluster/CTX/NS/\nhelm/install.yaml"]:::stamp
    install_config[".stamps/cluster/CTX/NS/\nconfig-dir/install.yaml"]:::stamp
    install_plain[".stamps/cluster/CTX/NS/\nplain-manifests-file/install.yaml"]:::stamp

    ctrl_deployed[".stamps/cluster/CTX/NS/\ncontroller.deployed"]:::stamp

    go_src --> ctrl_id
    dockerfile --> ctrl_id
    ctrl_id -->|local build path| project_img
    project_img --> img_loaded
    cluster_ready --> img_loaded

    services_ready --> install_helm
    helm_out --> install_helm

    services_ready --> install_config
    manifests_out --> install_config

    services_ready --> install_plain
    dist_yaml --> install_plain

    install_helm --> ctrl_deployed
    install_config --> ctrl_deployed
    install_plain --> ctrl_deployed
    img_loaded --> ctrl_deployed
```

> **Note on `PROJECT_IMAGE`**: if `PROJECT_IMAGE` is set externally (CI), the `controller.id` build step is skipped entirely and the image is pulled instead. The `project-image.ready` stamp tracks which path was taken.

---

### 5. E2E Test Execution

The full chain from `make test-e2e` to a passing suite. The Go test binary calls `make prepare-e2e` from `BeforeSuite`, so the stamp chain feeds directly into the test process.

```mermaid
flowchart TD
    classDef src fill:#fff2cc,stroke:#d6b656,color:#000
    classDef phony fill:#dae8fc,stroke:#6c8ebf,color:#000
    classDef stamp fill:#f8cecc,stroke:#b85450,color:#000

    age_tool(["test/e2e/tools/gen-age-key/"]):::src
    bootstrap_sh(["hack/e2e/gitea-bootstrap.sh"]):::src
    run_setup_sh(["hack/e2e/gitea-run-setup.sh"]):::src
    go_tests(["test/e2e/*.go"]):::src

    age_key[".stamps/cluster/CTX/age-key.txt"]:::stamp
    sops_yaml[".stamps/cluster/CTX/NS/\nsops-secret.yaml"]:::stamp
    sops_applied[".stamps/cluster/CTX/NS/\nsops-secret.applied"]:::stamp

    install_yaml[".stamps/cluster/CTX/NS/\nINSTALL_MODE/install.yaml"]:::stamp
    img_loaded[".stamps/cluster/CTX/image.loaded"]:::stamp
    ctrl_deployed[".stamps/cluster/CTX/NS/\ncontroller.deployed"]:::stamp

    prepare_ready[".stamps/cluster/CTX/NS/\nprepare-e2e.ready"]:::stamp
    portforward(portforward-ensure):::phony

    bootstrap_ready[".stamps/cluster/CTX/gitea/\nbootstrap/ready\n(api.ready + org.ready &:)"]:::stamp
    checkout_ready[".stamps/cluster/CTX/NS/\nrepo/checkout.ready"]:::stamp

    e2e_passed[".stamps/cluster/CTX/e2e.passed"]:::stamp
    test_e2e(test-e2e):::phony
    prepare_e2e(prepare-e2e):::phony

    age_tool --> age_key
    age_key --> sops_yaml
    sops_yaml --> sops_applied
    install_yaml --> sops_applied

    install_yaml --> ctrl_deployed
    img_loaded --> ctrl_deployed

    install_yaml --> prepare_ready
    img_loaded --> prepare_ready
    ctrl_deployed --> prepare_ready
    sops_applied --> prepare_ready

    prepare_ready -.->|alias| prepare_e2e
    portforward -.-> prepare_e2e

    prepare_ready --> bootstrap_ready
    bootstrap_sh --> bootstrap_ready

    bootstrap_ready --> checkout_ready
    run_setup_sh --> checkout_ready

    age_key --> e2e_passed
    go_tests --> e2e_passed
    bootstrap_sh --> e2e_passed
    run_setup_sh --> e2e_passed

    e2e_passed --> test_e2e
```

> **`e2e-gitea-bootstrap`** and **`e2e-gitea-run-setup`** are convenience phony aliases for `bootstrap/ready` and `repo/checkout.ready` respectively, callable independently for debugging.

---

### 6. Stamp File Hierarchy

All `.stamps/` paths, showing scope and what invalidates each level.

```mermaid
flowchart TD
    classDef cluster fill:#e1d5e7,stroke:#9673a6,color:#000
    classDef image fill:#dae8fc,stroke:#6c8ebf,color:#000
    classDef ns fill:#d5e8d4,stroke:#82b366,color:#000
    classDef gitea fill:#fff2cc,stroke:#d6b656,color:#000

    subgraph image_scope["Scope: local machine (image/)"]
        ctrl_id[".stamps/image/controller.id\nInvalidated by: GO_SOURCES, Dockerfile"]:::image
        proj_img[".stamps/image/project-image.ready\nInvalidated by: controller.id change\nor PROJECT_IMAGE env var"]:::image
    end

    subgraph cluster_scope["Scope: per CTX (.stamps/cluster/CTX/)"]
        ready["/ready\nInvalidated by: start-cluster.sh"]:::cluster
        cert["/cert-manager.installed\nInvalidated by: CERT_MANAGER_VERSION"]:::cluster
        gitea_i["/gitea.installed\nInvalidated by: GITEA_CHART_VERSION,\ngitea-values.yaml"]:::cluster
        prom["/prometheus.installed\nInvalidated by: ensure-prometheus-operator.sh,\nprometheus manifests"]:::cluster
        svc["/services.ready"]:::cluster
        img_loaded["/image.loaded\nInvalidated by: project-image.ready"]:::cluster
        age["/age-key.txt\nInvalidated by: Makefile, gen-age-key"]:::cluster
        envtest[".stamps/envtest-X.Y.ready\nInvalidated by: Makefile change"]:::cluster

        subgraph ns_scope["Scope: per namespace (.stamps/cluster/CTX/NS/)"]
            sops_yaml["/sops-secret.yaml"]:::ns
            sops_applied["/sops-secret.applied"]:::ns
            install_yaml["/INSTALL_MODE/install.yaml\nInvalidated by: services, manifests,\nHELM_SYNC_OUTPUTS, or dist/install.yaml"]:::ns
            ctrl_deployed["/controller.deployed\nInvalidated by: install.yaml, image.loaded"]:::ns
            prepare["/prepare-e2e.ready"]:::ns
            e2e["/e2e.passed\nInvalidated by: test/e2e/** + hack/e2e/*.sh"]:::ns

            subgraph gitea_scope["Scope: per cluster, shared (.stamps/cluster/CTX/gitea/)"]
                bs_api["/bootstrap/api.ready"]:::gitea
                bs_org["/bootstrap/org-testorg.ready"]:::gitea
                bs_ready["/bootstrap/ready"]:::gitea
            end

            checkout["/repo/checkout.ready\nInvalidated by: gitea-run-setup.sh"]:::ns
        end
    end

    ctrl_id --> proj_img
    proj_img --> img_loaded
    ready --> img_loaded
    ready --> cert & gitea_i & prom
    cert & gitea_i & prom --> svc

    age --> sops_yaml
    sops_yaml --> sops_applied
    install_yaml --> sops_applied

    svc --> install_yaml
    install_yaml --> ctrl_deployed
    img_loaded --> ctrl_deployed

    install_yaml --> prepare
    ctrl_deployed --> prepare
    sops_applied --> prepare
    img_loaded --> prepare

    prepare --> bs_api & bs_org
    bs_api & bs_org --> bs_ready
    bs_ready --> checkout
    age --> e2e
    checkout -.->|test suite runs after checkout| e2e
```

---

## Reuse vs. Rebuild Decision Matrix

| What changed | Stamps invalidated | What Make rebuilds |
|---|---|---|
| `api/*.go` | `controller.id`, `zz_generated.deepcopy.go`, `config/crd/bases/*.yaml` | Image, generated code, manifests, helm-sync, install.yaml, controller.deployed |
| `cmd/` or `internal/*.go` | `controller.id`, CRD manifests | Image rebuilt, controller redeployed |
| `Dockerfile` | `controller.id` | Image rebuilt, controller redeployed |
| `charts/` templates | `dist/install.yaml` | Only `plain-manifests-file` install re-runs |
| `test/e2e/*.go` | `e2e.passed` | Go test binary re-runs, nothing else |
| `hack/e2e/*.sh` | `e2e.passed` (via E2E_TEST_INPUTS) | Go test binary re-runs |
| `test/e2e/gitea-values.yaml` | `gitea.installed` | Gitea upgraded, services chain re-runs |
| `GITEA_CHART_VERSION` | `gitea.installed` (stamp content mismatch) | Gitea upgraded |
| New `CTX=` | All `CS` stamps | Full cluster bootstrap (nothing else) |
| `INSTALL_MODE=` switch | `NS/INSTALL_MODE/install.yaml` | Only new install mode runs; old one cached |
