package ui

import (
	"encoding/json"
	"image/color"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/ridenow/coterix/design"
)

func TestEmbeddedColorTokensAreSoleSource(t *testing.T) {
	onDisk, err := os.ReadFile(filepath.Join("..", "..", "design", "coterix-color-tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != design.CoterixColorTokensJSON() {
		t.Fatal("embedded color tokens differ from the versioned JSON source")
	}

	tokens, err := loadColorTokens()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(tokens)
	if err != nil {
		t.Fatal(err)
	}

	var sourceSections map[string]any
	if err := json.Unmarshal(onDisk, &sourceSections); err != nil {
		t.Fatal(err)
	}
	var mappedSections map[string]any
	if err := json.Unmarshal(encoded, &mappedSections); err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{
		"palette",
		"gradient",
		"theme",
		"component",
		"status",
		"diff",
		"syntax",
		"ansi",
	} {
		if !reflect.DeepEqual(sourceSections[section], mappedSections[section]) {
			t.Fatalf("color-token section %q lost or changed a mapping", section)
		}
	}
}

func TestColorTokensRejectMissingRequiredColor(t *testing.T) {
	tokens, err := loadColorTokens()
	if err != nil {
		t.Fatal(err)
	}
	tokens.Theme.Primary = ""
	if err := validateColorTokens(tokens); err == nil {
		t.Fatal("missing semantic color was accepted")
	}
}

func TestColorTokensRejectMissingPaletteColorAtLoad(t *testing.T) {
	for _, rampName := range []string{
		"ink",
		"codex",
		"claude",
		"violet",
		"success",
		"warning",
		"danger",
		"cyan",
	} {
		t.Run(rampName, func(t *testing.T) {
			source := mutatePaletteRamp(t, rampName, func(ramp map[string]any) {
				delete(ramp, "400")
			})
			_, err := parseColorTokens(source)
			if err == nil {
				t.Fatal("missing palette color was accepted")
			}
			if !strings.Contains(err.Error(), "palette."+rampName+".400") {
				t.Fatalf("missing palette color error lacks token path: %v", err)
			}
		})
	}
}

func TestColorTokensRejectPaletteColorTypoAtLoad(t *testing.T) {
	for _, rampName := range []string{
		"ink",
		"codex",
		"claude",
		"violet",
		"success",
		"warning",
		"danger",
		"cyan",
	} {
		t.Run(rampName, func(t *testing.T) {
			source := mutatePaletteRamp(t, rampName, func(ramp map[string]any) {
				ramp["fourHundred"] = ramp["400"]
				delete(ramp, "400")
			})
			_, err := parseColorTokens(source)
			if err == nil {
				t.Fatal("misspelled palette color was accepted")
			}
			if !strings.Contains(err.Error(), "fourHundred") {
				t.Fatalf("misspelled palette color error lacks unknown key: %v", err)
			}
		})
	}
}

func TestDashboardStylesUseInjectedTokens(t *testing.T) {
	tokens, err := loadColorTokens()
	if err != nil {
		t.Fatal(err)
	}
	styles := newDashboardStyles(tokens)

	cell := firstStyledCell(t, styles.Primary.Render("P"))
	assertSameColor(t, cell.Style.Fg, lipgloss.Color(tokens.Theme.Primary))

	cell = firstStyledCell(t, styles.DiffInsert.Render("I"))
	assertSameColor(t, cell.Style.Fg, lipgloss.Color(tokens.Diff.InsertFG))
	assertSameColor(t, cell.Style.Bg, lipgloss.Color(tokens.Diff.InsertLineBG))

	cell = firstStyledCell(t, styles.Claude.Render("C"))
	assertSameColor(t, cell.Style.Fg, lipgloss.Color(tokens.Palette.Claude.Shade400))

	cell = firstStyledCell(t, styles.Codex.Render("C"))
	assertSameColor(t, cell.Style.Fg, lipgloss.Color(tokens.Palette.Codex.Shade400))

	cell = firstStyledCell(t, styles.Info.Render("I"))
	assertSameColor(t, cell.Style.Fg, lipgloss.Color(tokens.Status.Info.FG))
	assertSameColor(t, cell.Style.Bg, lipgloss.Color(tokens.Status.Info.BG))

	cell = firstStyledCell(t, styles.Hint.Render("H"))
	assertSameColor(t, cell.Style.Fg, lipgloss.Color(tokens.Theme.FGSubtle))

	cell = firstStyledCell(t, styles.Separator.Render("─"))
	assertSameColor(t, cell.Style.Fg, lipgloss.Color(tokens.Theme.Separator))

	phaseStyles := []struct {
		name  string
		style lipgloss.Style
		color string
	}{
		{name: "info", style: styles.PhaseInfo, color: tokens.Status.Info.FG},
		{name: "warning", style: styles.PhaseWarning, color: tokens.Status.Warning.FG},
		{name: "success", style: styles.PhaseSuccess, color: tokens.Status.Success.FG},
		{name: "error", style: styles.PhaseError, color: tokens.Status.Error.FG},
	}
	for _, phase := range phaseStyles {
		t.Run("phase_"+phase.name, func(t *testing.T) {
			cell := firstStyledCell(t, phase.style.Render("●"))
			assertSameColor(t, cell.Style.Fg, lipgloss.Color(phase.color))
			if _, ok := phase.style.GetBackground().(lipgloss.NoColor); !ok {
				t.Fatalf("%s phase dot has a background", phase.name)
			}
		})
	}
}

func TestWorkingSpinnerUsesInjectedGradientFrames(t *testing.T) {
	tokens, err := loadColorTokens()
	if err != nil {
		t.Fatal(err)
	}

	working := workingSpinner(tokens)
	if working.FPS < 200*time.Millisecond || working.FPS > 250*time.Millisecond {
		t.Fatalf("working spinner FPS=%s, want 200-250ms", working.FPS)
	}
	if len(working.Frames) != len(spinner.Points.Frames) {
		t.Fatalf(
			"working spinner has %d frames, want %d",
			len(working.Frames),
			len(spinner.Points.Frames),
		)
	}

	renderedColors := make([]color.Color, 0, len(working.Frames))
	for index, frame := range working.Frames {
		if plain := ansi.Strip(frame); plain != spinner.Points.Frames[index] {
			t.Fatalf(
				"working frame %d text=%q, want %q",
				index,
				plain,
				spinner.Points.Frames[index],
			)
		}
		cell := firstStyledCell(t, frame)
		assertSameColor(
			t,
			cell.Style.Bg,
			lipgloss.Color(tokens.Status.Busy.BG),
		)
		renderedColors = append(renderedColors, cell.Style.Fg)
	}
	assertSameColor(
		t,
		renderedColors[0],
		lipgloss.Color(tokens.Component.WorkingGradientFrom),
	)
	assertSameColor(
		t,
		renderedColors[len(renderedColors)-1],
		lipgloss.Color(tokens.Component.WorkingGradientTo),
	)
	for index := 1; index < len(renderedColors); index++ {
		if reflect.DeepEqual(
			rgba(renderedColors[index-1]),
			rgba(renderedColors[index]),
		) {
			t.Fatalf("working frames %d and %d have the same color", index-1, index)
		}
	}
}

func TestANSI16RemapPreservesOtherSGR(t *testing.T) {
	tokens, err := loadColorTokens()
	if err != nil {
		t.Fatal(err)
	}
	input := "\x1b[1;31;44;4mX\x1b[38;5;31mY\x1b[48;2;31;44;91mZ\x1b[0m"
	remapped := remapANSI16(input, tokens.ANSI)
	if !strings.Contains(remapped, "\x1b[38;5;31m") ||
		!strings.Contains(remapped, "\x1b[48;2;31;44;91m") {
		t.Fatalf("extended SGR colors changed: %q", remapped)
	}

	screen := uv.NewScreenBuffer(3, 1)
	uv.NewStyledString(remapped).Draw(screen, screen.Bounds())
	first := screen.CellAt(0, 0)
	assertSameColor(t, first.Style.Fg, lipgloss.Color(tokens.ANSI.Red))
	assertSameColor(t, first.Style.Bg, lipgloss.Color(tokens.ANSI.Blue))
	if first.Style.Attrs&uv.AttrBold == 0 {
		t.Fatal("bold SGR was not preserved")
	}
	if first.Style.Underline != uv.UnderlineSingle {
		t.Fatal("underline SGR was not preserved")
	}

	second := screen.CellAt(1, 0)
	indexed, ok := second.Style.Fg.(ansi.IndexedColor)
	if !ok || indexed != ansi.IndexedColor(31) {
		t.Fatalf("unrelated ANSI-256 color changed: %#v", second.Style.Fg)
	}
	third := screen.CellAt(2, 0)
	red, green, blue, _ := third.Style.Bg.RGBA()
	if red>>8 != 31 || green>>8 != 44 || blue>>8 != 91 {
		t.Fatalf("unrelated true-color SGR changed: %#v", third.Style.Bg)
	}
}

func TestUIProductionGoHasNoLiteralHexColors(t *testing.T) {
	literalHex := regexp.MustCompile(`#[[:xdigit:]]{6}`)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		content, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if match := literalHex.Find(content); match != nil {
			t.Fatalf("%s contains hard-coded color %q", name, match)
		}
	}
}

func firstStyledCell(t *testing.T, value string) *uv.Cell {
	t.Helper()
	width := max(1, lipgloss.Width(value))
	screen := uv.NewScreenBuffer(width, 1)
	uv.NewStyledString(value).Draw(screen, screen.Bounds())
	cell := screen.CellAt(0, 0)
	if cell == nil {
		t.Fatal("styled output had no first cell")
	}
	return cell
}

func assertSameColor(t *testing.T, got color.Color, want color.Color) {
	t.Helper()
	if got == nil || want == nil {
		t.Fatalf("nil color: got=%v want=%v", got, want)
	}
	if !reflect.DeepEqual(rgba(got), rgba(want)) {
		t.Fatalf("color mismatch: got=%v want=%v", rgba(got), rgba(want))
	}
}

func rgba(value color.Color) [4]uint32 {
	red, green, blue, alpha := value.RGBA()
	return [4]uint32{red, green, blue, alpha}
}

func mutatePaletteRamp(
	t *testing.T,
	name string,
	mutate func(map[string]any),
) []byte {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(
		[]byte(design.CoterixColorTokensJSON()),
		&document,
	); err != nil {
		t.Fatal(err)
	}
	palette, ok := document["palette"].(map[string]any)
	if !ok {
		t.Fatal("color-token palette is not an object")
	}
	ramp, ok := palette[name].(map[string]any)
	if !ok {
		t.Fatalf("color-token palette ramp %q is not an object", name)
	}
	mutate(ramp)
	source, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func TestANSI16RemapLeavesPlainTextAndOtherCSI(t *testing.T) {
	input := "plain\x1b[2Ktext"
	if got := remapANSI16(input, ansiTokens{}); got != input {
		t.Fatalf("non-SGR content changed: %q", got)
	}
	if strings.Contains(remapANSI16("plain", ansiTokens{}), "\x1b") {
		t.Fatal("plain text gained an escape sequence")
	}
}
