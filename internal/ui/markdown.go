package ui

import (
	"fmt"
	"strings"

	"charm.land/glamour/v2"
	glamouransi "charm.land/glamour/v2/ansi"
	uv "github.com/charmbracelet/ultraviolet"
)

type markdownRenderer struct {
	style glamouransi.StyleConfig
}

type markdownArtifact struct {
	Title    string
	Content  string
	Language string
}

func renderMarkdown(
	currentTheme theme,
	width int,
	artifacts []markdownArtifact,
) (string, error) {
	var markdown strings.Builder
	for index, artifact := range artifacts {
		if index > 0 {
			markdown.WriteString("\n")
		}
		if artifact.Title != "" {
			markdown.WriteString("## ")
			markdown.WriteString(artifact.Title)
			markdown.WriteString("\n\n")
		}
		if artifact.Language == "" {
			markdown.WriteString(artifact.Content)
			if !strings.HasSuffix(artifact.Content, "\n") {
				markdown.WriteString("\n")
			}
			continue
		}
		markdown.WriteString("```")
		markdown.WriteString(artifact.Language)
		markdown.WriteString("\n")
		markdown.WriteString(strings.TrimSuffix(artifact.Content, "\n"))
		markdown.WriteString("\n```\n")
	}
	return newMarkdownRenderer(currentTheme.tokens).render(markdown.String(), width)
}

func newMarkdownRenderer(tokens colorTokens) markdownRenderer {
	return markdownRenderer{style: glamourStyle(tokens)}
}

func (renderer markdownRenderer) renderPlan(markdown string, width int) (string, error) {
	return renderer.render(markdown, width)
}

func (renderer markdownRenderer) renderVerdict(markdown string, width int) (string, error) {
	return renderer.render(markdown, width)
}

func (renderer markdownRenderer) renderDiff(diff string, width int) (string, error) {
	fenced := "```diff\n" + strings.TrimSuffix(diff, "\n") + "\n```\n"
	return renderer.render(fenced, width)
}

func (renderer markdownRenderer) render(markdown string, width int) (string, error) {
	termRenderer, err := glamour.NewTermRenderer(
		glamour.WithWordWrap(max(1, width)),
		glamour.WithStyles(renderer.style),
		glamour.WithChromaFormatter("terminal16m"),
	)
	if err != nil {
		return "", fmt.Errorf("ui: create markdown renderer: %w", err)
	}
	rendered, err := termRenderer.Render(markdown)
	if err != nil {
		return "", fmt.Errorf("ui: render markdown: %w", err)
	}
	return rendered, nil
}

func glamourStyle(tokens colorTokens) glamouransi.StyleConfig {
	theme := tokens.Theme
	component := tokens.Component
	syntax := tokens.Syntax
	diff := tokens.Diff
	bold := true
	italic := true
	underline := true

	return glamouransi.StyleConfig{
		Document: glamouransi.StyleBlock{
			StylePrimitive: glamouransi.StylePrimitive{
				Color: colorPointer(theme.FGBase),
			},
		},
		BlockQuote: glamouransi.StyleBlock{
			StylePrimitive: glamouransi.StylePrimitive{
				Color: colorPointer(theme.FGSubtle),
			},
			Indent:      uintPointer(1),
			IndentToken: stringPointer("│ "),
		},
		Paragraph: glamouransi.StyleBlock{
			StylePrimitive: glamouransi.StylePrimitive{
				Color: colorPointer(theme.FGBase),
			},
		},
		List: glamouransi.StyleList{
			StyleBlock: glamouransi.StyleBlock{
				StylePrimitive: glamouransi.StylePrimitive{
					Color: colorPointer(theme.FGBase),
				},
			},
			LevelIndent: 2,
		},
		Heading: glamouransi.StyleBlock{
			StylePrimitive: glamouransi.StylePrimitive{
				Color:       colorPointer(theme.Primary),
				Bold:        &bold,
				BlockSuffix: "\n",
			},
		},
		H1: glamouransi.StyleBlock{
			StylePrimitive: glamouransi.StylePrimitive{
				Color:  colorPointer(theme.Secondary),
				Prefix: "# ",
				Bold:   &bold,
			},
		},
		H2: glamouransi.StyleBlock{
			StylePrimitive: glamouransi.StylePrimitive{
				Color:  colorPointer(theme.Primary),
				Prefix: "## ",
				Bold:   &bold,
			},
		},
		H3: glamouransi.StyleBlock{
			StylePrimitive: glamouransi.StylePrimitive{
				Color:  colorPointer(theme.Accent),
				Prefix: "### ",
				Bold:   &bold,
			},
		},
		H4: glamouransi.StyleBlock{
			StylePrimitive: glamouransi.StylePrimitive{
				Color:  colorPointer(theme.Keyword),
				Prefix: "#### ",
			},
		},
		H5: glamouransi.StyleBlock{
			StylePrimitive: glamouransi.StylePrimitive{
				Color:  colorPointer(theme.InfoMoreSubtle),
				Prefix: "##### ",
			},
		},
		H6: glamouransi.StyleBlock{
			StylePrimitive: glamouransi.StylePrimitive{
				Color:  colorPointer(theme.FGSubtle),
				Prefix: "###### ",
			},
		},
		Text: glamouransi.StylePrimitive{
			Color: colorPointer(theme.FGBase),
		},
		Emph: glamouransi.StylePrimitive{
			Color:  colorPointer(theme.FGSubtle),
			Italic: &italic,
		},
		Strong: glamouransi.StylePrimitive{
			Color: colorPointer(theme.Accent),
			Bold:  &bold,
		},
		HorizontalRule: glamouransi.StylePrimitive{
			Color:  colorPointer(theme.Separator),
			Format: "\n────────\n",
		},
		Item: glamouransi.StylePrimitive{
			Color:       colorPointer(theme.FGBase),
			BlockPrefix: "• ",
		},
		Enumeration: glamouransi.StylePrimitive{
			Color:       colorPointer(theme.Info),
			BlockPrefix: ". ",
		},
		Task: glamouransi.StyleTask{
			StylePrimitive: glamouransi.StylePrimitive{
				Color: colorPointer(theme.FGBase),
			},
			Ticked:   "[✓] ",
			Unticked: "[ ] ",
		},
		Link: glamouransi.StylePrimitive{
			Color:     colorPointer(component.Link),
			Underline: &underline,
		},
		LinkText: glamouransi.StylePrimitive{
			Color: colorPointer(component.Link),
		},
		Code: glamouransi.StyleBlock{
			StylePrimitive: glamouransi.StylePrimitive{
				Color:           colorPointer(component.CodeInlineFG),
				BackgroundColor: colorPointer(component.CodeInlineBG),
			},
		},
		CodeBlock: glamouransi.StyleCodeBlock{
			StyleBlock: glamouransi.StyleBlock{
				StylePrimitive: glamouransi.StylePrimitive{
					Color:           colorPointer(syntax.Text),
					BackgroundColor: colorPointer(theme.BGLessVisible),
				},
			},
			Chroma: &glamouransi.Chroma{
				Text:                syntaxStyle(syntax.Text),
				Error:               syntaxStyle(syntax.Error),
				Comment:             syntaxStyle(syntax.Comment),
				CommentPreproc:      syntaxStyle(syntax.Preprocessor),
				Keyword:             syntaxStyle(syntax.Keyword),
				KeywordReserved:     syntaxStyle(syntax.KeywordReserved),
				KeywordNamespace:    syntaxStyle(syntax.KeywordNamespace),
				KeywordType:         syntaxStyle(syntax.Type),
				Operator:            syntaxStyle(syntax.Operator),
				Punctuation:         syntaxStyle(syntax.Punctuation),
				Name:                syntaxStyle(syntax.Name),
				NameBuiltin:         syntaxStyle(syntax.Builtin),
				NameTag:             syntaxStyle(syntax.Tag),
				NameAttribute:       syntaxStyle(syntax.Attribute),
				NameClass:           syntaxStyle(syntax.Class),
				NameConstant:        syntaxStyle(syntax.Name),
				NameDecorator:       syntaxStyle(syntax.Decorator),
				NameException:       syntaxStyle(syntax.Error),
				NameFunction:        syntaxStyle(syntax.Function),
				NameOther:           syntaxStyle(syntax.Name),
				Literal:             syntaxStyle(syntax.Text),
				LiteralNumber:       syntaxStyle(syntax.Number),
				LiteralDate:         syntaxStyle(syntax.String),
				LiteralString:       syntaxStyle(syntax.String),
				LiteralStringEscape: syntaxStyle(syntax.StringEscape),
				GenericDeleted:      syntaxStyleWithBackground(diff.DeleteFG, diff.DeleteLineBG),
				GenericEmph:         syntaxStyle(theme.Accent),
				GenericInserted:     syntaxStyleWithBackground(diff.InsertFG, diff.InsertLineBG),
				GenericStrong: glamouransi.StylePrimitive{
					Color: colorPointer(theme.FGBase),
					Bold:  &bold,
				},
				GenericSubheading: syntaxStyle(theme.Keyword),
				Background:        syntaxStyle(theme.BGLessVisible),
			},
		},
		Table: glamouransi.StyleTable{
			StyleBlock: glamouransi.StyleBlock{
				StylePrimitive: glamouransi.StylePrimitive{
					Color: colorPointer(theme.FGBase),
				},
			},
			CenterSeparator: stringPointer("┼"),
			ColumnSeparator: stringPointer("│"),
			RowSeparator:    stringPointer("─"),
		},
		DefinitionTerm: glamouransi.StylePrimitive{
			Color: colorPointer(theme.Accent),
			Bold:  &bold,
		},
		DefinitionDescription: glamouransi.StylePrimitive{
			Color:       colorPointer(theme.FGSubtle),
			BlockPrefix: "  ",
		},
		HTMLBlock: glamouransi.StyleBlock{
			StylePrimitive: glamouransi.StylePrimitive{
				Color: colorPointer(theme.FGSubtle),
			},
		},
		HTMLSpan: glamouransi.StyleBlock{
			StylePrimitive: glamouransi.StylePrimitive{
				Color: colorPointer(theme.FGSubtle),
			},
		},
	}
}

func syntaxStyle(color string) glamouransi.StylePrimitive {
	return glamouransi.StylePrimitive{Color: colorPointer(color)}
}

func syntaxStyleWithBackground(
	foreground string,
	background string,
) glamouransi.StylePrimitive {
	return glamouransi.StylePrimitive{
		Color:           colorPointer(foreground),
		BackgroundColor: colorPointer(background),
	}
}

func colorPointer(value string) *string {
	return &value
}

func stringPointer(value string) *string {
	return &value
}

func uintPointer(value uint) *uint {
	return &value
}

type uvRegion struct {
	X       int
	Y       int
	Width   int
	Height  int
	Content string
	Wrap    bool
	Tail    string
}

// composeUV converts styled ANSI component strings into cells, composites
// them into non-overlapping rectangles, then returns one styled ANSI frame.
func composeUV(width int, height int, regions []uvRegion) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	screen := uv.NewScreenBuffer(width, height)
	bounds := screen.Bounds()
	for _, region := range regions {
		area := uv.Rect(
			region.X,
			region.Y,
			region.Width,
			region.Height,
		).Intersect(bounds)
		if area.Empty() {
			continue
		}
		styled := uv.NewStyledString(region.Content)
		styled.Wrap = region.Wrap
		styled.Tail = region.Tail
		styled.Draw(screen, area)
	}
	return screen.Render()
}
