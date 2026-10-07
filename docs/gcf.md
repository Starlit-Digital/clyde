# GCF support

Research checked 2026-10-05.

clyde uses Graph Compact Format (GCF) from Blackwell Systems, an UTF-8,
line-oriented format for structured data. Its generic profile represents the
JSON data model; tabular arrays declare fields once and use positional rows.
The graph profile represents symbols and edges. clyde uses the generic profile
because its scan reports and source chunks are records, not a code graph.

Implementation: pinned `github.com/blackwell-systems/gcf-go v1.8.0`, which has no
transitive runtime dependencies. clyde marshals through its existing JSON field
names and uses the SDK's ordered JSON parser and checked encoder to preserve
integer precision, nulls, array ordering, escaping, and deterministic output.
Nested-field flattening is disabled, following the SDK's guidance for open-weight
models. GCF is an optional representation; token savings for clyde's source text
have not been measured. Escaping multiline code can add overhead.

## Automatic defaults and commands

The agent defaults to `--context-format auto`. It serializes the same context
records as compact JSON and GCF, then picks the smaller complete representation
in UTF-8 bytes, including its final newline. JSON wins ties. If a value cannot be
represented in GCF's numeric domain, auto output falls back to JSON. This is a
size heuristic, not a model-quality or token-count guarantee.

```sh
clyde agent . 'Review this repository'
clyde agent . --context-format gcf 'Review this repository'
clyde agent . --context-format json 'Review this repository'
clyde agent . --context-format text 'Review this repository'
clyde agent . --dry-run 'Review this repository' > prompt.txt
clyde scan-report . --format auto --top 20
clyde preview . --gcf
```

`agent --dry-run` prepares the same prompt as a real run, writes only that prompt
to stdout, and does not contact Ollama or list models. It can inspect prompts
with a configured remote URL without authorizing a transfer. Actual agent runs
retain the remote-source approval guard.

Reports default to human-readable text. `--format auto` chooses the smaller
compact JSON/GCF report. Explicit `--json` and `--format json` retain pretty JSON;
`--gcf` and `--format gcf` force GCF. `--format`, `--gcf`, and `--json` are mutually
exclusive. Use a forced format for scripts that require a stable output syntax.
Both report formats contain the same fields.

The agent retains its instructions and task as prose, followed by a structured
payload containing repository metadata and prioritized source chunks. Its
`--max-context-chars` budget is measured in UTF-8 bytes, as in clyde's existing
prompt code. The complete JSON/GCF-mode prompt fits the budget. Source prefixes
are shortened before encoding; wire syntax is never cut. `context_truncated`
marks omitted context and `text_truncated` marks a shortened source chunk.
A budget too small for instructions, the task, and metadata returns an error.
The original text mode remains available explicitly.

## Local conversion and size comparison

```sh
clyde preview . --json | clyde gcf encode > report.gcf
clyde gcf decode report.gcf > report.json
clyde gcf stats report.json
clyde gcf stats report.json --json
```

`encode` and `stats` read JSON; `decode` reads GCF. A missing filename or `-` reads
stdin. These commands run locally, independently of clyde's configuration, and
write to stdout. File inputs must be regular files; input is limited to 1 MiB.
JSON conversion rejects duplicate object keys, trailing values/garbage, invalid
UTF-8, nesting beyond 256 levels, and numbers outside GCF's numeric domain.

Stats report original input bytes, canonical compact JSON bytes, GCF bytes, and
savings relative to compact JSON. Negative savings indicate that GCF is larger.
The GCF count includes its required header and final newline; compact JSON is
counted without a final newline. Auto selection counts final newlines on both
representations. No model tokenizer is used.

Tests cover conversion and report round trips, exact signed 64-bit integers,
Unicode and escaping, malformed and oversized input, file/stdin operation,
negative savings, auto selection and JSON fallback, prompt-budget coverage,
writer failures, and dry-run network avoidance.

## Ollama compatibility

Ollama's generate API accepts a string `prompt`, so clyde can send GCF inside
that text using the existing JSON HTTP request. GCF is not an Ollama transport
format or a documented structured-output mode: the `format` option supports
`json` or a JSON schema. clyde does not set `format` to `gcf`.

The acceptance of text establishes transport compatibility, not model
comprehension. Understanding depends on the selected model. Tests verify that
the agent sends a decodable GCF prompt through a mock Ollama endpoint with the
source bytes preserved. No live model comprehension benchmark was performed.

## Google NotebookLM compatibility

Google documents copied/pasted text and TXT/Markdown sources, among other file
types. `.gcf` is not listed as a supported source extension. The practical route
for an exported GCF report is pasted text or a `.txt` file. Accepting that text
does not establish GCF-aware parsing, retrieval, or reliable field interpretation.
No live NotebookLM import or comprehension test was performed.

clyde's current MCP and `nlm` backends submit source content as text. These features
provide structured reports, local conversion, and automatic agent context; bundle persistence, digest approval,
and sync payloads retain their existing representation. A future GCF sync option
would need to bind the exact encoded upload bytes to bundle approval and receipts.

## Sources

- [GCF specification](https://github.com/blackwell-systems/gcf/blob/main/SPEC.md)
- [Official Go SDK and model guidance](https://github.com/blackwell-systems/gcf-go)
- [Ollama generate API](https://docs.ollama.com/api/generate)
- [Ollama API schema](https://github.com/ollama/ollama/blob/main/docs/openapi.yaml)
- [Google's supported notebook source types](https://support.google.com/gemininotebook/answer/16215270)
