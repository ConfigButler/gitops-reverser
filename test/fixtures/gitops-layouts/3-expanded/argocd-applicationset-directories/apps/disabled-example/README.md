# disabled-example (intentionally excluded)

This directory is deliberately excluded from the ApplicationSet git directory
generator via an `exclude: true` entry (`path: apps/disabled-example`) in
`bootstrap/applicationset.yaml`.

Even though it sits under `apps/`, and even though `apps/*` would otherwise
match it, no `Application` is generated for it. It contains no Kubernetes
manifests — only this note — so there is nothing here to deploy. It exists to
show that presence under the wildcard path is not sufficient: an explicit
exclusion overrides the match.
