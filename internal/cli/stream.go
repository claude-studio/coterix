package cli

import (
	"encoding/json"
	"strings"
)

// StreamLine is one displayable row decoded from a CLI's JSONL stream.
type StreamLine struct {
	Text string
	// Failed keys off the payload's own `is_error`, not off which stream the bytes
	// arrived on: claude writes ordinary progress to stdout and codex writes it to
	// stderr, so the stream tells you nothing about severity (T13 R5 · T13a-2).
	Failed bool
}

// DecodeStreamLine turns one line of CLI output into what the activity tail should
// show, or reports false to drop it.
//
// The default for parsed-but-undisplayable JSON is **drop**, which is the reverse of
// the original plan. Measured against the real CLI (`claude -p --output-format
// stream-json --verbose`, 2026-07-25): a trivial one-turn request emitted 10 lines
// and 7 of them were `system/*` or `rate_limit_event` bookkeeping. Passing those
// through would bury the one line that says what the model is doing.
//
// A line that is not JSON at all is passed through unchanged — plain-text mode and
// codex both rely on that, and dropping unrecognised output would lose information.
func DecodeStreamLine(line string) (StreamLine, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return StreamLine{}, false
	}
	if !strings.HasPrefix(trimmed, "{") {
		return StreamLine{Text: line}, true
	}
	var envelope streamEnvelope
	if err := json.Unmarshal([]byte(trimmed), &envelope); err != nil {
		// Malformed JSON is still output the operator may need to see.
		return StreamLine{Text: line}, true
	}

	switch envelope.Type {
	case "system", "rate_limit_event":
		// Session bookkeeping and quota telemetry: never what the step is doing.
		return StreamLine{}, false
	case "assistant", "user":
		if text := summarizeContent(envelope.Message.Content); text != "" {
			return StreamLine{Text: text}, true
		}
		return StreamLine{}, false
	case "result":
		text := strings.TrimSpace(envelope.Result)
		if text == "" {
			text = strings.TrimSpace(envelope.Error)
		}
		if text == "" {
			return StreamLine{}, false
		}
		return StreamLine{Text: firstLine(text), Failed: envelope.IsError}, true
	}
	return StreamLine{}, false
}

type streamEnvelope struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
	Error   string `json:"error"`
	Message struct {
		Content []streamBlock `json:"content"`
	} `json:"message"`
}

type streamBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

// summarizeContent renders a message's blocks as one row. `thinking` is dropped by
// decision: it is long, it is not what the step is doing, and it would swamp a
// five-line tail.
func summarizeContent(blocks []streamBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if text := strings.TrimSpace(block.Text); text != "" {
				parts = append(parts, firstLine(text))
			}
		case "tool_use":
			parts = append(parts, "⚙ "+block.Name+toolArgumentHint(block.Input))
		case "tool_result":
			if text := strings.TrimSpace(rawText(block.Content)); text != "" {
				parts = append(parts, "↩ "+firstLine(text))
			}
		}
	}
	return strings.Join(parts, " · ")
}

// toolArgumentHint names the tool's most identifying argument — the path or command
// — so two calls to the same tool are distinguishable in one row.
func toolArgumentHint(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var fields map[string]any
	if err := json.Unmarshal(input, &fields); err != nil {
		return ""
	}
	for _, key := range []string{"file_path", "path", "command", "pattern", "query"} {
		if value, ok := fields[key].(string); ok &&
			strings.TrimSpace(value) != "" {
			return "(" + firstLine(strings.TrimSpace(value)) + ")"
		}
	}
	return ""
}

// rawText accepts either a JSON string or a nested block array, which is how
// tool_result content arrives depending on the tool.
func rawText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var blocks []streamBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return summarizeContent(blocks)
	}
	return ""
}

// firstLine keeps a row a row. The full output is still in the run's logs/ files, so
// nothing is lost by summarising here (spec: 원문 보존).
func firstLine(text string) string {
	if index := strings.IndexAny(text, "\r\n"); index >= 0 {
		text = text[:index]
	}
	const maxCells = 200
	if len(text) > maxCells {
		return text[:maxCells] + "…"
	}
	return text
}

// StreamArgs are the flags that make a CLI report progress line by line. Without
// them a multi-minute step prints nothing until it finishes, which is what made the
// dashboard look frozen (live-smoke finding, 2026-07-25).
func StreamArgs(cliName string) []string {
	if strings.ToLower(strings.TrimSpace(cliName)) == "claude" {
		return []string{"--output-format", "stream-json", "--verbose"}
	}
	return nil
}
