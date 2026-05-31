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
