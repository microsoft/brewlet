package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// Result commands print human-readable text by default. JSON is opt-in with
// -o json and is never inferred from whether stdout is a terminal or a pipe,
// so a command behaves the same interactively and in scripts.
const (
	outputText = "text"
	outputJSON = "json"
)

// addOutputFlag registers --output/-o for commands that print a result.
func addOutputFlag(fs *flag.FlagSet) *string {
	output := fs.String("output", outputText, "stdout format: text|json")
	fs.StringVar(output, "o", outputText, "shorthand for --output")
	return output
}

func checkTextOutput(name, output string) error {
	return k8sCheckOutput(name, output, outputText, outputJSON)
}

// writeJSON writes v as indented JSON followed by a newline.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

type field struct{ name, value string }

// writeFields prints aligned "name:  value" rows, skipping empty values.
func writeFields(w io.Writer, indent string, fields ...field) error {
	tw := tabwriter.NewWriter(w, 0, 4, 1, ' ', 0)
	for _, f := range fields {
		if f.value != "" {
			fmt.Fprintf(tw, "%s%s:\t%s\n", indent, f.name, f.value)
		}
	}
	return tw.Flush()
}

// writeTable prints a header and rows aligned in columns.
func writeTable(w io.Writer, indent string, header []string, rows [][]string) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "%s%s\n", indent, strings.Join(header, "\t"))
	for _, row := range rows {
		fmt.Fprintf(tw, "%s%s\n", indent, strings.Join(row, "\t"))
	}
	return tw.Flush()
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

func platformLabel(p *v1.Platform) string {
	if p == nil {
		return "-"
	}
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// indentJSON re-indents raw JSON so every line sits under prefix.
func indentJSON(raw []byte, prefix string) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, prefix, "  "); err != nil {
		return prefix + strings.TrimSpace(string(raw))
	}
	return prefix + strings.TrimSpace(buf.String())
}
