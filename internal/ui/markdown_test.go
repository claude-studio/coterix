package ui

import (
	"image/color"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

func TestMarkdownRendererRendersPlanVerdictAndDiff(t *testing.T) {
	tokens, err := loadColorTokens()
	if err != nil {
		t.Fatal(err)
	}
	renderer := newMarkdownRenderer(tokens)

	plan, err := renderer.renderPlan("# Plan\n\n- one\n- two\n", 60)
	if err != nil {
		t.Fatal(err)
	}
	if plain := ansi.Strip(plan); !strings.Contains(plain, "Plan") ||
		!strings.Contains(plain, "one") {
		t.Fatalf("plan markdown not rendered: %q", plain)
	}

	verdict, err := renderer.renderVerdict("## Verdict\n\n**clean**\n", 60)
	if err != nil {
		t.Fatal(err)
	}
	if plain := ansi.Strip(verdict); !strings.Contains(plain, "Verdict") ||
		!strings.Contains(plain, "clean") {
		t.Fatalf("verdict markdown not rendered: %q", plain)
	}

	diff, err := renderer.renderDiff("@@ section @@\n-old\n+new\n", 60)
	if err != nil {
		t.Fatal(err)
	}
	plainDiff := ansi.Strip(diff)
	if !strings.Contains(plainDiff, "-old") || !strings.Contains(plainDiff, "+new") {
		t.Fatalf("diff markdown not rendered: %q", plainDiff)
	}
	assertRenderedColorPresent(t, diff, tokens.Diff.InsertFG)
	assertRenderedColorPresent(t, diff, tokens.Diff.DeleteFG)
}

func TestRenderMarkdownCombinesArtifacts(t *testing.T) {
	currentTheme, err := loadTheme()
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := renderMarkdown(
		currentTheme,
		60,
		[]markdownArtifact{
			{
				Title:   "Plan",
				Content: "- first task\n",
			},
			{
				Title:    "Diff",
				Content:  "-old\n+new\n",
				Language: "diff",
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	plain := ansi.Strip(rendered)
	for _, expected := range []string{"Plan", "first task", "Diff", "-old", "+new"} {
		if !strings.Contains(plain, expected) {
			t.Fatalf("combined markdown lacks %q: %q", expected, plain)
		}
	}
}

func TestComposeUVCompositesStyledStrings(t *testing.T) {
	tokens, err := loadColorTokens()
	if err != nil {
		t.Fatal(err)
	}
	styles := newDashboardStyles(tokens)
	composed := composeUV(
		6,
		1,
		[]uvRegion{{
			Content: styles.Primary.Render("L"),
			X:       0,
			Y:       0,
			Width:   3,
			Height:  1,
		}, {
			Content: styles.Secondary.Render("R"),
			X:       3,
			Y:       0,
			Width:   3,
			Height:  1,
		}},
	)

	screen := uv.NewScreenBuffer(6, 1)
	uv.NewStyledString(composed).Draw(screen, screen.Bounds())
	left := screen.CellAt(0, 0)
	right := screen.CellAt(3, 0)
	if left.Content != "L" || right.Content != "R" {
		t.Fatalf("UV composition content mismatch: left=%q right=%q", left.Content, right.Content)
	}
	assertSameColor(t, left.Style.Fg, colorFromToken(tokens.Theme.Primary))
	assertSameColor(t, right.Style.Fg, colorFromToken(tokens.Theme.Secondary))
}

func TestComposeUVRejectsEmptyDimensions(t *testing.T) {
	if got := composeUV(0, 1, nil); got != "" {
		t.Fatalf("zero-width composition returned %q", got)
	}
	if got := composeUV(1, 0, nil); got != "" {
		t.Fatalf("zero-height composition returned %q", got)
	}
}

func assertRenderedColorPresent(t *testing.T, rendered string, token string) {
	t.Helper()
	screen := uv.NewScreenBuffer(max(1, ansi.StringWidth(rendered)), max(1, strings.Count(rendered, "\n")+1))
	uv.NewStyledString(rendered).Draw(screen, screen.Bounds())
	want := colorFromToken(token)
	for y := screen.Bounds().Min.Y; y < screen.Bounds().Max.Y; y++ {
		for x := screen.Bounds().Min.X; x < screen.Bounds().Max.X; x++ {
			cell := screen.CellAt(x, y)
			if cell != nil && sameColor(cell.Style.Fg, want) {
				return
			}
		}
	}
	t.Fatalf("rendered output did not use injected token %q", token)
}

func colorFromToken(token string) color.Color {
	return lipgloss.Color(token)
}

func sameColor(left color.Color, right color.Color) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return rgba(left) == rgba(right)
}
