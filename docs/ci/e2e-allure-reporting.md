# E2E Allure Reporting

The CI e2e job uploads raw Ginkgo JSON reports for each matrix entry. Developers
can download those artifacts and render a visual Allure HTML report locally when
the timeline view is useful.

The e2e tasks still run through the repository Taskfile so local and CI execution
stay aligned. Each task writes Ginkgo JSON reports under
`.stamps/cluster/<ctx>/<namespace>/ginkgo-report-<suite>.json`. After the e2e
run, CI uploads those files, plus the timing summary, as the
`e2e-ginkgo-reports-<matrix-name>` artifact.

The raw Ginkgo JSON files are the source of truth for local tooling and timing
analysis. To render an Allure report from downloaded or locally produced Ginkgo
JSON files, use:

```bash
go run ./test/e2e/tools/ginkgo-allure --output-dir allure-results <ginkgo-report.json>...
```

During an active e2e run, GitHub Actions logs remain the live view. After the run
finishes, the downloaded Ginkgo JSON can be converted locally into an Allure
report with a visual timeline, status breakdown, labels, durations, and captured
Ginkgo output.

The converter is deliberately small and one-way:

- Ginkgo `passed` maps to Allure `passed`.
- Ginkgo `failed` maps to Allure `failed`.
- Ginkgo `pending` and `skipped` map to Allure `skipped`.
- Ginkgo timeout, interrupt, abort, and panic states map to Allure `broken`.
- Captured `GinkgoWriter` and parallel stdout/stderr output are attached as
  plain-text test artifacts.

## Allure labels

The converter attaches labels so the report's grouping and timeline views are
useful for a parallel suite:

- `thread` — `ginkgo-process-<N>`, the Ginkgo parallel process that ran the
  spec. This is what gives the Allure timeline its per-process lanes, so you can
  see how specs were distributed across processes and spot a single overloaded
  lane. Only emitted for parallel runs (`ParallelProcess > 0`).
- `suite` — the Ginkgo suite description.
- `package` — the base name of the suite's source path.
- `parentSuite` / `subSuite` — the spec's container hierarchy (outermost
  `Describe` as `parentSuite`, the remaining nested containers as `subSuite`).
- `tag` — one per Ginkgo `Label(...)`, plus a `Serial` tag for specs that ran
  serially.

## Local devcontainer use

The devcontainer does not install Java or the Allure CLI. Local report rendering
uses Docker to run Allure in an on-demand Java runtime container, keeping the
devcontainer image smaller.

Run any e2e task that writes a Ginkgo JSON report, for example:

```bash
task test-e2e
```

Then generate a local Allure HTML report:

```bash
task allure-e2e-report
```

The report is written to `.stamps/allure-report`. To serve it from inside the
devcontainer on the forwarded Allure port:

```bash
task allure-e2e-open
```

This serves the report on port `19081`. By default, the local Allure task reads
the newest Ginkgo JSON report from the current e2e context and namespace:
`.stamps/cluster/<ctx>/<namespace>/ginkgo-report-*.json`.

To render a different set of reports, set `ALLURE_GINKGO_REPORTS_DIR` to another
directory or set `ALLURE_GINKGO_REPORTS` to an explicit whitespace-separated
list of report files. To render every report in the selected directory, set
`ALLURE_INCLUDE_ALL_GINKGO_REPORTS=true`.

The same tasks can reuse an older local Ginkgo JSON report in that directory, so
you can generate the report after a failed run without rerunning the suite.

The first local render pulls the `eclipse-temurin:17-jre` image and downloads the
Allure CLI into `.stamps/allure-cli`. Later renders reuse both Docker's image
cache and the local Allure CLI cache.
