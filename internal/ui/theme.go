package ui

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"
	"github.com/ridenow/coterix/design"
)

// quickStyleOpts mirrors the semantic theme object in
// design/coterix-color-tokens.json one field for one field.
type quickStyleOpts struct {
	Primary           string `json:"primary"`
	Secondary         string `json:"secondary"`
	Accent            string `json:"accent"`
	Keyword           string `json:"keyword"`
	FGBase            string `json:"fgBase"`
	FGSubtle          string `json:"fgSubtle"`
	FGMoreSubtle      string `json:"fgMoreSubtle"`
	FGMostSubtle      string `json:"fgMostSubtle"`
	OnPrimary         string `json:"onPrimary"`
	BGBase            string `json:"bgBase"`
	BGLeastVisible    string `json:"bgLeastVisible"`
	BGLessVisible     string `json:"bgLessVisible"`
	BGMostVisible     string `json:"bgMostVisible"`
	Separator         string `json:"separator"`
	Destructive       string `json:"destructive"`
	Error             string `json:"error"`
	Warning           string `json:"warning"`
	WarningSubtle     string `json:"warningSubtle"`
	Denied            string `json:"denied"`
	Busy              string `json:"busy"`
	Info              string `json:"info"`
	InfoMoreSubtle    string `json:"infoMoreSubtle"`
	InfoMostSubtle    string `json:"infoMostSubtle"`
	Success           string `json:"success"`
	SuccessMoreSubtle string `json:"successMoreSubtle"`
	SuccessMostSubtle string `json:"successMostSubtle"`
}

type paletteRampTokens struct {
	Shade50  string `json:"50"`
	Shade100 string `json:"100"`
	Shade200 string `json:"200"`
	Shade300 string `json:"300"`
	Shade400 string `json:"400"`
	Shade500 string `json:"500"`
	Shade600 string `json:"600"`
	Shade700 string `json:"700"`
	Shade800 string `json:"800"`
	Shade900 string `json:"900"`
	Shade950 string `json:"950"`
}

type paletteTokens struct {
	Ink     paletteRampTokens `json:"ink"`
	Codex   paletteRampTokens `json:"codex"`
	Claude  paletteRampTokens `json:"claude"`
	Violet  paletteRampTokens `json:"violet"`
	Success paletteRampTokens `json:"success"`
	Warning paletteRampTokens `json:"warning"`
	Danger  paletteRampTokens `json:"danger"`
	Cyan    paletteRampTokens `json:"cyan"`
}

type gradientTokens struct {
	BrandLeftToRight []string `json:"brandLeftToRight"`
	BrandReverse     []string `json:"brandReverse"`
	Codex            []string `json:"codex"`
	Claude           []string `json:"claude"`
	Ambient          []string `json:"ambient"`
}

type componentTokens struct {
	FocusRing           string `json:"focusRing"`
	SelectionBG         string `json:"selectionBg"`
	SelectionFG         string `json:"selectionFg"`
	Cursor              string `json:"cursor"`
	BorderFocused       string `json:"borderFocused"`
	BorderBlurred       string `json:"borderBlurred"`
	Link                string `json:"link"`
	LinkVisited         string `json:"linkVisited"`
	CodeInlineFG        string `json:"codeInlineFg"`
	CodeInlineBG        string `json:"codeInlineBg"`
	WorkingGradientFrom string `json:"workingGradientFrom"`
	WorkingGradientTo   string `json:"workingGradientTo"`
}

type statusColorTokens struct {
	FG         string `json:"fg"`
	Subtle     string `json:"subtle"`
	BG         string `json:"bg"`
	EmphasisBG string `json:"emphasisBg"`
}

type statusTokens struct {
	Success statusColorTokens `json:"success"`
	Warning statusColorTokens `json:"warning"`
	Error   statusColorTokens `json:"error"`
	Info    statusColorTokens `json:"info"`
	Busy    statusColorTokens `json:"busy"`
}

type diffTokens struct {
	InsertFG         string `json:"insertFg"`
	InsertSymbol     string `json:"insertSymbol"`
	InsertLineBG     string `json:"insertLineBg"`
	InsertEmphasisBG string `json:"insertEmphasisBg"`
	DeleteFG         string `json:"deleteFg"`
	DeleteSymbol     string `json:"deleteSymbol"`
	DeleteLineBG     string `json:"deleteLineBg"`
	DeleteEmphasisBG string `json:"deleteEmphasisBg"`
	DividerFG        string `json:"dividerFg"`
	DividerBG        string `json:"dividerBg"`
}

type syntaxTokens struct {
	Text             string `json:"text"`
	Error            string `json:"error"`
	Comment          string `json:"comment"`
	Preprocessor     string `json:"preprocessor"`
	Keyword          string `json:"keyword"`
	KeywordReserved  string `json:"keywordReserved"`
	KeywordNamespace string `json:"keywordNamespace"`
	Type             string `json:"type"`
	Operator         string `json:"operator"`
	Punctuation      string `json:"punctuation"`
	Name             string `json:"name"`
	Builtin          string `json:"builtin"`
	Tag              string `json:"tag"`
	Attribute        string `json:"attribute"`
	Class            string `json:"class"`
	Decorator        string `json:"decorator"`
	Function         string `json:"function"`
	Number           string `json:"number"`
	String           string `json:"string"`
	StringEscape     string `json:"stringEscape"`
	Inserted         string `json:"inserted"`
	Deleted          string `json:"deleted"`
}

type ansiTokens struct {
	Black         string `json:"black"`
	Red           string `json:"red"`
	Green         string `json:"green"`
	Yellow        string `json:"yellow"`
	Blue          string `json:"blue"`
	Magenta       string `json:"magenta"`
	Cyan          string `json:"cyan"`
	White         string `json:"white"`
	BrightBlack   string `json:"brightBlack"`
	BrightRed     string `json:"brightRed"`
	BrightGreen   string `json:"brightGreen"`
	BrightYellow  string `json:"brightYellow"`
	BrightBlue    string `json:"brightBlue"`
	BrightMagenta string `json:"brightMagenta"`
	BrightCyan    string `json:"brightCyan"`
	BrightWhite   string `json:"brightWhite"`
}

func (tokens ansiTokens) colors() [16]string {
	return [16]string{
		tokens.Black,
		tokens.Red,
		tokens.Green,
		tokens.Yellow,
		tokens.Blue,
		tokens.Magenta,
		tokens.Cyan,
		tokens.White,
		tokens.BrightBlack,
		tokens.BrightRed,
		tokens.BrightGreen,
		tokens.BrightYellow,
		tokens.BrightBlue,
		tokens.BrightMagenta,
		tokens.BrightCyan,
		tokens.BrightWhite,
	}
}

type colorTokens struct {
	Meta      json.RawMessage `json:"meta"`
	Palette   paletteTokens   `json:"palette"`
	Gradient  gradientTokens  `json:"gradient"`
	Theme     quickStyleOpts  `json:"theme"`
	Component componentTokens `json:"component"`
	Status    statusTokens    `json:"status"`
	Diff      diffTokens      `json:"diff"`
	Syntax    syntaxTokens    `json:"syntax"`
	ANSI      ansiTokens      `json:"ansi"`
}

type theme struct {
	tokens colorTokens
	styles dashboardStyles
}

func loadTheme() (theme, error) {
	tokens, err := loadColorTokens()
	if err != nil {
		return theme{}, err
	}
	return theme{
		tokens: tokens,
		styles: newDashboardStyles(tokens),
	}, nil
}

func loadColorTokens() (colorTokens, error) {
	return parseColorTokens([]byte(design.CoterixColorTokensJSON()))
}

func parseColorTokens(source []byte) (colorTokens, error) {
	var tokens colorTokens
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&tokens); err != nil {
		return colorTokens{}, fmt.Errorf("ui: decode color tokens: %w", err)
	}
	if err := ensureJSONEnd(decoder); err != nil {
		return colorTokens{}, err
	}
	if err := validateColorTokens(tokens); err != nil {
		return colorTokens{}, err
	}
	return tokens, nil
}

func ensureJSONEnd(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("ui: color tokens contain multiple JSON values")
		}
		return fmt.Errorf("ui: decode trailing color-token data: %w", err)
	}
	return nil
}

func validateColorTokens(tokens colorTokens) error {
	sections := []struct {
		name  string
		value any
	}{
		{name: "palette", value: tokens.Palette},
		{name: "gradient", value: tokens.Gradient},
		{name: "theme", value: tokens.Theme},
		{name: "component", value: tokens.Component},
		{name: "status", value: tokens.Status},
		{name: "diff", value: tokens.Diff},
		{name: "syntax", value: tokens.Syntax},
		{name: "ansi", value: tokens.ANSI},
	}
	for _, section := range sections {
		if err := validateColorSection(section.name, reflect.ValueOf(section.value)); err != nil {
			return err
		}
	}
	return nil
}

func validateColorSection(path string, value reflect.Value) error {
	switch value.Kind() {
	case reflect.Struct:
		valueType := value.Type()
		for index := range value.NumField() {
			field := valueType.Field(index)
			name := field.Tag.Get("json")
			if comma := strings.IndexByte(name, ','); comma >= 0 {
				name = name[:comma]
			}
			if name == "" {
				name = field.Name
			}
			if err := validateColorSection(path+"."+name, value.Field(index)); err != nil {
				return err
			}
		}
	case reflect.String:
		if err := validateHexColor(value.String()); err != nil {
			return fmt.Errorf("ui: invalid color token %s: %w", path, err)
		}
	case reflect.Slice:
		if value.Len() == 0 {
			return fmt.Errorf("ui: color token list %s must not be empty", path)
		}
		for index := range value.Len() {
			if err := validateColorSection(
				fmt.Sprintf("%s[%d]", path, index),
				value.Index(index),
			); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("ui: unsupported color token type at %s", path)
	}
	return nil
}

func validateHexColor(value string) error {
	if len(value) != 7 || value[0] != '#' {
		return fmt.Errorf("expected #RRGGBB")
	}
	if _, err := hex.DecodeString(value[1:]); err != nil {
		return fmt.Errorf("expected #RRGGBB: %w", err)
	}
	return nil
}

type dashboardStyles struct {
	Canvas       lipgloss.Style
	Main         lipgloss.Style
	MainCard     lipgloss.Style
	Sidebar      lipgloss.Style
	Header       lipgloss.Style
	StatusBar    lipgloss.Style
	Logo         lipgloss.Style
	SectionTitle lipgloss.Style
	Separator    lipgloss.Style
	// BorderFocus draws the border of a focused box (T14 W2). The token was
	// previously consumed only by the Input frame.
	BorderFocus lipgloss.Style
	// TabActive marks the selected artifact tab (T14 W3): Primary carries the
	// colour, bold carries the same signal without it.
	TabActive lipgloss.Style
	Label     lipgloss.Style
	Value     lipgloss.Style
	Muted     lipgloss.Style
	Primary   lipgloss.Style
	Secondary lipgloss.Style
	Claude    lipgloss.Style
	Codex     lipgloss.Style
	Success   lipgloss.Style
	Warning   lipgloss.Style
	Error     lipgloss.Style
	Info      lipgloss.Style
	Busy      lipgloss.Style
	Hint      lipgloss.Style
	// Link styles OSC8 hyperlinks (T14 W9). `component.link` was a fully unconsumed
	// token before this.
	Link         lipgloss.Style
	PhaseInfo    lipgloss.Style
	PhaseWarning lipgloss.Style
	PhaseSuccess lipgloss.Style
	PhaseError   lipgloss.Style
	PhaseBusy    lipgloss.Style
	Input        lipgloss.Style
	InputCursor  lipgloss.Style
	DiffInsert   lipgloss.Style
	DiffDelete   lipgloss.Style
	DiffDivider  lipgloss.Style
}

func newDashboardStyles(tokens colorTokens) dashboardStyles {
	theme := tokens.Theme
	palette := tokens.Palette
	component := tokens.Component
	status := tokens.Status
	diff := tokens.Diff

	// Surface styles carry no background on purpose: the dashboard inherits
	// the terminal background uniformly instead of painting partial patches.
	// Backgrounds remain only on intentional chips (status/working/input).
	return dashboardStyles{
		Canvas: lipgloss.NewStyle().
			Foreground(lipgloss.Color(theme.FGBase)),
		Main: lipgloss.NewStyle().
			Foreground(lipgloss.Color(theme.FGBase)),
		MainCard: lipgloss.NewStyle().
			Foreground(lipgloss.Color(theme.FGBase)).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color(component.BorderBlurred)),
		Sidebar: lipgloss.NewStyle().
			Foreground(lipgloss.Color(theme.FGSubtle)).
			BorderLeft(true).
			BorderStyle(lipgloss.NormalBorder()).
			BorderLeftForeground(lipgloss.Color(theme.Separator)),
		Header: lipgloss.NewStyle().
			Foreground(lipgloss.Color(theme.FGBase)).
			Bold(true),
		StatusBar: lipgloss.NewStyle().
			Foreground(lipgloss.Color(theme.FGSubtle)),
		Logo: lipgloss.NewStyle().
			Foreground(lipgloss.Color(theme.Primary)).
			Bold(true),
		SectionTitle: lipgloss.NewStyle().
			Foreground(lipgloss.Color(theme.Accent)).
			Bold(true),
		Separator: lipgloss.NewStyle().
			Foreground(lipgloss.Color(theme.Separator)),
		BorderFocus: lipgloss.NewStyle().
			Foreground(lipgloss.Color(component.BorderFocused)),
		TabActive: lipgloss.NewStyle().
			Foreground(lipgloss.Color(theme.Primary)).
			Bold(true),
		Link: lipgloss.NewStyle().
			Foreground(lipgloss.Color(component.Link)).
			Underline(true),
		Label: lipgloss.NewStyle().
			Foreground(lipgloss.Color(theme.FGMoreSubtle)),
		Value: lipgloss.NewStyle().
			Foreground(lipgloss.Color(theme.FGBase)),
		Muted: lipgloss.NewStyle().
			Foreground(lipgloss.Color(theme.FGMostSubtle)),
		Primary: lipgloss.NewStyle().
			Foreground(lipgloss.Color(theme.Primary)),
		Secondary: lipgloss.NewStyle().
			Foreground(lipgloss.Color(theme.Secondary)),
		Claude: lipgloss.NewStyle().
			Foreground(lipgloss.Color(palette.Claude.Shade400)),
		Codex: lipgloss.NewStyle().
			Foreground(lipgloss.Color(palette.Codex.Shade400)),
		Success: lipgloss.NewStyle().
			Foreground(lipgloss.Color(status.Success.FG)).
			Background(lipgloss.Color(status.Success.BG)),
		Warning: lipgloss.NewStyle().
			Foreground(lipgloss.Color(status.Warning.FG)).
			Background(lipgloss.Color(status.Warning.BG)),
		Error: lipgloss.NewStyle().
			Foreground(lipgloss.Color(status.Error.FG)).
			Background(lipgloss.Color(status.Error.BG)),
		Info: lipgloss.NewStyle().
			Foreground(lipgloss.Color(status.Info.FG)).
			Background(lipgloss.Color(status.Info.BG)),
		Busy: lipgloss.NewStyle().
			Foreground(lipgloss.Color(status.Busy.FG)).
			Background(lipgloss.Color(status.Busy.BG)),
		Hint: lipgloss.NewStyle().
			Foreground(lipgloss.Color(theme.FGSubtle)),
		PhaseInfo: lipgloss.NewStyle().
			Foreground(lipgloss.Color(status.Info.FG)),
		PhaseWarning: lipgloss.NewStyle().
			Foreground(lipgloss.Color(status.Warning.FG)),
		PhaseSuccess: lipgloss.NewStyle().
			Foreground(lipgloss.Color(status.Success.FG)),
		PhaseError: lipgloss.NewStyle().
			Foreground(lipgloss.Color(status.Error.FG)),
		PhaseBusy: lipgloss.NewStyle().
			Foreground(lipgloss.Color(status.Busy.FG)),
		Input: lipgloss.NewStyle().
			Foreground(lipgloss.Color(component.SelectionFG)).
			Background(lipgloss.Color(component.SelectionBG)).
			BorderForeground(lipgloss.Color(component.BorderFocused)).
			Border(lipgloss.NormalBorder()),
		InputCursor: lipgloss.NewStyle().
			Foreground(lipgloss.Color(component.Cursor)),
		DiffInsert: lipgloss.NewStyle().
			Foreground(lipgloss.Color(diff.InsertFG)).
			Background(lipgloss.Color(diff.InsertLineBG)),
		DiffDelete: lipgloss.NewStyle().
			Foreground(lipgloss.Color(diff.DeleteFG)).
			Background(lipgloss.Color(diff.DeleteLineBG)),
		DiffDivider: lipgloss.NewStyle().
			Foreground(lipgloss.Color(diff.DividerFG)).
			Background(lipgloss.Color(diff.DividerBG)),
	}
}

func workingSpinner(tokens colorTokens) spinner.Spinner {
	colors := lipgloss.Blend1D(
		len(spinner.Points.Frames),
		lipgloss.Color(tokens.Component.WorkingGradientFrom),
		lipgloss.Color(tokens.Component.WorkingGradientTo),
	)
	frames := make([]string, len(spinner.Points.Frames))
	for index, frame := range spinner.Points.Frames {
		frames[index] = lipgloss.NewStyle().
			Foreground(colors[index]).
			Background(lipgloss.Color(tokens.Status.Busy.BG)).
			Render(frame)
	}
	return spinner.Spinner{
		Frames: frames,
		FPS:    225 * time.Millisecond,
	}
}

// remapANSI16 replaces only ANSI 0-15 foreground/background SGR parameters
// with their token colors. Other SGR parameters and non-SGR bytes are retained.
func remapANSI16(input string, palette ansiTokens) string {
	colors := palette.colors()
	var output strings.Builder
	output.Grow(len(input))

	for offset := 0; offset < len(input); {
		if input[offset] != '\x1b' || offset+1 >= len(input) || input[offset+1] != '[' {
			output.WriteByte(input[offset])
			offset++
			continue
		}

		end := offset + 2
		for end < len(input) && (input[end] < 0x40 || input[end] > 0x7e) {
			end++
		}
		if end >= len(input) {
			output.WriteString(input[offset:])
			break
		}
		if input[end] != 'm' {
			output.WriteString(input[offset : end+1])
			offset = end + 1
			continue
		}

		params := strings.Split(input[offset+2:end], ";")
		for index := 0; index < len(params); {
			parameter := params[index]
			code, err := strconv.Atoi(parameter)
			if err != nil {
				index++
				continue
			}
			if consumed := extendedColorParameterCount(params[index:]); consumed > 0 {
				index += consumed
				continue
			}
			colorIndex, background, ok := ansi16Index(code)
			if !ok {
				index++
				continue
			}
			red, green, blue, valid := rgbBytes(colors[colorIndex])
			if !valid {
				index++
				continue
			}
			prefix := "38"
			if background {
				prefix = "48"
			}
			params[index] = fmt.Sprintf("%s;2;%d;%d;%d", prefix, red, green, blue)
			index++
		}
		output.WriteString(input[offset : offset+2])
		output.WriteString(strings.Join(params, ";"))
		output.WriteByte('m')
		offset = end + 1
	}
	return output.String()
}

func extendedColorParameterCount(params []string) int {
	if len(params) < 2 {
		return 0
	}
	code, err := strconv.Atoi(params[0])
	if err != nil || (code != 38 && code != 48 && code != 58) {
		return 0
	}
	mode, err := strconv.Atoi(params[1])
	if err != nil {
		return min(2, len(params))
	}
	switch mode {
	case 5:
		return min(3, len(params))
	case 2:
		count := 5
		if len(params) > 2 && params[2] == "" {
			count = 6
		}
		return min(count, len(params))
	default:
		return min(2, len(params))
	}
}

func ansi16Index(code int) (index int, background bool, ok bool) {
	switch {
	case code >= 30 && code <= 37:
		return code - 30, false, true
	case code >= 90 && code <= 97:
		return code - 90 + 8, false, true
	case code >= 40 && code <= 47:
		return code - 40, true, true
	case code >= 100 && code <= 107:
		return code - 100 + 8, true, true
	default:
		return 0, false, false
	}
}

func rgbBytes(value string) (byte, byte, byte, bool) {
	if validateHexColor(value) != nil {
		return 0, 0, 0, false
	}
	decoded, _ := hex.DecodeString(value[1:])
	return decoded[0], decoded[1], decoded[2], true
}
