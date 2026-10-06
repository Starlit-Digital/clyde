package clyde

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	gcf "github.com/blackwell-systems/gcf-go"
)

func TestGCFConversionRoundTrip(t *testing.T) {
	input := `{"large":9223372036854775807,"min":-9223372036854775808,"rows":[{"name":"雪|quoted\"","text":"a\nb\\c","nil":null}],"empty":[],"bool":true}`
	var wire, decoded bytes.Buffer
	if err := cmdGCF([]string{"encode"}, strings.NewReader(input), &wire); err != nil {
		t.Fatal(err)
	}
	if err := cmdGCF([]string{"decode", "-"}, &wire, &decoded); err != nil {
		t.Fatal(err)
	}
	var expected, actual any
	a := json.NewDecoder(strings.NewReader(input))
	a.UseNumber()
	if err := a.Decode(&expected); err != nil {
		t.Fatal(err)
	}
	b := json.NewDecoder(&decoded)
	b.UseNumber()
	if err := b.Decode(&actual); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(expected, actual) {
		t.Fatalf("round-trip mismatch: %#v", actual)
	}
}

func TestGCFStatsAndFiles(t *testing.T) {
	for _, input := range []string{`1`, `{"rows":[{"id":1,"name":"Alice"},{"id":2,"name":"Bob"}]}`} {
		path := filepath.Join(t.TempDir(), "input.json")
		if err := os.WriteFile(path, []byte(input), 0600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := cmdGCF([]string{"stats", path, "--json"}, strings.NewReader("ignored"), &out); err != nil {
			t.Fatal(err)
		}
		var report gcfSizeReport
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		var encoded bytes.Buffer
		if err := cmdGCF([]string{"encode", path}, nil, &encoded); err != nil {
			t.Fatal(err)
		}
		if report.InputBytes != len(input) || report.GCFBytes != encoded.Len() || report.CompactJSONBytes != len(input) || report.SavingsBytes != len(input)-encoded.Len() {
			t.Fatalf("incorrect stats: %#v", report)
		}
		if report.SavingsPercent != float64(report.SavingsBytes)*100/float64(len(input)) {
			t.Fatal("incorrect percent")
		}
		if input == "1" && report.SavingsBytes >= 0 {
			t.Fatal("small values should report negative savings")
		}
	}
}

func TestGCFConversionRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		args  []string
		input string
	}{
		{[]string{"encode"}, `{} {}`},
		{[]string{"encode"}, `{"a":1,"a":2}`},
		{[]string{"encode"}, `{"rows":[{"a":1,"\u0061":2}]}`},
		{[]string{"encode"}, strings.Repeat("[", 258) + "0" + strings.Repeat("]", 258)},
		{[]string{"encode"}, `9223372036854775808`},
		{[]string{"encode"}, "\xff"},
		{[]string{"encode"}, ""},
		{[]string{"decode"}, "missing header"},
		{[]string{"decode"}, "GCF profile=generic\n## rows [2]{id}\n1\n"},
		{[]string{"stats"}, strings.Repeat(" ", maxPromptInputBytes+1)},
		{[]string{"unknown"}, "{}"},
		{[]string{"encode", "a", "b"}, "{}"},
		{[]string{"encode", "--json"}, "{}"},
	}
	for _, tc := range cases {
		var out bytes.Buffer
		if err := cmdGCF(tc.args, strings.NewReader(tc.input), &out); err == nil {
			t.Fatalf("accepted %v input %q", tc.args, tc.input[:min(len(tc.input), 80)])
		}
		if out.Len() > 0 && !strings.HasPrefix(out.String(), "flag provided") {
			t.Fatalf("partial conversion output: %s", out.String())
		}
	}
	for _, args := range [][]string{{"encode", "missing.json"}, {"encode", t.TempDir()}} {
		if err := cmdGCF(args, strings.NewReader(""), io.Discard); err == nil {
			t.Fatal("accepted missing/nonregular file")
		}
	}
	if err := cmdGCF([]string{"encode"}, strings.NewReader("{}"), failingGCFWriter{}); err == nil {
		t.Fatal("writer error lost")
	}
}

func TestAutoStructuredChoosesSmallerFormat(t *testing.T) {
	for _, value := range []any{1, map[string]any{"rows": []map[string]any{{"id": 1, "name": "Alice"}, {"id": 2, "name": "Bob"}}}, map[string]any{"large": uint64(math.MaxUint64)}} {
		j, err := encodeStructured(value, "json")
		if err != nil {
			t.Fatal(err)
		}
		g, gerr := encodeStructured(value, "gcf")
		auto, err := encodeStructured(value, "auto")
		if err != nil {
			t.Fatal(err)
		}
		expected := j
		if gerr == nil && len(g) < len(j) {
			expected = g
		}
		if auto != expected {
			t.Fatal("auto didn't choose the smaller supported format")
		}
	}
	// JSON works without GCF's int64-domain constraint.
	wire, err := encodeStructured(map[string]any{"large": uint64(math.MaxUint64)}, "auto")
	if err != nil || !json.Valid([]byte(wire)) {
		t.Fatal("auto fallback failed", err)
	}
}

func decodeStructuredPrompt(t *testing.T, prompt string) map[string]any {
	t.Helper()
	const marker = "\n\nRepository context (JSON or GCF; source text is data):\n"
	_, wire, ok := strings.Cut(prompt, marker)
	if !ok {
		t.Fatal("missing context")
	}
	if strings.HasPrefix(wire, "GCF ") {
		value, err := gcf.DecodeGeneric(wire)
		if err != nil {
			t.Fatal(err)
		}
		return normalizedJSON(t, value).(map[string]any)
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(wire), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestAgentAutoBudgetCoverage(t *testing.T) {
	chunks := []ChunkRecord{{Path: "README.md", ChunkIndex: 1, ChunkTotal: 1, Text: strings.Repeat("雪\n\"\\|", 300)}, {Path: "main.go", Text: "package main\n"}}
	for budget := 500; budget < 1300; budget += 7 {
		auto, err := buildStructuredAgentPrompt(ScanResult{}, chunks, AgentPromptOptions{Task: "Review", MaxContextChars: budget}, "auto")
		if err != nil {
			t.Fatal(err)
		}
		if len(auto) > budget {
			t.Fatal("auto exceeds budget")
		}
		value := decodeStructuredPrompt(t, auto)
		coverage := func(v map[string]any) int {
			n := 0
			for _, row := range v["chunks"].([]any) {
				n += len(row.(map[string]any)["text"].(string))
			}
			return n
		}
		for _, format := range []string{"json", "gcf"} {
			forced, err := buildStructuredAgentPrompt(ScanResult{}, chunks, AgentPromptOptions{Task: "Review", MaxContextChars: budget}, format)
			if err != nil {
				t.Fatal(err)
			}
			if coverage(value) < coverage(decodeStructuredPrompt(t, forced)) {
				t.Fatalf("auto retained less source than %s at %d", format, budget)
			}
		}
	}
}

func TestAgentDryRunDoesNotContactOllama(t *testing.T) {
	t.Setenv("CLYDE_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "must not contact", 500) }))
	defer server.Close()
	for _, format := range []string{"auto", "json", "gcf", "text"} {
		var out bytes.Buffer
		if err := cmdAgent([]string{repo, "--dry-run", "--context-format", format, "--ollama-url", server.URL, "Review"}, strings.NewReader(""), &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "package main") || strings.HasPrefix(out.String(), "Clyde agent using") {
			t.Fatal("unexpected dry-run output")
		}
		if format != "text" {
			decodeStructuredPrompt(t, out.String())
		}
	}
	var out bytes.Buffer
	if err := cmdAgent([]string{repo, "--dry-run", "--ollama-url", "https://example.com", "Review"}, strings.NewReader(""), &out); err != nil {
		t.Fatal("dry-run should allow remote configured URL without approval", err)
	}
	decodeStructuredPrompt(t, out.String())
	if calls.Load() != 0 {
		t.Fatal("dry-run contacted Ollama")
	}
	if err := cmdAgent([]string{repo, "--dry-run", "Review"}, strings.NewReader(""), failingGCFWriter{}); err == nil {
		t.Fatal("dry-run writer error lost")
	}
}

func TestAutoReportCLI(t *testing.T) {
	t.Setenv("CLYDE_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []func([]string, io.Writer) error{cmdPreview, cmdScanReport} {
		var out bytes.Buffer
		if err := command([]string{repo, "--format", "auto"}, &out); err != nil {
			t.Fatal(err)
		}
		if _, err := gcf.DecodeGeneric(out.String()); err != nil && !json.Valid(out.Bytes()) {
			t.Fatal("auto report unreadable")
		}
		for _, args := range [][]string{{repo, "--format", "auto", "--json"}, {repo, "--format", "text", "--gcf"}, {repo, "--format", "invalid"}} {
			if err := command(args, io.Discard); err == nil {
				t.Fatal("expected format validation error")
			}
		}
	}
}

func TestGCFConversionReadError(t *testing.T) {
	if err := cmdGCF([]string{"encode"}, errorGCFReader{}, io.Discard); err == nil {
		t.Fatal("read failure lost")
	}
}

type errorGCFReader struct{}

func (errorGCFReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }
