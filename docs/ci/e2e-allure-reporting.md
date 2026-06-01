# E2E Allure Reporting

The CI e2e job produces a visual Allure HTML report for each matrix entry.

The e2e tasks still run through the repository Taskfile so local and CI execution
stay aligned. Each task writes Ginkgo JSON reports under
`.stamps/cluster/<ctx>/<namespace>/ginkgo-report-<suite>.json`. After the e2e
run, CI converts those Ginkgo JSON files into Allure result files with:

```bash
go run ./test/e2e/tools/ginkgo-allure --output-dir allure-results <ginkgo-report.json>...
```

Then `simple-elf/allure-report-action` renders `allure-report`, and CI uploads
the generated HTML as the `e2e-allure-report-<matrix-name>` artifact.

This is intentionally a post-run report. During an active e2e run, GitHub Actions
logs remain the live view. After the run finishes, the Allure artifact provides a
visual timeline, status breakdown, labels, durations, and captured Ginkgo output.

The converter is deliberately small and one-way:

- Ginkgo `passed` maps to Allure `passed`.
- Ginkgo `failed` maps to Allure `failed`.
- Ginkgo `pending` and `skipped` map to Allure `skipped`.
- Ginkgo timeout, interrupt, abort, and panic states map to Allure `broken`.
- Captured `GinkgoWriter` and parallel stdout/stderr output are attached as
  plain-text test artifacts.

The existing raw Ginkgo JSON and timing-summary artifacts are still uploaded.
Those files remain the source of truth for local tooling and timing analysis.

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
