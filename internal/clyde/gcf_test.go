package clyde

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	gcf "github.com/blackwell-systems/gcf-go"
)

func normalizedJSON(t *testing.T, value any) any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var result any
	if err := dec.Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestGCFReportsRoundTrip(t *testing.T) {
	result := ScanResult{Repo: "/tmp/雪", Files: []FileRecord{{Rel: "a|b\n雪.go", Size: math.MaxInt64, SHA256: "00123"}}, Skips: []SkipRecord{{Path: "true", Reason: "quote\" slash\\\t"}}}
	flags := scanFlags{maxFileBytes: 250000, maxChunkChars: 18000, include: []string{"*.go"}}
	for _, value := range []any{previewData(result, 2, flags), buildScanReport(result, 2, flags, 1), previewData(ScanResult{}, 0, scanFlags{})} {
		wire, err := encodeGCF(value)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := gcf.DecodeGeneric(wire)
		if err != nil {
			t.Fatalf("%v\n%s", err, wire)
		}
		if !reflect.DeepEqual(normalizedJSON(t, decoded), normalizedJSON(t, value)) {
			t.Fatalf("round-trip mismatch:\n%s", wire)
		}
		again, err := encodeGCF(value)
		if err != nil || again != wire {
			t.Fatal("non-deterministic encoding", err)
		}
	}
}

func decodeAgentGCF(t *testing.T, prompt string) map[string]any {
	t.Helper()
	start := strings.Index(prompt, "GCF profile=generic\n")
	if start < 0 {
		t.Fatal("missing GCF header")
	}
	value, err := gcf.DecodeGeneric(prompt[start:])
	if err != nil {
		t.Fatalf("%v\n%s", err, prompt)
	}
	return normalizedJSON(t, value).(map[string]any)
}

func TestGCFAgentPromptBudgetAndPriority(t *testing.T) {
	result := ScanResult{Repo: "/tmp/repo", Files: []FileRecord{{Rel: "z.go"}, {Rel: "README.md"}}}
	text := strings.Repeat("雪\n\"\\|", 500)
	chunks := []ChunkRecord{{Path: "z.go", ChunkIndex: 1, ChunkTotal: 1, Text: "later"}, {Path: "README.md", ChunkIndex: 1, ChunkTotal: 1, Text: text}}
	full, err := buildGCFAgentPrompt(result, chunks, AgentPromptOptions{Task: "Review", MaxContextChars: 16000})
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeAgentGCF(t, full)
	if decoded["context_truncated"] != false {
		t.Fatal("unexpected full-context truncation")
	}
	rows := decoded["chunks"].([]any)
	if rows[0].(map[string]any)["path"] != "README.md" || rows[0].(map[string]any)["text"] != text {
		t.Fatal("priority or source preservation failed")
	}
	for budget := 500; budget < 1500; budget++ {
		prompt, err := buildGCFAgentPrompt(result, chunks, AgentPromptOptions{Task: "Review", MaxContextChars: budget})
		if err != nil {
			t.Fatal(err)
		}
		if len(prompt) > budget || !utf8.ValidString(prompt) {
			t.Fatalf("invalid prompt at budget %d", budget)
		}
		decoded := decodeAgentGCF(t, prompt)
		if decoded["context_truncated"] != true {
			t.Fatal("missing truncation indicator")
		}
		rows := decoded["chunks"].([]any)
		if len(rows) > 0 {
			row := rows[0].(map[string]any)
			if row["path"] != "README.md" || row["text_truncated"] != true || !strings.HasPrefix(text, row["text"].(string)) {
				t.Fatal("invalid truncated row")
			}
		}
	}
	if _, err := buildGCFAgentPrompt(result, chunks, AgentPromptOptions{MaxContextChars: 10}); err == nil {
		t.Fatal("expected insufficient budget error")
	}
	// Exact boundary includes the longer false token in the completed payload.
	exact, err := buildGCFAgentPrompt(result, chunks, AgentPromptOptions{Task: "Review", MaxContextChars: len(full)})
	if err != nil || len(exact) > len(full) {
		t.Fatal("exact budget failed", err)
	}
}

func TestGCFScanCommands(t *testing.T) {
	t.Setenv("CLYDE_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, command := range map[string]func([]string, io.Writer) error{"preview": cmdPreview, "scan-report": cmdScanReport} {
		t.Run(name, func(t *testing.T) {
			var jsonOut, gcfOut bytes.Buffer
			if err := command([]string{repo, "--json"}, &jsonOut); err != nil {
				t.Fatal(err)
			}
			if err := command([]string{repo, "--gcf"}, &gcfOut); err != nil {
				t.Fatal(err)
			}
			var expected any
			dec := json.NewDecoder(&jsonOut)
			dec.UseNumber()
			if err := dec.Decode(&expected); err != nil {
				t.Fatal(err)
			}
			decoded, err := gcf.DecodeGeneric(gcfOut.String())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(normalizedJSON(t, expected), normalizedJSON(t, decoded)) {
				t.Fatal("CLI output models differ")
			}
			if err := command([]string{"--gcf", "--json", repo}, &gcfOut); err == nil {
				t.Fatal("expected mutually exclusive flags error")
			}
		})
	}
}

type failingGCFWriter struct{}

func (failingGCFWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }
func TestGCFWriterFailure(t *testing.T) {
	if err := printStructured(failingGCFWriter{}, map[string]any{"a": 1}, true); err == nil {
		t.Fatal("writer failure lost")
	}
}

func TestGCFAgentOllamaTransport(t *testing.T) {
	t.Setenv("CLYDE_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	repo := t.TempDir()
	source := "package main\n// 雪 | quote\"\n"
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{"name": "qwen2.5-coder:7b"}}})
		case "/api/generate":
			if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"response": "ok"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	var out bytes.Buffer
	if err := cmdAgent([]string{repo, "--context-format", "gcf", "--ollama-url", server.URL, "--no-stream", "Review"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if _, exists := received["format"]; exists {
		t.Fatal("GCF must be prompt text, not Ollama's structured output format")
	}
	prompt, ok := received["prompt"].(string)
	if !ok {
		t.Fatal("missing prompt")
	}
	decoded := decodeAgentGCF(t, prompt)
	rows := decoded["chunks"].([]any)
	if len(rows) != 1 || !strings.Contains(rows[0].(map[string]any)["text"].(string), source) {
		t.Fatal("source missing from transport")
	}
	if err := cmdAgent([]string{repo, "--context-format", "bogus"}, strings.NewReader(""), &out); err == nil {
		t.Fatal("invalid format accepted")
	}
}

func TestGCFAgentEmptyContextBudget(t *testing.T) {
	full, err := buildGCFAgentPrompt(ScanResult{}, nil, AgentPromptOptions{})
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeAgentGCF(t, full)
	if decoded["context_truncated"] != false || len(decoded["chunks"].([]any)) != 0 {
		t.Fatal("invalid empty context")
	}
	exact, err := buildGCFAgentPrompt(ScanResult{}, nil, AgentPromptOptions{MaxContextChars: len(full)})
	if err != nil || len(exact) != len(full) {
		t.Fatal("empty exact budget failed", err)
	}
	if _, err := buildGCFAgentPrompt(ScanResult{}, nil, AgentPromptOptions{MaxContextChars: len(full) - 1}); err == nil {
		t.Fatal("empty context exceeds budget")
	}
}

func TestGCFHelp(t *testing.T) {
	for name, flagName := range map[string]string{"preview": "gcf", "scan-report": "gcf", "agent": "context-format"} {
		var out bytes.Buffer
		if err := cmdHelp([]string{name}, strings.NewReader(""), &out, &out); err != nil && !errors.Is(err, flag.ErrHelp) {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), flagName) {
			t.Fatalf("%s help missing %s", name, flagName)
		}
	}
}
