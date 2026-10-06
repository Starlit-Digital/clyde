package clyde

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"unicode/utf8"

	gcf "github.com/blackwell-systems/gcf-go"
)

type gcfSizeReport struct {
	InputBytes       int     `json:"input_bytes"`
	CompactJSONBytes int     `json:"compact_json_bytes"`
	GCFBytes         int     `json:"gcf_bytes"`
	SavingsBytes     int     `json:"savings_bytes"`
	SavingsPercent   float64 `json:"savings_percent"`
}

func printGCFHelp(out io.Writer) {
	fmt.Fprintln(out, "usage: clyde gcf {encode|decode|stats} [FILE|-] [--json (stats only)]")
	fmt.Fprintln(out, "\nConvert JSON to GCF, decode GCF to JSON, or compare GCF with compact JSON.")
	fmt.Fprintln(out, "Reads stdin when FILE is omitted or '-'. Input is limited to 1 MiB.")
	fmt.Fprintln(out, "All operations run locally. Stats measure UTF-8 bytes, not model tokens.")
	fmt.Fprintln(out, "\nexamples:\n  clyde preview . --json | clyde gcf encode\n  clyde gcf decode report.gcf\n  clyde gcf stats report.json --json")
}

func cmdGCF(args []string, stdin io.Reader, out io.Writer) error {
	if len(args) == 0 || isHelpArgs(args) {
		printGCFHelp(out)
		return nil
	}
	action := args[0]
	if action != "encode" && action != "decode" && action != "stats" {
		return errf("gcf requires encode, decode, or stats")
	}
	fs := flag.NewFlagSet("gcf "+action, flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() { printGCFHelp(out) }
	var jsonOut bool
	if action == "stats" {
		fs.BoolVar(&jsonOut, "json", false, "print machine-readable byte comparison")
	}
	if err := fs.Parse(interspersedArgs(args[1:], map[string]bool{"json": true})); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return errf("gcf %s accepts at most one FILE", action)
	}
	reader := stdin
	if fs.NArg() == 1 && fs.Arg(0) != "-" {
		file, err := os.Open(fs.Arg(0))
		if err != nil {
			return err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errf("GCF input must be a regular file")
		}
		reader = file
	}
	input, err := readLimitedText(reader, maxPromptInputBytes, "GCF conversion")
	if err != nil {
		return err
	}
	if !utf8.Valid(input) {
		return errf("GCF conversion input must be valid UTF-8")
	}
	if action == "decode" {
		value, err := gcf.DecodeGeneric(string(input))
		if err != nil {
			return errf("decode GCF: %w", err)
		}
		return printStructured(out, value, false)
	}
	// The SDK's ordered parser reads one value. Validate the entire input
	// first so trailing values or garbage cannot be silently discarded.
	if !json.Valid(input) {
		return errf("GCF conversion requires one valid JSON value")
	}
	if err := validateJSONKeys(input); err != nil {
		return err
	}
	value, err := gcf.ParseJSONOrdered(input)
	if err != nil {
		return errf("parse JSON: %w", err)
	}
	wire, err := gcf.EncodeGenericChecked(value, gcf.GenericOptions{NoFlatten: true})
	if err != nil {
		return err
	}
	if action == "encode" {
		_, err := io.WriteString(out, wire)
		return err
	}
	compact, err := json.Marshal(value)
	if err != nil {
		return err
	}
	report := gcfSizeReport{InputBytes: len(input), CompactJSONBytes: len(compact), GCFBytes: len(wire), SavingsBytes: len(compact) - len(wire)}
	report.SavingsPercent = float64(report.SavingsBytes) * 100 / float64(report.CompactJSONBytes)
	if jsonOut {
		return printStructured(out, report, false)
	}
	_, err = fmt.Fprintf(out, "Input: %d bytes\nCompact JSON: %d bytes\nGCF: %d bytes\nSavings vs compact JSON: %d bytes (%.1f%%)\nMeasured in UTF-8 bytes; token counts depend on the model tokenizer.\n", report.InputBytes, report.CompactJSONBytes, report.GCFBytes, report.SavingsBytes, report.SavingsPercent)
	return err
}

// JSON allows repeated object keys, but GCF cannot preserve their meaning.
// Reject them before the SDK's ordered-map parser can overwrite a value.
func validateJSONKeys(input []byte) error {
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	var visit func(int) error
	visit = func(depth int) error {
		if depth > 256 {
			return errf("GCF conversion nesting exceeds 256 levels")
		}
		token, err := dec.Token()
		if err != nil {
			return err
		}
		switch token {
		case json.Delim('{'):
			seen := make(map[string]bool)
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return err
				}
				name := key.(string)
				if seen[name] {
					return errf("GCF conversion rejects duplicate JSON object keys")
				}
				seen[name] = true
				if err := visit(depth + 1); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		case json.Delim('['):
			for dec.More() {
				if err := visit(depth + 1); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		}
		return nil
	}
	return visit(0)
}
