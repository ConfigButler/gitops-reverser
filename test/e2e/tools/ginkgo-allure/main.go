// SPDX-License-Identifier: Apache-2.0

// ginkgo-allure converts Ginkgo JSON reports into Allure result files.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2/types"
)

const (
	exitUsage     = 2
	usageInstruct = "usage: ginkgo-allure --output-dir <allure-results> <ginkgo-report.json>..."
)

func main() {
	os.Exit(mainRun(os.Args[1:], os.Stdout, os.Stderr))
}

func mainRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ginkgo-allure", flag.ContinueOnError)
	fs.SetOutput(stderr)
	outputDir := fs.String("output-dir", "allure-results", "directory for generated Allure result files")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *outputDir == "" || fs.NArg() == 0 {
		fmt.Fprintln(stderr, usageInstruct)
		return exitUsage
	}
	count, err := run(*outputDir, fs.Args())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "wrote %d Allure result files to %s\n", count, *outputDir)
	return 0
}

type allureResult struct {
	UUID          string               `json:"uuid"`
	HistoryID     string               `json:"historyId"`
	TestCaseID    string               `json:"testCaseId"`
	FullName      string               `json:"fullName"`
	Name          string               `json:"name"`
	Status        string               `json:"status"`
	StatusDetails *allureStatusDetails `json:"statusDetails,omitempty"`
	Stage         string               `json:"stage"`
	Start         int64                `json:"start,omitempty"`
	Stop          int64                `json:"stop,omitempty"`
	Labels        []allureLabel        `json:"labels,omitempty"`
	Parameters    []allureParameter    `json:"parameters,omitempty"`
	Attachments   []allureAttachment   `json:"attachments,omitempty"`
}

type allureStatusDetails struct {
	Message string `json:"message,omitempty"`
	Trace   string `json:"trace,omitempty"`
}

type allureLabel struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type allureParameter struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type allureAttachment struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Type   string `json:"type"`
}

func run(outputDir string, paths []string) (int, error) {
	if err := os.MkdirAll(outputDir, 0o750); err != nil {
		return 0, fmt.Errorf("create output dir %s: %w", outputDir, err)
	}

	count := 0
	for _, path := range paths {
		reports, err := readReports(path)
		if err != nil {
			return count, err
		}
		reportCount, err := convertReports(outputDir, path, reports)
		if err != nil {
			return count, err
		}
		count += reportCount
	}
	return count, nil
}

func convertReports(outputDir, path string, reports []types.Report) (int, error) {
	count := 0
	for reportIndex, report := range reports {
		if report.SuiteConfig.DryRun {
			continue
		}
		for specIndex, spec := range report.SpecReports {
			if spec.LeafNodeType != types.NodeTypeIt || specSkippedByFilter(report, spec) {
				continue
			}
			result := convertSpec(path, reportIndex, specIndex, report, spec)
			if err := writeResult(outputDir, result, spec.CombinedOutput()); err != nil {
				return count, err
			}
			count++
		}
	}
	return count, nil
}

func readReports(path string) ([]types.Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	var reports []types.Report
	if err := json.Unmarshal(data, &reports); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return reports, nil
}

func convertSpec(path string, reportIndex, specIndex int, report types.Report, spec types.SpecReport) allureResult {
	fullName := spec.FullText()
	if fullName == "" {
		fullName = spec.LeafNodeText
	}
	if fullName == "" {
		fullName = fmt.Sprintf("%s#%d", filepath.Base(path), specIndex)
	}

	start, stop := specTimes(report, spec)
	result := allureResult{
		UUID:       uuidFromParts(path, reportIndex, specIndex, fullName),
		HistoryID:  hashString("history", fullName),
		TestCaseID: hashString("test-case", fullName),
		FullName:   fullName,
		Name:       resultName(spec),
		Status:     allureStatus(spec.State),
		Stage:      "finished",
		Start:      unixMillis(start),
		Stop:       unixMillis(stop),
		Labels:     labels(report, spec),
		Parameters: []allureParameter{
			{Name: "ginkgo.parallel_process", Value: strconv.Itoa(spec.ParallelProcess)},
			{Name: "ginkgo.random_seed", Value: strconv.FormatInt(report.SuiteConfig.RandomSeed, 10)},
		},
	}
	if details := statusDetails(spec); details != nil {
		result.StatusDetails = details
	}
	return result
}

func writeResult(outputDir string, result allureResult, combinedOutput string) error {
	if strings.TrimSpace(combinedOutput) != "" {
		source := result.UUID + "-attachment.txt"
		if err := os.WriteFile(filepath.Join(outputDir, source), []byte(combinedOutput), 0o600); err != nil {
			return fmt.Errorf("write attachment %s: %w", source, err)
		}
		result.Attachments = append(result.Attachments, allureAttachment{
			Name:   "Ginkgo output",
			Source: source,
			Type:   "text/plain",
		})
	}

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal result %s: %w", result.UUID, err)
	}
	name := result.UUID + "-result.json"
	if err := os.WriteFile(filepath.Join(outputDir, name), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write result %s: %w", name, err)
	}
	return nil
}

func resultName(spec types.SpecReport) string {
	if spec.LeafNodeText != "" {
		return spec.LeafNodeText
	}
	return spec.FullText()
}

func labels(report types.Report, spec types.SpecReport) []allureLabel {
	out := []allureLabel{
		{Name: "language", Value: "go"},
		{Name: "framework", Value: "ginkgo"},
	}
	if spec.ParallelProcess > 0 {
		out = append(out, allureLabel{Name: "thread", Value: fmt.Sprintf("ginkgo-process-%d", spec.ParallelProcess)})
	}
	if report.SuiteDescription != "" {
		out = append(out, allureLabel{Name: "suite", Value: report.SuiteDescription})
	}
	if report.SuitePath != "" {
		out = append(out, allureLabel{Name: "package", Value: filepath.Base(report.SuitePath)})
	}
	if len(spec.ContainerHierarchyTexts) > 0 {
		out = append(out, allureLabel{Name: "parentSuite", Value: spec.ContainerHierarchyTexts[0]})
	}
	if len(spec.ContainerHierarchyTexts) > 1 {
		out = append(out, allureLabel{Name: "subSuite", Value: strings.Join(spec.ContainerHierarchyTexts[1:], " ")})
	}
	for _, label := range spec.Labels() {
		out = append(out, allureLabel{Name: "tag", Value: label})
	}
	if spec.IsSerial {
		out = append(out, allureLabel{Name: "tag", Value: "Serial"})
	}
	return out
}

func allureStatus(state types.SpecState) string {
	switch {
	case state == types.SpecStatePassed:
		return "passed"
	case state == types.SpecStateSkipped || state == types.SpecStatePending:
		return "skipped"
	case state == types.SpecStateFailed:
		return "failed"
	case state.Is(types.SpecStateAborted | types.SpecStatePanicked | types.SpecStateInterrupted | types.SpecStateTimedout):
		return "broken"
	default:
		return "unknown"
	}
}

func specSkippedByFilter(report types.Report, spec types.SpecReport) bool {
	return report.SuiteConfig.LabelFilter != "" &&
		spec.State == types.SpecStateSkipped &&
		spec.EndTime.IsZero() &&
		spec.RunTime == 0 &&
		spec.Failure.IsZero()
}

func statusDetails(spec types.SpecReport) *allureStatusDetails {
	if spec.Failure.IsZero() {
		return nil
	}
	trace := spec.Failure.Location.FullStackTrace
	if trace == "" && spec.Failure.Location != (types.CodeLocation{}) {
		trace = spec.Failure.Location.String()
	}
	if spec.Failure.ForwardedPanic != "" {
		if trace != "" {
			trace += "\n"
		}
		trace += spec.Failure.ForwardedPanic
	}
	return &allureStatusDetails{
		Message: spec.Failure.Message,
		Trace:   trace,
	}
}

func specTimes(report types.Report, spec types.SpecReport) (time.Time, time.Time) {
	start := spec.StartTime
	if start.IsZero() {
		start = report.StartTime
	}
	stop := spec.EndTime
	if stop.IsZero() && !start.IsZero() {
		stop = start.Add(spec.RunTime)
	}
	return start, stop
}

func unixMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano() / int64(time.Millisecond)
}

func uuidFromParts(parts ...any) string {
	hash := hashParts(parts...)
	hexed := hex.EncodeToString(hash[:16])
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexed[0:8], hexed[8:12], hexed[12:16], hexed[16:20], hexed[20:32])
}

func hashString(parts ...any) string {
	hash := hashParts(parts...)
	return hex.EncodeToString(hash[:16])
}

func hashParts(parts ...any) [32]byte {
	h := sha256.New()
	for _, part := range parts {
		_, _ = fmt.Fprintf(h, "%v\x00", part)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}
