# Support today: the GitOps layout corpus

<!-- GENERATED FILE — DO NOT EDIT. Regenerate with `task gitops-layouts-baseline`. -->

A behavioural baseline: what `manifest-analyzer --mode scan-repo` reports for every
fixture in this corpus, as of the last regeneration. It is **descriptive**. It
records what the tool does today, not what the operator should support — that is
[`docs/design/support-boundary/support-contract.md`](../../../docs/design/support-boundary/support-contract.md).

This file carries no interpretation on purpose. When it disagrees with the support
contract, that disagreement is the backlog.

Reading rules:

- `rc=0` means the scan command succeeded. It does **not** mean the fixture is supported.
- `accepted` / `refused` count reported **candidate folders**, not whole-repository support.
- `scan-repo` is structure-only. It never executes Argo CD, Flux, Helm, SOPS, plugins,
  or remote fetches — so it cannot see a generator's output, a rendered chart, or an
  input set that lives in a Git-host API.
- **A missing candidate matters as much as a refusal**: it means the tool did not
  explain that part of the repository at all.

## Summary

| Fixture | rc | Outcome | Accepted | Refused | Layouts | Unsupported constructs | Reported refusal signal |
|---|---:|---|---:|---:|---|---|---|
| 1-desired-state/argocd-app-of-apps | 0 | All reported candidates accepted | 4 | 0 | plain=4 | - | None |
| 1-desired-state/argocd-plain | 0 | Partial | 1 | 1 | plain=2 | - | non-krm-yaml: ci-metadata.yaml: YAML is not a Kubernetes manifest |
| 1-desired-state/flux-monorepo | 0 | All reported candidates accepted | 6 | 0 | kustomize-overlay=2, kustomize-single=4 | - | None |
| 1-desired-state/repo-per-environment | 0 | Partial | 6 | 3 | plain=9 | - | foreign-file: .gitignore: foreign file .gitignore is not a managed manifest; remove it or name it in .gittargetignore<br>foreign-file: .gitignore: foreign file .gitignore is not a managed manifest; remove it or name it in .gittargetignore<br>foreign-file: .gitignore: foreign file .gitignore is not a managed manifest; remove it or name it in .gittargetignore |
| 2-rendered/argocd-external-helm | 0 | Partial | 2 | 1 | plain=3 | - | non-krm-yaml: values.yaml: YAML is not a Kubernetes manifest |
| 2-rendered/helm-chart | 0 | All reported candidates accepted | 1 | 0 | plain=1 | - | None |
| 2-rendered/helm-environment-values | 0 | All reported candidates accepted | 1 | 0 | plain=1 | - | None |
| 2-rendered/kustomize-overlay-minimal | 0 | All reported candidates accepted | 2 | 0 | kustomize-overlay=2 | - | None |
| 2-rendered/kustomize-overlays | 0 | Partial | 1 | 3 | kustomize-single=1, refused-structural=3 | configMapGenerator, namePrefix, nameSuffix, remote-base, secretGenerator | refused-structural: kustomization uses unsupported feature(s): remote-base<br>refused-structural: kustomization uses unsupported feature(s): configMapGenerator, nameSuffix, secretGenerator<br>refused-structural: kustomization uses unsupported feature(s): configMapGenerator, namePrefix |
| 2-rendered/rendered-manifests | 0 | Partial | 3 | 2 | plain=3, refused-structural=2 | namePrefix | refused-structural: kustomization uses unsupported feature(s): namePrefix<br>refused-structural: kustomization uses unsupported feature(s): namePrefix |
| 3-expanded/argocd-applicationset-directories | 0 | All reported candidates accepted | 5 | 0 | plain=5 | - | None |
| 3-expanded/argocd-applicationset-files | 0 | No reported candidates accepted | 0 | 1 | plain=1 | - | non-krm-yaml: chart/Chart.yaml: YAML is not a Kubernetes manifest<br>foreign-file: chart/templates/_helpers.tpl: foreign file chart/templates/_helpers.tpl is not a managed manifest; remove it or name it in .gittargetignore<br>non-krm-yaml: chart/templates/deployment.yaml: YAML is not a Kubernetes manifest<br>non-krm-yaml: chart/templates/service.yaml: YAML is not a Kubernetes manifest<br>+5 more |
| 3-expanded/argocd-multicluster-matrix | 0 | All reported candidates accepted | 4 | 0 | plain=4 | - | None |
| 3-expanded/flux-helmrelease | 0 | Partial | 3 | 1 | kustomize-single=3, refused-structural=1 | configMapGenerator | refused-structural: kustomization uses unsupported feature(s): configMapGenerator |
| 3-expanded/flux-resourceset-inline | 0 | All reported candidates accepted | 1 | 0 | plain=1 | - | None |
| 3-expanded/flux-resourceset-pull-requests | 0 | All reported candidates accepted | 1 | 0 | plain=1 | - | None |
| 4-machine-written/flux-image-automation | 0 | All reported candidates accepted | 3 | 0 | kustomize-single=3 | - | None |
| 5-opaque/sops-encrypted | 0 | All reported candidates accepted | 3 | 0 | kustomize-single=2, plain=1 | - | None |
| 6-hostile/mixed-and-hostile | 0 | Partial | 3 | 2 | plain=4, refused-structural=1 | unparseable | refused-structural: kustomization uses unsupported feature(s): unparseable (invalid Kustomization: json: unknown field "spec")<br>impure-managed-file: bundle.yaml: a file with managed resources may contain only valid KRM documents; document #1 is a non-KRM document<br>impure-managed-file: bundle.yaml: a file with managed resources may contain only valid KRM documents; document #2 is an empty document<br>foreign-file: deployment.json: foreign file deployment.json is not a managed manifest; remove it or name it in .gittargetignore |

## 1-desired-state/argocd-app-of-apps

Reported rc `0`. Accepted `4`, refused `0`.
Unsupported constructs: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `applications` | `plain` | true | `argocd` | 2/2/0 | none |
| `bootstrap` | `plain` | true | `argocd` | 1/1/0 | none |
| `manifests/backend` | `plain` | true | `backend` | 2/2/0 | none |
| `manifests/frontend` | `plain` | true | `frontend` | 2/2/0 | none |

## 1-desired-state/argocd-plain

Reported rc `0`. Accepted `1`, refused `1`.
Unsupported constructs: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `apps/frontend` | `plain` | false | `frontend` | 6/6/1 | non-krm-yaml: ci-metadata.yaml: YAML is not a Kubernetes manifest |
| `argocd` | `plain` | true | `argocd` | 1/1/0 | none |

## 1-desired-state/flux-monorepo

Reported rc `0`. Accepted `6`, refused `0`.
Unsupported constructs: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `apps/production` | `kustomize-overlay` | true | `production` | 2/0/0 | none |
| `apps/staging` | `kustomize-overlay` | true | `staging` | 2/0/0 | none |
| `clusters/production` | `kustomize-single` | true | `flux-system` | 7/7/0 | none |
| `clusters/staging` | `kustomize-single` | true | `flux-system` | 7/7/0 | none |
| `infrastructure/configs` | `kustomize-single` | true | `-` | 1/1/0 | none |
| `infrastructure/controllers` | `kustomize-single` | true | `-` | 2/2/0 | none |

## 1-desired-state/repo-per-environment

Reported rc `0`. Accepted `6`, refused `3`.
Unsupported constructs: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `gitops-dev` | `plain` | false | `-` | 6/6/1 | foreign-file: .gitignore: foreign file .gitignore is not a managed manifest; remove it or name it in .gittargetignore |
| `gitops-dev/apps/backend` | `plain` | true | `backend-dev` | 2/2/0 | none |
| `gitops-dev/apps/frontend` | `plain` | true | `frontend-dev` | 2/2/0 | none |
| `gitops-production` | `plain` | false | `-` | 7/7/1 | foreign-file: .gitignore: foreign file .gitignore is not a managed manifest; remove it or name it in .gittargetignore |
| `gitops-production/apps/backend` | `plain` | true | `backend-production` | 2/2/0 | none |
| `gitops-production/apps/frontend` | `plain` | true | `frontend-production` | 3/3/0 | none |
| `gitops-staging` | `plain` | false | `-` | 6/6/1 | foreign-file: .gitignore: foreign file .gitignore is not a managed manifest; remove it or name it in .gittargetignore |
| `gitops-staging/apps/backend` | `plain` | true | `backend-staging` | 2/2/0 | none |
| `gitops-staging/apps/frontend` | `plain` | true | `frontend-staging` | 2/2/0 | none |

## 2-rendered/argocd-external-helm

Reported rc `0`. Accepted `2`, refused `1`.
Unsupported constructs: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `applications` | `plain` | true | `argocd` | 3/3/0 | none |
| `extras/ingress-nginx` | `plain` | true | `ingress-nginx` | 2/2/0 | none |
| `platform/cert-manager` | `plain` | false | `argocd` | 2/2/1 | non-krm-yaml: values.yaml: YAML is not a Kubernetes manifest |

## 2-rendered/helm-chart

Reported rc `0`. Accepted `1`, refused `0`.
Unsupported constructs: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `charts/frontend/crds` | `plain` | true | `-` | 1/1/0 | none |

## 2-rendered/helm-environment-values

Reported rc `0`. Accepted `1`, refused `0`.
Unsupported constructs: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `argocd` | `plain` | true | `argocd` | 2/2/0 | none |

## 2-rendered/kustomize-overlay-minimal

Reported rc `0`. Accepted `2`, refused `0`.
Unsupported constructs: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `overlays/production` | `kustomize-overlay` | true | `frontend-production` | 2/0/0 | none |
| `overlays/staging` | `kustomize-overlay` | true | `frontend-staging` | 2/0/0 | none |

## 2-rendered/kustomize-overlays

Reported rc `0`. Accepted `1`, refused `3`.
Unsupported constructs: `configMapGenerator, namePrefix, nameSuffix, remote-base, secretGenerator`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `apps/backend/base` | `kustomize-single` | true | `-` | 2/2/0 | none |
| `apps/backend/overlays/production` | `refused-structural` | false | `backend-production` | 0/0/0 | refused-structural: kustomization uses unsupported feature(s): remote-base |
| `apps/frontend/overlays/production` | `refused-structural` | false | `frontend-production` | 2/0/3 | refused-structural: kustomization uses unsupported feature(s): configMapGenerator, nameSuffix, secretGenerator |
| `apps/frontend/overlays/staging` | `refused-structural` | false | `frontend-staging` | 2/0/1 | refused-structural: kustomization uses unsupported feature(s): configMapGenerator, namePrefix |

## 2-rendered/rendered-manifests

Reported rc `0`. Accepted `3`, refused `2`.
Unsupported constructs: `namePrefix`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `argocd` | `plain` | true | `argocd` | 1/1/0 | none |
| `rendered/production` | `plain` | true | `frontend-production` | 3/3/0 | none |
| `rendered/staging` | `plain` | true | `frontend-staging` | 3/3/0 | none |
| `src/frontend/overlays/production` | `refused-structural` | false | `frontend-production` | 2/0/0 | refused-structural: kustomization uses unsupported feature(s): namePrefix |
| `src/frontend/overlays/staging` | `refused-structural` | false | `frontend-staging` | 2/0/0 | refused-structural: kustomization uses unsupported feature(s): namePrefix |

## 3-expanded/argocd-applicationset-directories

Reported rc `0`. Accepted `5`, refused `0`.
Unsupported constructs: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `apps/backend` | `plain` | true | `backend` | 2/2/0 | none |
| `apps/frontend` | `plain` | true | `frontend` | 2/2/0 | none |
| `apps/platform/monitoring` | `plain` | true | `platform` | 2/2/0 | none |
| `apps/worker` | `plain` | true | `worker` | 1/1/0 | none |
| `bootstrap` | `plain` | true | `argocd` | 1/1/0 | none |

## 3-expanded/argocd-applicationset-files

Reported rc `0`. Accepted `0`, refused `1`.
Unsupported constructs: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `.` | `plain` | false | `argocd` | 1/1/9 | non-krm-yaml: chart/Chart.yaml: YAML is not a Kubernetes manifest<br>foreign-file: chart/templates/_helpers.tpl: foreign file chart/templates/_helpers.tpl is not a managed manifest; remove it or name it in .gittargetignore<br>non-krm-yaml: chart/templates/deployment.yaml: YAML is not a Kubernetes manifest<br>non-krm-yaml: chart/templates/service.yaml: YAML is not a Kubernetes manifest<br>non-krm-yaml: chart/values.yaml: YAML is not a Kubernetes manifest<br>non-krm-yaml: deployments/dev/backend.yaml: YAML is not a Kubernetes manifest<br>non-krm-yaml: deployments/dev/frontend.yaml: YAML is not a Kubernetes manifest<br>non-krm-yaml: deployments/production/backend.yaml: YAML is not a Kubernetes manifest<br>non-krm-yaml: deployments/production/frontend.yaml: YAML is not a Kubernetes manifest |

## 3-expanded/argocd-multicluster-matrix

Reported rc `0`. Accepted `4`, refused `0`.
Unsupported constructs: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `applicationsets` | `plain` | true | `argocd` | 1/1/0 | none |
| `apps/backend` | `plain` | true | `-` | 2/2/0 | none |
| `apps/frontend` | `plain` | true | `-` | 2/2/0 | none |
| `clusters` | `plain` | true | `argocd` | 3/3/0 | none |

## 3-expanded/flux-helmrelease

Reported rc `0`. Accepted `3`, refused `1`.
Unsupported constructs: `configMapGenerator`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `apps/frontend` | `refused-structural` | false | `-` | 1/1/1 | refused-structural: kustomization uses unsupported feature(s): configMapGenerator |
| `clusters/production` | `kustomize-single` | true | `flux-system` | 7/7/0 | none |
| `infrastructure/controllers/ingress-nginx` | `kustomize-single` | true | `flux-system` | 3/3/0 | none |
| `infrastructure/sources` | `kustomize-single` | true | `flux-system` | 3/3/0 | none |

## 3-expanded/flux-resourceset-inline

Reported rc `0`. Accepted `1`, refused `0`.
Unsupported constructs: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `tenants` | `plain` | true | `flux-system` | 1/1/0 | none |

## 3-expanded/flux-resourceset-pull-requests

Reported rc `0`. Accepted `1`, refused `0`.
Unsupported constructs: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `previews` | `plain` | true | `flux-system` | 2/2/0 | none |

## 4-machine-written/flux-image-automation

Reported rc `0`. Accepted `3`, refused `0`.
Unsupported constructs: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `apps/frontend` | `kustomize-single` | true | `frontend` | 2/2/0 | none |
| `clusters/production` | `kustomize-single` | true | `flux-system` | 2/2/0 | none |
| `infrastructure/image-automation` | `kustomize-single` | true | `flux-system` | 3/3/0 | none |

## 5-opaque/sops-encrypted

Reported rc `0`. Accepted `3`, refused `0`.
Unsupported constructs: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `apps/frontend` | `kustomize-single` | true | `frontend-production` | 2/2/0 | none |
| `clusters/production` | `plain` | true | `flux-system` | 2/2/0 | none |
| `infrastructure/secrets` | `kustomize-single` | true | `infrastructure` | 1/1/0 | none |

## 6-hostile/mixed-and-hostile

Reported rc `0`. Accepted `3`, refused `2`.
Unsupported constructs: `unparseable`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `.` | `refused-structural` | false | `backend` | 0/0/6 | refused-structural: kustomization uses unsupported feature(s): unparseable (invalid Kustomization: json: unknown field "spec") |
| `crossplane` | `plain` | true | `-` | 1/1/0 | none |
| `kro` | `plain` | true | `-` | 1/1/0 | none |
| `mixed` | `plain` | false | `-` | 3/3/2 | impure-managed-file: bundle.yaml: a file with managed resources may contain only valid KRM documents; document #1 is a non-KRM document<br>impure-managed-file: bundle.yaml: a file with managed resources may contain only valid KRM documents; document #2 is an empty document<br>foreign-file: deployment.json: foreign file deployment.json is not a managed manifest; remove it or name it in .gittargetignore |
| `secrets` | `plain` | true | `backend` | 1/1/0 | none |
