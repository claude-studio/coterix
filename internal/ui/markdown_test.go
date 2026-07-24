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
	assertRenderedBackgroundPresent(t, diff, tokens.Diff.InsertLineBG)
	assertRenderedBackgroundPresent(t, diff, tokens.Diff.DeleteLineBG)
}

func TestGlamourStyleKeepsSyntaxAndDiffTokenMappings(t *testing.T) {
	tokens, err := loadColorTokens()
	if err != nil {
		t.Fatal(err)
	}
	chroma := glamourStyle(tokens).CodeBlock.Chroma
	if chroma == nil {
		t.Fatal("Glamour Chroma style is nil")
	}

	syntaxCases := []struct {
		name string
		got  *string
		want string
	}{
		{name: "text", got: chroma.Text.Color, want: tokens.Syntax.Text},
		{name: "error", got: chroma.Error.Color, want: tokens.Syntax.Error},
		{name: "comment", got: chroma.Comment.Color, want: tokens.Syntax.Comment},
		{name: "preprocessor", got: chroma.CommentPreproc.Color, want: tokens.Syntax.Preprocessor},
		{name: "keyword", got: chroma.Keyword.Color, want: tokens.Syntax.Keyword},
		{name: "keyword_reserved", got: chroma.KeywordReserved.Color, want: tokens.Syntax.KeywordReserved},
		{name: "keyword_namespace", got: chroma.KeywordNamespace.Color, want: tokens.Syntax.KeywordNamespace},
		{name: "type", got: chroma.KeywordType.Color, want: tokens.Syntax.Type},
		{name: "operator", got: chroma.Operator.Color, want: tokens.Syntax.Operator},
		{name: "punctuation", got: chroma.Punctuation.Color, want: tokens.Syntax.Punctuation},
		{name: "name", got: chroma.Name.Color, want: tokens.Syntax.Name},
		{name: "builtin", got: chroma.NameBuiltin.Color, want: tokens.Syntax.Builtin},
		{name: "tag", got: chroma.NameTag.Color, want: tokens.Syntax.Tag},
		{name: "attribute", got: chroma.NameAttribute.Color, want: tokens.Syntax.Attribute},
		{name: "class", got: chroma.NameClass.Color, want: tokens.Syntax.Class},
		{name: "decorator", got: chroma.NameDecorator.Color, want: tokens.Syntax.Decorator},
		{name: "function", got: chroma.NameFunction.Color, want: tokens.Syntax.Function},
		{name: "number", got: chroma.LiteralNumber.Color, want: tokens.Syntax.Number},
		{name: "string", got: chroma.LiteralString.Color, want: tokens.Syntax.String},
		{name: "string_escape", got: chroma.LiteralStringEscape.Color, want: tokens.Syntax.StringEscape},
	}
	for _, syntaxCase := range syntaxCases {
		t.Run(syntaxCase.name, func(t *testing.T) {
			if syntaxCase.got == nil || *syntaxCase.got != syntaxCase.want {
				t.Fatalf(
					"syntax mapping=%v, want %q",
					syntaxCase.got,
					syntaxCase.want,
				)
			}
		})
	}

	diffCases := []struct {
		name string
		got  *string
		want string
	}{
		{
			name: "insert_foreground",
			got:  chroma.GenericInserted.Color,
			want: tokens.Diff.InsertFG,
		},
		{
			name: "insert_line_background",
			got:  chroma.GenericInserted.BackgroundColor,
			want: tokens.Diff.InsertLineBG,
		},
		{
			name: "delete_foreground",
			got:  chroma.GenericDeleted.Color,
			want: tokens.Diff.DeleteFG,
		},
		{
			name: "delete_line_background",
			got:  chroma.GenericDeleted.BackgroundColor,
			want: tokens.Diff.DeleteLineBG,
		},
	}
	for _, diffCase := range diffCases {
		t.Run(diffCase.name, func(t *testing.T) {
			if diffCase.got == nil || *diffCase.got != diffCase.want {
				t.Fatalf(
					"diff mapping=%v, want %q",
					diffCase.got,
					diffCase.want,
				)
			}
		})
	}
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

func assertRenderedBackgroundPresent(t *testing.T, rendered string, token string) {
	t.Helper()
	screen := uv.NewScreenBuffer(max(1, ansi.StringWidth(rendered)), max(1, strings.Count(rendered, "\n")+1))
	uv.NewStyledString(rendered).Draw(screen, screen.Bounds())
	want := colorFromToken(token)
	for y := screen.Bounds().Min.Y; y < screen.Bounds().Max.Y; y++ {
		for x := screen.Bounds().Min.X; x < screen.Bounds().Max.X; x++ {
			cell := screen.CellAt(x, y)
			if cell != nil && sameColor(cell.Style.Bg, want) {
				return
			}
		}
	}
	t.Fatalf("rendered output did not use injected background token %q", token)
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
