# Support today: GitOps layout fixtures

Generated from the current `manifest-analyzer` repo scan. This file is
descriptive: it records what the tool reports today, not what the product should
eventually support.

Command used per fixture:

```bash
go build -o /tmp/manifest-analyzer ./cmd/manifest-analyzer
/tmp/manifest-analyzer --mode scan-repo --format json test/fixtures/gitops-layouts/<fixture>
```

Important reading rules:

- `rc=0` means the scan command succeeded. It does not mean the fixture is fully supported.
- `accepted` and `refused` count reported candidate folders, not whole repository support.
- `scan-repo` is structure-only. It does not execute Argo CD, Flux, Helm, SOPS,
  plugins, or remote fetches.
- A missing candidate can be as important as a refusal: it means the tool did not
  explain that part of the repo at all.

## Summary

| Fixture | rc | Outcome | Accepted | Refused | Layouts | Fleet root | Unsupported constructs | Reported refusal signal |
|---|---:|---|---:|---:|---|---|---|---|
| 01-argocd-plain | 0 | Partial | 1 | 1 | plain=2 | false | - | non-krm-yaml: ci-metadata.yaml: YAML is not a Kubernetes manifest |
| 02-argocd-app-of-apps | 0 | All reported candidates accepted | 4 | 0 | plain=4 | false | - | None |
| 03-argocd-applicationset-directories | 0 | All reported candidates accepted | 5 | 0 | plain=5 | false | - | None |
| 04-argocd-applicationset-files | 0 | No reported candidates accepted | 0 | 1 | plain=1 | false | - | non-krm-yaml: chart/Chart.yaml: YAML is not a Kubernetes manifest<br>foreign-file: chart/templates/_helpers.tpl: foreign file chart/templates/_helpers.tpl is not a managed manifest; remove it or name it in .gittargetignore<br>non-krm-yaml: chart/templates/deployment.yaml: YAML is not a Kubernetes manifest<br>non-krm-yaml: chart/templates/service.yaml: YAML is not a Kubernetes manifest<br>+5 more |
| 05-kustomize-overlays | 0 | Partial | 1 | 3 | kustomize-single=1, refused-structural=3 | false | configMapGenerator, namePrefix, nameSuffix, patches, remote-base, secretGenerator | refused-structural: kustomization uses unsupported feature(s): remote-base<br>refused-structural: kustomization uses unsupported feature(s): configMapGenerator, nameSuffix, patches, secretGenerator<br>refused-structural: kustomization uses unsupported feature(s): configMapGenerator, namePrefix, patches |
| 06-helm-chart | 0 | All reported candidates accepted | 1 | 0 | plain=1 | false | - | None |
| 07-helm-environment-values | 0 | All reported candidates accepted | 1 | 0 | plain=1 | false | - | None |
| 08-argocd-external-helm | 0 | Partial | 2 | 1 | plain=3 | false | - | non-krm-yaml: values.yaml: YAML is not a Kubernetes manifest |
| 09-flux-monorepo | 0 | Partial | 4 | 2 | kustomize-single=4, refused-structural=2 | false | patches | refused-structural: kustomization uses unsupported feature(s): patches |
| 10-flux-helmrelease | 0 | Partial | 3 | 1 | kustomize-single=3, refused-structural=1 | false | configMapGenerator | refused-structural: kustomization uses unsupported feature(s): configMapGenerator |
| 11-repo-per-environment | 0 | Partial | 6 | 3 | plain=9 | false | - | foreign-file: .gitignore: foreign file .gitignore is not a managed manifest; remove it or name it in .gittargetignore |
| 12-multicluster-applicationset | 0 | All reported candidates accepted | 4 | 0 | plain=4 | false | - | None |
| 13-sops-encrypted | 0 | All reported candidates accepted | 3 | 0 | kustomize-single=2, plain=1 | false | - | None |
| 14-rendered-manifests | 0 | Partial | 3 | 2 | plain=3, refused-structural=2 | false | namePrefix | refused-structural: kustomization uses unsupported feature(s): namePrefix |
| 15-mixed-and-hostile | 0 | Partial | 3 | 2 | kustomize-single=1, plain=4 | false | - | non-krm-yaml: ci/.gitlab-ci.yml: YAML is not a Kubernetes manifest<br>non-krm-yaml: ci/docker-compose.yml: YAML is not a Kubernetes manifest<br>foreign-file: empty-dir/.gitkeep: foreign file empty-dir/.gitkeep is not a managed manifest; remove it or name it in .gittargetignore<br>mixed-managed-allowlisted: kustomization.yaml: managed resource kustomize.toolkit.fluxcd.io/v1/Kustomization/flux-system/apps must not live in the allowlisted build-directive file kustomization.yaml<br>+8 more |
| 16-flux-image-automation | 0 | All reported candidates accepted | 3 | 0 | kustomize-single=3 | false | - | None |

## Review notes

- The scanner gives useful, specific refusal messages for non-KRM YAML, foreign
  files, invalid YAML, mixed managed/non-KRM documents, and unsupported kustomize
  features. That is the good news.
- Helm support is under-reported. Fixtures `06-helm-chart` and
  `07-helm-environment-values` do not produce a clear "Helm chart/templates are
  unsupported" result; the scanner mostly reports only adjacent KRM such as
  `crds/` or Argo CD Application manifests.
- Argo CD and Flux orchestration semantics are not modeled. The scanner treats
  their CRs as ordinary KRM and does not follow Application paths, ApplicationSet
  generators, Flux image update policies, or Helm value references.
- The Flux fleet-root signal did not fire for the realistic `clusters/` +
  `apps/` + `infrastructure/` shape in fixture `09-flux-monorepo`; the current
  detector appears narrower than the fixture vocabulary.
- Several accepted candidates are control-plane/configuration folders (`argocd`,
  `applications`, `bootstrap`, `clusters`, `infrastructure`) rather than workload
  desired-state folders. That may be structurally true KRM, but it is probably
  not the onboarding answer a product should lead with.

## 01-argocd-plain

Reported result: **Partial**. Accepted `1`, refused `1`.
Unsupported constructs summary: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | Resources rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `apps/frontend` | `plain` | false | `frontend` | 6/6/1 | non-krm-yaml: ci-metadata.yaml: YAML is not a Kubernetes manifest |
| `argocd` | `plain` | true | `argocd` | 1/1/0 | none |

## 02-argocd-app-of-apps

Reported result: **All reported candidates accepted**. Accepted `4`, refused `0`.
Unsupported constructs summary: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | Resources rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `applications` | `plain` | true | `argocd` | 2/2/0 | none |
| `bootstrap` | `plain` | true | `argocd` | 1/1/0 | none |
| `manifests/backend` | `plain` | true | `backend` | 2/2/0 | none |
| `manifests/frontend` | `plain` | true | `frontend` | 2/2/0 | none |

## 03-argocd-applicationset-directories

Reported result: **All reported candidates accepted**. Accepted `5`, refused `0`.
Unsupported constructs summary: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | Resources rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `apps/backend` | `plain` | true | `backend` | 2/2/0 | none |
| `apps/frontend` | `plain` | true | `frontend` | 2/2/0 | none |
| `apps/platform/monitoring` | `plain` | true | `platform` | 2/2/0 | none |
| `apps/worker` | `plain` | true | `worker` | 1/1/0 | none |
| `bootstrap` | `plain` | true | `argocd` | 1/1/0 | none |

## 04-argocd-applicationset-files

Reported result: **No reported candidates accepted**. Accepted `0`, refused `1`.
Unsupported constructs summary: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | Resources rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `.` | `plain` | false | `argocd` | 1/1/9 | non-krm-yaml: chart/Chart.yaml: YAML is not a Kubernetes manifest; foreign-file: chart/templates/_helpers.tpl: foreign file chart/templates/_helpers.tpl is not a managed manifest; remove it or name it in .gittargetignore; non-krm-yaml: chart/templates/deployment.yaml: YAML is not a Kubernetes manifest; non-krm-yaml: chart/templates/service.yaml: YAML is not a Kubernetes manifest; non-krm-yaml: chart/values.yaml: YAML is not a Kubernetes manifest; non-krm-yaml: deployments/dev/backend.yaml: YAML is not a Kubernetes manifest; non-krm-yaml: deployments/dev/frontend.yaml: YAML is not a Kubernetes manifest; non-krm-yaml: deployments/production/backend.yaml: YAML is not a Kubernetes manifest; non-krm-yaml: deployments/production/frontend.yaml: YAML is not a Kubernetes manifest |

## 05-kustomize-overlays

Reported result: **Partial**. Accepted `1`, refused `3`.
Unsupported constructs summary: `configMapGenerator, namePrefix, nameSuffix, patches, remote-base, secretGenerator`.
Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | Resources rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `apps/backend/base` | `kustomize-single` | true | `` | 2/2/0 | none |
| `apps/backend/overlays/production` | `refused-structural` | false | `backend-production` | 0/0/0 | refused-structural: kustomization uses unsupported feature(s): remote-base |
| `apps/frontend/overlays/production` | `refused-structural` | false | `frontend-production` | 2/0/3 | refused-structural: kustomization uses unsupported feature(s): configMapGenerator, nameSuffix, patches, secretGenerator |
| `apps/frontend/overlays/staging` | `refused-structural` | false | `frontend-staging` | 2/0/1 | refused-structural: kustomization uses unsupported feature(s): configMapGenerator, namePrefix, patches |

## 06-helm-chart

Reported result: **All reported candidates accepted**. Accepted `1`, refused `0`.
Unsupported constructs summary: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | Resources rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `charts/frontend/crds` | `plain` | true | `` | 1/1/0 | none |

## 07-helm-environment-values

Reported result: **All reported candidates accepted**. Accepted `1`, refused `0`.
Unsupported constructs summary: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | Resources rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `argocd` | `plain` | true | `argocd` | 2/2/0 | none |

## 08-argocd-external-helm

Reported result: **Partial**. Accepted `2`, refused `1`.
Unsupported constructs summary: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | Resources rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `applications` | `plain` | true | `argocd` | 3/3/0 | none |
| `extras/ingress-nginx` | `plain` | true | `ingress-nginx` | 2/2/0 | none |
| `platform/cert-manager` | `plain` | false | `argocd` | 2/2/1 | non-krm-yaml: values.yaml: YAML is not a Kubernetes manifest |

## 09-flux-monorepo

Reported result: **Partial**. Accepted `4`, refused `2`.
Unsupported constructs summary: `patches`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | Resources rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `apps/production` | `refused-structural` | false | `production` | 2/0/0 | refused-structural: kustomization uses unsupported feature(s): patches |
| `apps/staging` | `refused-structural` | false | `staging` | 2/0/0 | refused-structural: kustomization uses unsupported feature(s): patches |
| `clusters/production` | `kustomize-single` | true | `flux-system` | 7/7/0 | none |
| `clusters/staging` | `kustomize-single` | true | `flux-system` | 7/7/0 | none |
| `infrastructure/configs` | `kustomize-single` | true | `` | 1/1/0 | none |
| `infrastructure/controllers` | `kustomize-single` | true | `` | 2/2/0 | none |

## 10-flux-helmrelease

Reported result: **Partial**. Accepted `3`, refused `1`.
Unsupported constructs summary: `configMapGenerator`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | Resources rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `apps/frontend` | `refused-structural` | false | `` | 1/1/1 | refused-structural: kustomization uses unsupported feature(s): configMapGenerator |
| `clusters/production` | `kustomize-single` | true | `flux-system` | 7/7/0 | none |
| `infrastructure/controllers/ingress-nginx` | `kustomize-single` | true | `flux-system` | 3/3/0 | none |
| `infrastructure/sources` | `kustomize-single` | true | `flux-system` | 3/3/0 | none |

## 11-repo-per-environment

Reported result: **Partial**. Accepted `6`, refused `3`.
Unsupported constructs summary: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | Resources rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `gitops-dev` | `plain` | false | `` | 6/6/1 | foreign-file: .gitignore: foreign file .gitignore is not a managed manifest; remove it or name it in .gittargetignore |
| `gitops-dev/apps/backend` | `plain` | true | `backend-dev` | 2/2/0 | none |
| `gitops-dev/apps/frontend` | `plain` | true | `frontend-dev` | 2/2/0 | none |
| `gitops-production` | `plain` | false | `` | 7/7/1 | foreign-file: .gitignore: foreign file .gitignore is not a managed manifest; remove it or name it in .gittargetignore |
| `gitops-production/apps/backend` | `plain` | true | `backend-production` | 2/2/0 | none |
| `gitops-production/apps/frontend` | `plain` | true | `frontend-production` | 3/3/0 | none |
| `gitops-staging` | `plain` | false | `` | 6/6/1 | foreign-file: .gitignore: foreign file .gitignore is not a managed manifest; remove it or name it in .gittargetignore |
| `gitops-staging/apps/backend` | `plain` | true | `backend-staging` | 2/2/0 | none |
| `gitops-staging/apps/frontend` | `plain` | true | `frontend-staging` | 2/2/0 | none |

## 12-multicluster-applicationset

Reported result: **All reported candidates accepted**. Accepted `4`, refused `0`.
Unsupported constructs summary: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | Resources rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `applicationsets` | `plain` | true | `argocd` | 1/1/0 | none |
| `apps/backend` | `plain` | true | `` | 2/2/0 | none |
| `apps/frontend` | `plain` | true | `` | 2/2/0 | none |
| `clusters` | `plain` | true | `argocd` | 3/3/0 | none |

## 13-sops-encrypted

Reported result: **All reported candidates accepted**. Accepted `3`, refused `0`.
Unsupported constructs summary: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | Resources rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `apps/frontend` | `kustomize-single` | true | `frontend-production` | 2/2/0 | none |
| `clusters/production` | `plain` | true | `flux-system` | 2/2/0 | none |
| `infrastructure/secrets` | `kustomize-single` | true | `infrastructure` | 1/1/0 | none |

## 14-rendered-manifests

Reported result: **Partial**. Accepted `3`, refused `2`.
Unsupported constructs summary: `namePrefix`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | Resources rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `argocd` | `plain` | true | `argocd` | 1/1/0 | none |
| `rendered/production` | `plain` | true | `frontend-production` | 3/3/0 | none |
| `rendered/staging` | `plain` | true | `frontend-staging` | 3/3/0 | none |
| `src/frontend/overlays/production` | `refused-structural` | false | `frontend-production` | 2/0/0 | refused-structural: kustomization uses unsupported feature(s): namePrefix |
| `src/frontend/overlays/staging` | `refused-structural` | false | `frontend-staging` | 2/0/0 | refused-structural: kustomization uses unsupported feature(s): namePrefix |

## 15-mixed-and-hostile

Reported result: **Partial**. Accepted `3`, refused `2`.
Unsupported constructs summary: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | Resources rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `.` | `kustomize-single` | false | `backend` | 0/0/6 | non-krm-yaml: ci/.gitlab-ci.yml: YAML is not a Kubernetes manifest; non-krm-yaml: ci/docker-compose.yml: YAML is not a Kubernetes manifest; foreign-file: empty-dir/.gitkeep: foreign file empty-dir/.gitkeep is not a managed manifest; remove it or name it in .gittargetignore; mixed-managed-allowlisted: kustomization.yaml: managed resource kustomize.toolkit.fluxcd.io/v1/Kustomization/flux-system/apps must not live in the allowlisted build-directive file kustomization.yaml; impure-managed-file: mixed/bundle.yaml: a file with managed resources may contain only valid KRM documents; document #1 is a non-KRM document; impure-managed-file: mixed/bundle.yaml: a file with managed resources may contain only valid KRM documents; document #2 is an empty document; foreign-file: mixed/deployment.json: foreign file mixed/deployment.json is not a managed manifest; remove it or name it in .gittargetignore; invalid-yaml: templates/deployment.yaml: invalid YAML: yaml: line 21: did not find expected key; non-krm-yaml: values.yaml: YAML is not a Kubernetes manifest |
| `crossplane` | `plain` | true | `` | 1/1/0 | none |
| `kro` | `plain` | true | `` | 1/1/0 | none |
| `mixed` | `plain` | false | `` | 3/3/2 | impure-managed-file: bundle.yaml: a file with managed resources may contain only valid KRM documents; document #1 is a non-KRM document; impure-managed-file: bundle.yaml: a file with managed resources may contain only valid KRM documents; document #2 is an empty document; foreign-file: deployment.json: foreign file deployment.json is not a managed manifest; remove it or name it in .gittargetignore |
| `secrets` | `plain` | true | `backend` | 1/1/0 | none |

## 16-flux-image-automation

Reported result: **All reported candidates accepted**. Accepted `3`, refused `0`.
Unsupported constructs summary: `none`. Fleet root: `false`.

| Candidate | Layout | Accepted today | Namespace | Resources rendered/editable/non-KRM | Refusal reasons |
|---|---|---|---|---|---|
| `apps/frontend` | `kustomize-single` | true | `frontend` | 2/2/0 | none |
| `clusters/production` | `kustomize-single` | true | `flux-system` | 2/2/0 | none |
| `infrastructure/image-automation` | `kustomize-single` | true | `flux-system` | 3/3/0 | none |
