package cli

import (
	"os"
	"strings"
	"testing"
)

// testdata/claude-stream-json.jsonl is a real capture from
// `claude -p --output-format stream-json --verbose` (2026-07-25), with
// machine-specific and account fields removed. It is the ground truth for the
// noise ratio the decoder exists to fix: 10 lines for a one-word answer, 7 of them
// bookkeeping.
func TestDecodeStreamDropsBookkeepingFromTheRealCapture(t *testing.T) {
	raw, err := os.ReadFile("testdata/claude-stream-json.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 10 {
		t.Fatalf("fixture has %d lines, expected the captured 10", len(lines))
	}

	kept := make([]StreamLine, 0, len(lines))
	for _, line := range lines {
		if decoded, ok := DecodeStreamLine(line); ok {
			kept = append(kept, decoded)
		}
	}
	if len(kept) != 2 {
		t.Fatalf(
			"kept %d of %d lines, want 2 (the assistant text and the result):\n%#v",
			len(kept),
			len(lines),
			kept,
		)
	}
	if kept[0].Text != "OK" || kept[0].Failed {
		t.Fatalf("first kept line = %#v, want the assistant text", kept[0])
	}
	if kept[1].Text != "OK" || kept[1].Failed {
		t.Fatalf("second kept line = %#v, want the successful result", kept[1])
	}

	// Nothing from the dropped envelopes may leak through.
	for _, decoded := range kept {
		for _, marker := range []string{"hook", "rate_limit", "session"} {
			if strings.Contains(strings.ToLower(decoded.Text), marker) {
				t.Fatalf("bookkeeping leaked into %q", decoded.Text)
			}
		}
	}
}

func TestDecodeStreamLineClassifiesEachEnvelope(t *testing.T) {
	for _, test := range []struct {
		name   string
		line   string
		want   string
		failed bool
		drop   bool
	}{
		{
			name: "plain text passes through untouched",
			line: "  building internal/ui...  ",
			want: "  building internal/ui...  ",
		},
		{
			name: "malformed json is still shown",
			line: `{"type":"assistant",`,
			want: `{"type":"assistant",`,
		},
		{
			name: "blank line is dropped",
			line: "   ",
			drop: true,
		},
		{
			name: "unknown json type is dropped, not passed through",
			line: `{"type":"some_future_event","payload":{"a":1}}`,
			drop: true,
		},
		{
			name: "thinking blocks are dropped",
			line: `{"type":"assistant","message":{"content":[` +
				`{"type":"thinking","thinking":"let me consider the layout budget"}]}}`,
			drop: true,
		},
		{
			name: "tool_use names the tool and its target",
			line: `{"type":"assistant","message":{"content":[{"type":"tool_use",` +
				`"name":"Edit","input":{"file_path":"internal/ui/view.go"}}]}}`,
			want: "⚙ Edit(internal/ui/view.go)",
		},
		{
			name: "tool_use without a recognised argument still names the tool",
			line: `{"type":"assistant","message":{"content":[{"type":"tool_use",` +
				`"name":"TodoWrite","input":{"todos":[]}}]}}`,
			want: "⚙ TodoWrite",
		},
		{
			name: "text and tool_use in one message share a row",
			line: `{"type":"assistant","message":{"content":[` +
				`{"type":"text","text":"Reading the layout"},` +
				`{"type":"tool_use","name":"Read","input":{"path":"view.go"}}]}}`,
			want: "Reading the layout · ⚙ Read(view.go)",
		},
		{
			name: "tool_result is summarised",
			line: `{"type":"user","message":{"content":[{"type":"tool_result",` +
				`"content":"ok\nmore detail"}]}}`,
			want: "↩ ok",
		},
		{
			// The icon must key off is_error, not off subtype: the CLI reports
			// subtype "success" with is_error true (measured T13a-1).
			name:   "success subtype with is_error is a failure",
			line:   `{"type":"result","subtype":"success","is_error":true,"result":"boom"}`,
			want:   "boom",
			failed: true,
		},
		{
			name: "result falls back to the error field",
			line: `{"type":"result","subtype":"error_during_execution",` +
				`"is_error":true,"error":"authentication_failed"}`,
			want:   "authentication_failed",
			failed: true,
		},
		{
			name: "multi-line text collapses to its first row",
			line: `{"type":"result","subtype":"success","result":"done\nand more"}`,
			want: "done",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			decoded, ok := DecodeStreamLine(test.line)
			if ok == test.drop {
				t.Fatalf("ok=%v want drop=%v (decoded=%#v)", ok, test.drop, decoded)
			}
			if test.drop {
				return
			}
			if decoded.Text != test.want {
				t.Fatalf("text=%q want %q", decoded.Text, test.want)
			}
			if decoded.Failed != test.failed {
				t.Fatalf("failed=%v want %v", decoded.Failed, test.failed)
			}
		})
	}
}

// A very long line must not become a very long row: the tail has one row per line,
// and the untouched original stays in the run's logs/ files.
func TestDecodeStreamLineKeepsRowsToOneRow(t *testing.T) {
	long := strings.Repeat("x", 500)
	decoded, ok := DecodeStreamLine(
		`{"type":"result","subtype":"success","result":"` + long + `"}`,
	)
	if !ok {
		t.Fatal("a long result was dropped")
	}
	if len(decoded.Text) > 210 || !strings.HasSuffix(decoded.Text, "…") {
		t.Fatalf("row is %d bytes and does not mark the cut: %q",
			len(decoded.Text), decoded.Text)
	}
}
