#!/usr/bin/env bash
# Regenerate test/fixtures/gitops-layouts/support-today.md from a live
# manifest-analyzer scan of every fixture in the corpus.
#
# The output is a BEHAVIOURAL BASELINE, not documentation: it records what the
# analyzer reports today, so that any change to the acceptance boundary shows up
# in review as a diff of exactly which fixtures moved, and in which direction.
#
# It carries no interpretation. Interpretation belongs in
# docs/design/support-boundary/, which is free to disagree with this file — that
# disagreement is the backlog.
#
# Usage: task gitops-layouts-baseline
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

corpus="test/fixtures/gitops-layouts"
out="$corpus/support-today.md"
analyzer="$(mktemp -d)/manifest-analyzer"
trap 'rm -rf "$(dirname "$analyzer")"' EXIT

echo "building manifest-analyzer..." >&2
go build -o "$analyzer" ./cmd/manifest-analyzer

# Every fixture is <family>/<fixture>, two levels below the corpus root.
mapfile -t fixtures < <(find "$corpus" -mindepth 2 -maxdepth 2 -type d | sort)

emit_summary_row() {
  # $1 = fixture path relative to the corpus, $2 = rc, $3 = json
  local name="$1" rc="$2" json="$3"
  local accepted refused layouts constructs outcome signal
  accepted=$(jq -r '.summary.accepted // 0' <<<"$json")
  refused=$(jq -r '.summary.refused // 0' <<<"$json")
  layouts=$(jq -r '(.summary.candidatesByLayout // {}) | to_entries
                   | map("\(.key)=\(.value)") | join(", ") | if . == "" then "-" else . end' <<<"$json")
  constructs=$(jq -r '(.summary.unsupportedConstructs // []) | join(", ")
                      | if . == "" then "-" else . end' <<<"$json")
  signal=$(jq -r '[.candidates[]? | select(.acceptedByOperator == false)
                   | .refusalReasons[]? | "\(.code): \(.detail)"]
                  | if length == 0 then "None"
                    elif length > 4 then (.[0:4] | join("<br>")) + "<br>+\(length - 4) more"
                    else join("<br>") end' <<<"$json")

  if [[ "$rc" != "0" ]]; then
    outcome="Scan failed"
  elif [[ "$accepted" == "0" && "$refused" == "0" ]]; then
    outcome="No candidates reported"
  elif [[ "$refused" == "0" ]]; then
    outcome="All reported candidates accepted"
  elif [[ "$accepted" == "0" ]]; then
    outcome="No reported candidates accepted"
  else
    outcome="Partial"
  fi

  printf '| %s | %s | %s | %s | %s | %s | %s | %s |\n' \
    "$name" "$rc" "$outcome" "$accepted" "$refused" "$layouts" "$constructs" "$signal"
}

emit_detail() {
  local name="$1" rc="$2" json="$3"
  local accepted refused constructs fleet
  accepted=$(jq -r '.summary.accepted // 0' <<<"$json")
  refused=$(jq -r '.summary.refused // 0' <<<"$json")
  constructs=$(jq -r '(.summary.unsupportedConstructs // []) | join(", ")
                      | if . == "" then "none" else . end' <<<"$json")
  fleet=$(jq -r '.summary.fleetRoot // false' <<<"$json")

  printf '\n## %s\n\n' "$name"
  printf 'Reported rc `%s`. Accepted `%s`, refused `%s`.\n' "$rc" "$accepted" "$refused"
  printf 'Unsupported constructs: `%s`. Fleet root: `%s`.\n\n' "$constructs" "$fleet"

  if [[ "$(jq -r '(.candidates // []) | length' <<<"$json")" == "0" ]]; then
    printf '_No candidate folders reported._\n'
    return
  fi

  printf '| Candidate | Layout | Accepted today | Namespace | rendered/editable/non-KRM | Refusal reasons |\n'
  printf '|---|---|---|---|---|---|\n'
  jq -r '.candidates[]
         | "| `\(.path)` | `\(.layout)` | \(.acceptedByOperator) | `\(.inferredNamespace // "-")` | "
           + "\(.resources.rendered // 0)/\(.resources.editable // 0)/\(.resources.nonKrm // 0) | "
           + ((.refusalReasons // []) | map("\(.code): \(.detail)") | join("<br>")
              | if . == "" then "none" else . end)
           + " |"' <<<"$json"
}

{
  cat <<'PREAMBLE'
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
PREAMBLE

  declare -a details=()
  for dir in "${fixtures[@]}"; do
    name="${dir#"$corpus"/}"
    set +e
    json="$("$analyzer" --mode scan-repo --format json "$dir" 2>/dev/null)"
    rc=$?
    set -e
    [[ -n "$json" ]] || json='{}'
    emit_summary_row "$name" "$rc" "$json"
    details+=("$(emit_detail "$name" "$rc" "$json")")
  done

  printf '%s\n' "${details[@]}"
} >"$out"

echo "wrote $out ($(wc -l <"$out") lines, ${#fixtures[@]} fixtures)" >&2
