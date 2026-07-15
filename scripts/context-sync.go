package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
)

const (
	startMarker = "<!-- BEGIN GENERATED: REPORT_CATALOG -->"
	endMarker   = "<!-- END GENERATED: REPORT_CATALOG -->"
)

func tableCell(value string) string {
	return strings.NewReplacer("|", `\|`, "\n", " ").Replace(value)
}

func renderReportCatalog(definitions []report.Definition) string {
	lines := []string{
		"| Key | Thai label | Version | Status | Period mode | Refresh class | Chunk safe |",
		"| --- | --- | --- | --- | --- | --- | --- |",
	}
	for _, definition := range definitions {
		lines = append(lines, fmt.Sprintf(
			"| `%s` | %s | `%s` | `%s` | `%s` | `%s` | `%t` |",
			tableCell(string(definition.Key)),
			tableCell(definition.LabelTH),
			tableCell(definition.Version),
			tableCell(string(definition.Status)),
			tableCell(string(definition.ParameterKind)),
			tableCell(string(definition.RefreshClass)),
			definition.ChunkSafe,
		))
	}
	return strings.Join(lines, "\n")
}

func replaceGeneratedBlock(document, generated string) (string, error) {
	start := strings.Index(document, startMarker)
	end := strings.Index(document, endMarker)
	if start < 0 || end < 0 || end <= start {
		return "", errors.New("report catalog marker pair is missing or invalid")
	}
	if strings.Count(document, startMarker) != 1 || strings.Count(document, endMarker) != 1 {
		return "", errors.New("report catalog markers must appear exactly once")
	}
	contentStart := start + len(startMarker)
	return document[:contentStart] + "\n" + generated + "\n" + document[end:], nil
}

func main() {
	write := flag.Bool("write", false, "write generated context")
	check := flag.Bool("check", false, "check generated context without writing")
	flag.Parse()
	if *write == *check {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/context-sync.go --write|--check")
		os.Exit(2)
	}
	const documentPath = "docs/knowledge/02-domain-and-reports.md"
	currentBytes, err := os.ReadFile(documentPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "context sync failed: %v\n", err)
		os.Exit(1)
	}
	definitions := report.Definitions()
	expected, err := replaceGeneratedBlock(string(currentBytes), renderReportCatalog(definitions))
	if err != nil {
		fmt.Fprintf(os.Stderr, "context sync failed: %v\n", err)
		os.Exit(1)
	}
	if *check {
		if expected != string(currentBytes) {
			fmt.Fprintln(os.Stderr, "context sync failed: report catalog is stale; run make context-sync")
			os.Exit(1)
		}
		fmt.Printf("context sync ok: reports=%d\n", len(definitions))
		return
	}
	if expected != string(currentBytes) {
		if err := os.WriteFile(documentPath, []byte(expected), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "context sync failed: %v\n", err)
			os.Exit(1)
		}
	}
	fmt.Printf("context sync updated: reports=%d\n", len(definitions))
}
