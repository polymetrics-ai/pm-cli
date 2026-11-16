package clientparity

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPlaywrightFailureTextNamesFailingSpecs checks the failure text of a red
// Playwright run without a browser or the dependency stack.
func TestPlaywrightFailureTextNamesFailingSpecs(t *testing.T) {
	const exitLine = "pinned Playwright failed: exit status 1 (stdout=7 bytes, stderr=9 bytes)"
	const failedReport = `{"suites":[{"specs":[
		{"title":"pm-browser-client-parity-E01-C01 different workspace remains isolated","ok":true,
		 "tests":[{"expectedStatus":"passed","results":[{"status":"passed"}]}]},
		{"title":"pm-browser-client-parity-E01-C02 public SDK has no admin secret route","ok":false,
		 "tests":[{"expectedStatus":"passed","results":[{"status":"failed"}]}]}],
		"suites":[{"specs":[
		{"title":"pm-browser-client-parity-E01-C08 AbortSignal closes only viewing","ok":false,
		 "tests":[{"expectedStatus":"passed","results":[{"status":"timedOut"}]}]}]}]}],"errors":[]}`
	cases := []struct {
		name     string
		document *string
		want     string
	}{
		{
			name:     "failing specs",
			document: ptr(failedReport),
			want: `Playwright spec "pm-browser-client-parity-E01-C02 public SDK has no admin secret route" did not execute once and pass` + "\n" +
				`Playwright spec "pm-browser-client-parity-E01-C08 AbortSignal closes only viewing" did not execute once and pass` + "\n" +
				exitLine,
		},
		{name: "empty report", document: ptr(""), want: exitLine},
		{name: "unparsed report", document: ptr(`{"suites":[`), want: exitLine},
		{name: "missing report", want: exitLine},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "playwright-report.json")
			if testCase.document != nil {
				if err := os.WriteFile(path, []byte(*testCase.document), 0o600); err != nil {
					t.Fatal("write fixture report")
				}
			}
			report := readPlaywrightFailureReport(path, playwrightReportLimit)
			got := playwrightFailureText(report, errors.New("exit status 1"), 7, 9)
			if got != testCase.want {
				t.Fatalf("failure text:\n%s\nwant:\n%s", got, testCase.want)
			}
		})
	}
}

func TestReadPlaywrightFailureReportRejectsUnownedReports(t *testing.T) {
	directory := t.TempDir()
	oversized := filepath.Join(directory, "oversized.json")
	if err := os.WriteFile(oversized, []byte(strings.Repeat(" ", 17)), 0o600); err != nil {
		t.Fatal("write oversized fixture report")
	}
	if report := readPlaywrightFailureReport(oversized, 16); report != nil {
		t.Fatalf("oversized report returned %d bytes; want none", len(report))
	}
	link := filepath.Join(directory, "link.json")
	if err := os.Symlink(oversized, link); err != nil {
		t.Fatal("create fixture report link")
	}
	if report := readPlaywrightFailureReport(link, playwrightReportLimit); report != nil {
		t.Fatalf("linked report returned %d bytes; want none", len(report))
	}
}

func ptr(value string) *string { return &value }
