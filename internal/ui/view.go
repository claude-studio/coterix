package ui

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/ridenow/coterix/internal/pipeline"
	"github.com/ridenow/coterix/internal/runner"
	"github.com/ridenow/coterix/internal/state"
)

const (
	sidebarWidth    = 32
	topBarHeight    = 2
	wordmarkText    = "C O T E R I X"
	logTagCellWidth = 6
)

// THESIS: Make a synchronous multi-agent pipeline readable as one live
// instrument panel, without turning it into chat or session chrome.
// OWN-WORLD: COTERIX ink surfaces, token-driven Claude→Codex accents, compact
// evidence marks, and a single right-hand telemetry rail.
// STORY: Follow agent output, verify hard/soft gates, then answer the one human
// action that is actually blocking progress.
// FIRST VIEWPORT: Wide terminals show a brand top bar, a bordered feed card,
// and a 32-cell rail of PIPELINE/STATUS cards; compact terminals fold that
// rail into a two-line header above the same feed and status bar.
// FORM: Operate-mode dashboard, fixed by docs/spec/ui.md
// (north star: assets/coterix-color-system.png).

type sidebarData struct {
	RunID            string
	Phase            state.Phase
	Role             string
	CLI              string
	TaskID           string
	TaskStatus       state.TaskStatus
	Attempt          int
	Gate             evidenceOutcome
	Review           evidenceOutcome
	PlanRound        int
	Confirmed        int
	Total            int
	AwaitingApproval bool
	PendingKind      state.PendingKind
	PendingPrompt    string
	LastError        string
}

func renderDashboard(current model) string {
	width := max(1, current.width)
	height := max(1, current.height)
	statusHeight := 2
	if current.prompt != promptNone {
		statusHeight = min(promptRegionRows(current), height)
	}

	if isWide(width, height) {
		mainWidth := max(1, width-sidebarWidth)
		contentHeight := max(1, height-topBarHeight-statusHeight)
		topBar := renderTopBar(current, width, topBarHeight)
		main := renderMain(current, mainWidth, contentHeight)
		sidebar := renderSidebar(
			current,
			sidebarWidth,
			contentHeight,
		)
		statusBar := renderStatusBar(current, width, statusHeight)
		return composeUV(width, height, appendHelpOverlay(current, []uvRegion{
			{
				X:       0,
				Y:       0,
				Width:   width,
				Height:  topBarHeight,
				Content: topBar,
			},
			{
				X:       0,
				Y:       topBarHeight,
				Width:   mainWidth,
				Height:  contentHeight,
				Content: main,
			},
			{
				X:       mainWidth,
				Y:       topBarHeight,
				Width:   sidebarWidth,
				Height:  contentHeight,
				Content: sidebar,
			},
			{
				X:       0,
				Y:       topBarHeight + contentHeight,
				Width:   width,
				Height:  statusHeight,
				Content: statusBar,
			},
		}))
	}

	headerHeight := min(2, height)
	contentHeight := max(1, height-headerHeight-statusHeight)
	header := renderCompactHeader(current, width, headerHeight)
	main := renderMain(current, width, contentHeight)
	statusBar := renderStatusBar(current, width, statusHeight)
	return composeUV(width, height, appendHelpOverlay(current, []uvRegion{
		{
			X:       0,
			Y:       0,
			Width:   width,
			Height:  headerHeight,
			Content: header,
		},
		{
			X:       0,
			Y:       headerHeight,
			Width:   width,
			Height:  contentHeight,
			Content: main,
		},
		{
			X:       0,
			Y:       headerHeight + contentHeight,
			Width:   width,
			Height:  statusHeight,
			Content: statusBar,
		},
	}))
}

// appendHelpOverlay puts the key overlay on top of everything else — composeUV draws
// regions in order, so being last is what makes it an overlay rather than a pane
// (T14 W6). It is centred over the content and leaves the status bar visible.
func appendHelpOverlay(current model, regions []uvRegion) []uvRegion {
	if !current.helpOpen {
		return regions
	}
	width := max(1, current.width)
	height := max(1, current.height)
	cardWidth := min(max(24, width-8), 78)
	cardHeight := min(max(6, height-6), 22)
	return append(regions, uvRegion{
		X:       max(0, (width-cardWidth)/2),
		Y:       max(0, (height-cardHeight)/2),
		Width:   cardWidth,
		Height:  cardHeight,
		Content: renderHelpOverlay(current, cardWidth, cardHeight),
	})
}

func isWide(width, height int) bool {
	return width >= wideBreakpointWidth && height >= wideBreakpointHeight
}

// dashboardMainInnerWidth is the width the artifact markdown is rendered (and
// cached) at. It has to equal the *actual* body width of the box that shows it,
// otherwise the cached lines are either truncated on the right (wide) or re-wrapped
// by lipgloss, which silently adds rows and pushes the bottom box off screen
// (compact) — review T14a f1.
//
// wide:    pane − MainCard(border 2 + padding 2) − box(border 2 + padding 2)
// compact: pane − Main padding(2 each side)
func dashboardMainInnerWidth(width, height int) int {
	if isWide(width, height) {
		boxWidth := max(8, max(1, width-sidebarWidth)-4)
		return max(1, boxWidth-4)
	}
	return max(1, max(1, width-4)-4)
}

// renderMain wraps the feed in the bordered command card on wide layouts. The
// compact branch keeps the pre-card feed exactly (design-plan.md v1
// acceptance: compact runs header+feed+status without the card).
// renderMain lays the wide pane out as independently scrollable boxes (T14 W1):
// PENDING (only while blocked) · PIPELINE FEED · LIVE OUTPUT · ACTIVITY. Each
// box owns its scroll offset so reading the artifacts never displaces the live
// activity, and `tab` moves focus between them (T14 W2).
func renderMain(current model, width, height int) string {
	if !isWide(current.width, current.height) {
		return renderMainCompact(current, width, height)
	}
	// lipgloss Width() is the *final* width including the border (measured in
	// T12), so MainCard spends 2 cells on its border and 2 on padding before the
	// boxes get any room; each box then spends 4 more on its own border and
	// padding. Overshooting here wraps every box line and doubles the row count.
	boxWidth := max(8, width-4)
	innerWidth := max(1, boxWidth-4)
	order := mainBoxOrder(current)
	// MainCard's own border costs 2 rows and the header claims 1; the boxes share
	// what is left.
	total := max(1, height-3)

	bodies := make([]string, len(order))
	wants := make([]int, len(order))
	for index, box := range order {
		bodies[index] = mainBoxBody(current, box, innerWidth, max(1, total/2-2))
		wants[index] = mainBoxWantRows(current, box, bodies[index], total, 2)
	}
	heights := distributeMainBoxHeights(order, wants, total, 2)

	parts := make([]string, 0, len(order)+1)
	parts = append(parts, renderMainHeader(current, boxWidth))
	for index, box := range order {
		if heights[index] < 3 {
			continue
		}
		body := visibleLines(
			bodies[index],
			heights[index]-2,
			current.boxScroll[box],
		)
		if box == boxFeed {
			// The artifacts box absorbs the leftover rows, so pad it to its
			// allocation — otherwise the pane ends in a ragged gap below the last
			// box. Pad as a line slice: appending "\n" instead would leave a
			// trailing newline that renderBoxCard's TrimSuffix then eats, making
			// the row count drift.
			lines := strings.Split(body, "\n")
			for len(lines) < heights[index]-2 {
				lines = append(lines, "")
			}
			body = strings.Join(lines, "\n")
		}
		parts = append(parts, renderBoxCard(
			current.theme,
			mainBoxTitle(current, box),
			mainBoxTitleSuffix(
				current,
				box,
				pausedBelow(bodies[index], heights[index]-2, current.boxScroll[box]),
			),
			body,
			boxWidth,
			current.normalizedFocus() == box,
		))
	}
	return current.theme.styles.MainCard.
		Width(max(1, width)).
		Height(max(1, height)).
		Padding(0, 1).
		Render(strings.Join(parts, "\n"))
}

// renderMainCompact keeps the single-column stack: at 80x24 there is no room for
// per-box chrome or a focus concept, so section titles separate the parts and one
// scroll drives the whole column (T14 W1/W2 — compact is explicitly excluded).
func renderMainCompact(current model, width, height int) string {
	innerWidth := max(1, width-4)
	// Main has no border, but its Padding(1, 2) costs 2 rows and 4 cells. Wrapping
	// bodies to innerWidth would overflow that by 4 cells, and lipgloss then wraps
	// again — silently inflating the row count past the budget.
	bodyWidth := max(1, innerWidth-4)
	innerHeight := max(1, height-2)
	order := mainBoxOrder(current)
	bodies := make([]string, len(order))
	wants := make([]int, len(order))
	for index, box := range order {
		bodies[index] = mainBoxBody(
			current,
			box,
			bodyWidth,
			max(1, innerHeight/2-1),
		)
		wants[index] = mainBoxWantRows(current, box, bodies[index], innerHeight, 1)
	}
	// One row of chrome per section (its title). Sharing one budget is what keeps
	// the bottom section from being clipped without a marker at 80x24, where a
	// widened failure tail and a PENDING question compete for 18 rows
	// (review round-3 f1).
	heights := distributeMainBoxHeights(order, wants, innerHeight, 1)

	parts := make([]string, 0, len(order)*2)
	for index, box := range order {
		if heights[index] < 2 {
			continue
		}
		title := current.theme.styles.SectionTitle.Render(
			"╱╱╱ " + mainBoxTitle(current, box),
		)
		if suffix := mainBoxTitleSuffix(
			current,
			box,
			pausedBelow(bodies[index], heights[index]-1, current.boxScroll[box]),
		); suffix != "" {
			title += "  " + suffix
		}
		parts = append(
			parts,
			title,
			visibleLines(
				bodies[index],
				heights[index]-1,
				current.boxScroll[box],
			),
		)
	}
	return current.theme.styles.Main.
		Width(innerWidth).
		Height(innerHeight).
		Padding(1, 2).
		Render(strings.Join(parts, "\n"))
}

// The four box bodies below are the *content* of the main pane's boxes, without
// section chrome, so the wide layout can put each one in its own scrollable box
// (T14 W1) while the compact layout still stacks them into one column.

func artifactBody(current model) string {
	if current.artifactRenderErr != nil {
		return current.theme.styles.PhaseError.Render(
			"× " + current.artifactRenderErr.Error(),
		)
	}
	if current.artifactRender != "" {
		return current.artifactRender
	}
	// An empty tab stays selectable, so it has to say what is missing rather than
	// leave the box blank (T14 W3).
	return current.theme.styles.Muted.Render(
		"⋯ No " + strings.ToLower(artifactTabLabels[current.artifactTab]) +
			" yet",
	)
}

// selectionGutter is the two cells every lifecycle row reserves on the left. It is
// a column rather than a prefix on the selected row alone: prefixing would shift
// that row's timestamp out of line with its neighbours (T14 W4).
const selectionGutter = 2

// maxExpandedTextRows bounds an expanded entry's wrapped body; the untruncated text
// stays in the run's logs/ files (review T14c f3). Bounding alone does not guarantee
// the block fits — PENDING outranks LIVE OUTPUT for rows — so `j/k` also walk the
// block (maxExpandedBlockRows, review T14c-r2 f2).
const (
	maxExpandedTextRows = 6
	// maxExpandedBlockRows is the header, the body and the withheld marker: the
	// furthest `k` can usefully walk up inside one block.
	maxExpandedBlockRows = maxExpandedTextRows + 2
)

func lifecycleBody(current model, innerWidth int) string {
	if len(current.logs) == 0 {
		return current.theme.styles.Muted.Render(
			"⋯ Waiting for the first pipeline step",
		)
	}
	textWidth := max(1, innerWidth-selectionGutter)
	lines := make([]string, 0, len(current.logs))
	for index, line := range current.logs {
		selected := current.hasSelection && index == current.selectedEntry
		gutter := strings.Repeat(" ", selectionGutter)
		if selected {
			// `▌` and `▸` carry the cue without colour; the strip takes the focused
			// border token (color-system.md: never state by colour alone). This is
			// option B of design-plan W4 — no background, so T12's "chips only carry
			// a background" policy needs no amendment.
			gutter = current.theme.styles.BorderFocus.Render("▌") +
				current.theme.styles.TabActive.Render("▸")
		}
		if selected && current.entryExpanded {
			lines = append(lines, expandedEntryLines(current, line, gutter, textWidth)...)
			continue
		}
		lines = append(
			lines,
			gutter+renderLogLine(current.theme, line, textWidth, false),
		)
	}
	return strings.Join(lines, "\n")
}

// expandedEntryLines renders the cursor's entry in full instead of truncating it to
// one row: the columns stay on the first line and the wrapped remainder is indented
// under the message column (T14 W4).
func expandedEntryLines(
	current model,
	line logEntry,
	gutter string,
	textWidth int,
) []string {
	head := line
	head.Text = ""
	rows := []string{
		gutter + strings.TrimRight(
			renderLogLine(current.theme, head, textWidth, false),
			" ",
		),
	}
	indent := strings.Repeat(" ", selectionGutter+2)
	wrapped := strings.Split(ansi.HardwrapWc(
		remapANSI16(line.Text, current.theme.tokens.ANSI),
		max(1, textWidth-2),
		false,
	), "\n")
	// The block has to fit the rows the box can actually give it, or its head — the
	// marker and the columns — is scrolled off with no key to bring it back
	// (review T14c f3). What is withheld is named, and the untruncated text is in
	// the run's logs/ files (spec: 원문 보존).
	withheld := 0
	if len(wrapped) > maxExpandedTextRows {
		withheld = len(wrapped) - maxExpandedTextRows
		wrapped = wrapped[:maxExpandedTextRows]
	}
	for _, row := range wrapped {
		rows = append(rows, indent+current.theme.styles.Value.Render(row))
	}
	if withheld > 0 {
		rows = append(rows, indent+current.theme.styles.Muted.Render(
			fmt.Sprintf("… %d more rows in logs/", withheld),
		))
	}
	return rows
}

func activityBody(current model, innerWidth, limit int) string {
	if len(current.activity) == 0 {
		return activityWaitingBody(current, innerWidth)
	}
	lines := current.activity
	if limit > 0 && len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		// muteStream: tail lines are progress, not diagnostics (codex writes
		// progress to stderr).
		out = append(out, renderLogLine(current.theme, line, innerWidth, true))
	}
	return strings.Join(out, "\n")
}

func pendingBody(current model, innerWidth, maxRows int) string {
	if !hasPendingPrompt(current) {
		return ""
	}
	lines := strings.Split(
		ansi.HardwrapWc(
			strings.TrimSpace(current.status.PendingAction.Prompt),
			max(1, innerWidth),
			false,
		),
		"\n",
	)
	if maxRows > 0 && len(lines) > maxRows {
		lines = lines[:maxRows]
		lines[maxRows-1] = ansi.TruncateWc(
			lines[maxRows-1],
			max(1, innerWidth-2),
			"",
		) + " …"
	}
	return strings.Join(lines, "\n")
}

func mainBoxTitle(current model, box mainBox) string {
	switch box {
	case boxPending:
		kind := ""
		if current.status.PendingAction != nil {
			kind = string(current.status.PendingAction.Kind)
		}
		return "PENDING · " + kind
	case boxFeed:
		return "PIPELINE FEED"
	case boxLiveOutput:
		return "LIVE OUTPUT"
	case boxActivity:
		return "ACTIVITY"
	}
	return ""
}

// mainBoxTitleSuffix is the styled trailing chrome of a box heading: FEED carries
// the artifact tab strip (T14 W3), and any parked box carries the `↓ new` cue.
func mainBoxTitleSuffix(current model, box mainBox, paused bool) string {
	parts := make([]string, 0, 2)
	if box == boxFeed {
		parts = append(parts, renderArtifactTabs(current))
	}
	if paused {
		// The box is parked in history: say so, and `end` brings it back.
		parts = append(parts, current.theme.styles.Muted.Render("↓ new"))
	}
	return strings.Join(parts, current.theme.styles.Muted.Render(" · "))
}

func mainBoxBody(current model, box mainBox, innerWidth, maxRows int) string {
	switch box {
	case boxPending:
		return pendingBody(current, innerWidth, maxRows)
	case boxFeed:
		return artifactBody(current)
	case boxLiveOutput:
		return lifecycleBody(current, innerWidth)
	case boxActivity:
		return activityBody(current, innerWidth, activityRenderLimit(current))
	}
	return ""
}

// activityRenderLimit is how many retained lines the ACTIVITY body exposes. While
// the box follows the live edge only the tail is rendered (T13 W2), but a paused
// box has to render the retained buffer: rolling a 5-line tail underneath a fixed
// offset slid the window one line per arrival, so the reader still drifted
// (review T14a-r2 f1).
func activityRenderLimit(current model) int {
	if current.boxScroll[boxActivity] > 0 {
		return 0
	}
	return activityTailLimit(current)
}

// mainBoxWantRows is the height a box asks for. It is the body's row count except
// for a paused ACTIVITY, which renders its whole retained buffer: letting that
// body set the height would grow the box (and shrink FEED) the moment the reader
// scrolled back (review T14a-r2 f1).
// The share caps bound the *final box height*, so chrome has to come out of the
// content budget here — distribute adds it back. Applying the share to content rows
// alone let LIVE OUTPUT grow chrome rows past its third of the pane and take them
// from FEED (review T14c-r2 f3).
func mainBoxWantRows(
	current model,
	box mainBox,
	body string,
	total, chrome int,
) int {
	rows := countRows(body)
	share := func(limit int) int { return max(1, limit-chrome) }
	switch box {
	case boxActivity:
		rows = min(rows, activityTailLimit(current))
	case boxPending:
		rows = min(rows, share(total/2))
	case boxLiveOutput:
		// Normally a third of the pane. An expanded entry is a deliberate request to
		// read one entry in full, so the box may take half.
		limit := total / 3
		if current.expandedCursor() {
			limit = total / 2
		}
		rows = min(rows, share(limit))
	}
	return rows
}

// pausedBelow reports whether a box is scrolled away from its newest line *and*
// there is really content below the window. The `↓ new` cue must not appear when
// the offset is clamped away by short content (T14 W1).
func pausedBelow(body string, height, scroll int) bool {
	if scroll <= 0 || height <= 0 {
		return false
	}
	rows := countRows(body)
	if rows <= height {
		return false
	}
	return min(scroll, rows-height) > 0
}

func countRows(body string) int {
	if body == "" {
		return 0
	}
	return strings.Count(body, "\n") + 1
}

func indexOfBox(order []mainBox, box mainBox) int {
	for index, candidate := range order {
		if candidate == box {
			return index
		}
	}
	return -1
}

// distributeMainBoxHeights hands out rows by priority: the pending question
// first (the run is blocked on it), then the live edge, then history, and the
// artifacts absorb whatever is left. Every visible box gets its border plus at
// least one content row; the lowest-priority boxes are dropped when even that
// does not fit.
// chrome is the per-section overhead: 2 for the wide layout's box border, 1 for
// the compact layout's section title. Both layouts must budget from the same real
// row count or the bottom section is silently clipped (review round-3 f1).
// wants is each box's requested content rows, parallel to order — not the body
// itself, because a paused ACTIVITY renders more rows than it may claim
// (mainBoxWantRows, review T14a-r2 f1).
func distributeMainBoxHeights(
	order []mainBox,
	wants []int,
	total, chrome int,
) []int {
	minRows := chrome + 1 // chrome + one content row
	heights := make([]int, len(order))
	if len(order) == 0 || total < minRows {
		return heights
	}

	// Priority decides *which* boxes survive; the render order only decides where
	// they go. Trimming the order's tail instead dropped ACTIVITY (the highest
	// priority after PENDING) before FEED (the lowest) — review T14a f5.
	priority := []mainBox{boxPending, boxActivity, boxLiveOutput, boxFeed}
	capacity := total / minRows
	survivors := make(map[mainBox]bool, len(order))
	for _, box := range priority {
		if len(survivors) >= capacity {
			break
		}
		if indexOfBox(order, box) >= 0 {
			survivors[box] = true
		}
	}

	remaining := total
	for index, box := range order {
		if survivors[box] {
			heights[index] = minRows
			remaining -= minRows
		}
	}
	remaining = max(0, remaining)

	for _, box := range priority {
		index := indexOfBox(order, box)
		if index < 0 || !survivors[box] {
			continue
		}
		// The per-box share is applied by mainBoxWantRows, which knows whether the
		// cursor is expanded; this loop only distributes what was asked for.
		want := max(minRows, wants[index]+chrome)
		grow := min(remaining, max(0, want-heights[index]))
		heights[index] += grow
		remaining -= grow
	}
	if index := indexOfBox(order, boxFeed); index >= 0 && survivors[boxFeed] {
		heights[index] += remaining
		return heights
	}
	// FEED was dropped, so the slack goes to the highest-priority survivor.
	for _, box := range priority {
		if survivors[box] {
			heights[indexOfBox(order, box)] += remaining
			break
		}
	}
	return heights
}

func renderFeed(current model, innerWidth int) string {
	var content strings.Builder
	content.WriteString(
		current.theme.styles.SectionTitle.Render("╱╱╱ PIPELINE FEED"),
	)
	content.WriteString("\n")

	if current.artifactRenderErr != nil {
		content.WriteString(
			current.theme.styles.PhaseError.Render(
				"× " + current.artifactRenderErr.Error(),
			),
		)
		content.WriteString("\n")
	} else if current.artifactRender != "" {
		content.WriteString(current.artifactRender)
	}

	// A blank line before each section title: the artifact body runs right up
	// against the next heading otherwise (live-smoke finding, 2026-07-25).
	content.WriteString("\n")
	content.WriteString(
		current.theme.styles.SectionTitle.Render("╱╱╱ LIVE OUTPUT"),
	)
	content.WriteString("\n")
	if len(current.logs) == 0 {
		content.WriteString(
			current.theme.styles.Muted.Render(
				"⋯ Waiting for the first pipeline step",
			),
		)
		content.WriteString("\n")
	} else {
		for _, line := range current.logs {
			content.WriteString(
				renderLogLine(current.theme, line, innerWidth, false),
			)
			content.WriteString("\n")
		}
	}
	return content.String()
}

// renderActivityTail is the fixed pane under the scrolling feed: only the
// newest subprocess lines of the current step (plan T13 W2). Subprocess output
// no longer accumulates in the lifecycle feed, so this is where "what is it
// doing right now" shows up. Returns "" when there is nothing to show.
func renderActivityTail(current model, innerWidth, limit int) string {
	body := activityBody(current, innerWidth, limit)
	if body == "" {
		return ""
	}
	return "\n" +
		current.theme.styles.SectionTitle.Render("╱╱╱ ACTIVITY") + "\n" +
		body
}

// renderActivityWaiting fills the content pane while a step is running but has
// produced no output yet. Leaving the largest pane blank made a healthy run look
// stuck: the only motion was the sidebar clock and the status-bar spinner, both
// at the screen edges. This puts the spinner, the role/CLI actually running, its
// elapsed time, and where the full log lives into the content area itself
// (live-smoke finding, 2026-07-25).
func activityWaitingBody(current model, innerWidth int) string {
	descriptor := activeDescriptor(current)
	if descriptor == "" {
		return ""
	}
	headline := current.spinner.View() + " " +
		current.theme.styles.Value.Render(descriptor) +
		current.theme.styles.Muted.Render(" — waiting for the first line")
	lines := []string{ansi.TruncateWc(headline, max(1, innerWidth), "…")}
	if current.hasStatus && current.status.RunID != "" {
		lines = append(lines, current.theme.styles.Muted.Render(ansi.TruncateWc(
			"  full log: .coterix/runs/"+current.status.RunID+"/logs/",
			max(1, innerWidth),
			"…",
		)))
	}
	return strings.Join(lines, "\n")
}

// renderPendingBox renders the pending question in the *main* pane (T14 W1).
// It used to live in the sidebar, where a ~30-cell width turned a paragraph-long
// question into an unreadable vertical ribbon — the sidebar now keeps only the
// `? PENDING · kind` signal. Returns "" when nothing is pending.
func renderPendingBox(current model, innerWidth, maxRows int) string {
	// Box chrome costs 2 rows and 4 cells; wrap to what is left.
	body := pendingBody(current, max(1, innerWidth-4), max(1, maxRows-2))
	if body == "" {
		return ""
	}
	return renderSidebarCard(
		current.theme,
		mainBoxTitle(current, boxPending),
		body,
		innerWidth,
	)
}

// activityTailLimit is the render-time truncation width: the compact layout
// gets fewer rows, and a failed step keeps the whole buffer so the failure
// body is not cut off (plan T13 W2 · R4).
func activityTailLimit(current model) int {
	if current.activityFailed {
		return maxActivityLines
	}
	if !isWide(current.width, current.height) {
		return activityTailCompact
	}
	return activityTailWide
}

// activeDescriptor names what is running right now — `role · cli · elapsed`.
// Empty when nothing is active. Shared by the card header and the activity
// waiting state so both answer "what is it doing" the same way.
func activeDescriptor(current model) string {
	label := current.activeRole
	if label == "" {
		label = current.activeStep
	}
	if label == "" {
		return ""
	}
	if current.activeCLI != "" {
		label += " · " + current.activeCLI
	}
	for _, row := range deriveStepper(current) {
		if row.State != stepperActive {
			continue
		}
		if elapsed := formatStageDuration(row.Duration); elapsed != "" {
			label += " · " + elapsed
		}
		break
	}
	return label
}

// renderMainHeader is the feed card's command line: what this dashboard is
// running on the left, what is running *right now* on the right.
//
// The right slot used to be a fixed `● real-time` chip whose text never changed
// — only its color did. During a multi-minute step that told the user nothing,
// and the artifact body pushed every live signal off to the screen edges. Naming
// the active role/CLI and its elapsed time here keeps it visible regardless of
// scroll position or how large the artifacts grow (live-smoke finding, 2026-07-25).
func renderMainHeader(current model, width int) string {
	right := current.theme.styles.Muted.Render("● idle")
	if current.isWorking() {
		if descriptor := activeDescriptor(current); descriptor != "" {
			right = current.spinner.View() + " " +
				current.theme.styles.PhaseBusy.Render(descriptor)
		} else {
			right = current.spinner.View() + " " +
				current.theme.styles.PhaseBusy.Render("working")
		}
	}
	// The request has to yield whatever the live signal needs, not a fixed 24 —
	// and the `coterix run ` prefix is part of the left side's width, so it comes
	// out of the request budget too. Omitting it truncated the descriptor's CLI
	// and elapsed time on long requests (review T13a-1-followup f2).
	const headerPrefix = "coterix run "
	requestWidth := max(
		1,
		width-ansi.StringWidth(right)-ansi.StringWidth(headerPrefix)-2,
	)
	left := current.theme.styles.Secondary.Bold(true).Render(headerPrefix) +
		current.theme.styles.Value.Render(
			ansi.TruncateWc(
				strings.Join(strings.Fields(current.request), " "),
				requestWidth,
				"…",
			),
		)
	return alignStatusLine(left, right, max(1, width))
}

// markdownArtifacts returns only the selected tab's artifacts: the FEED box shows
// one kind at a time instead of stacking all three (T14 W3).
func markdownArtifacts(data artifactData, tab artifactTab) []markdownArtifact {
	switch tab {
	case tabPlan:
		if strings.TrimSpace(data.PlanMarkdown) != "" {
			return []markdownArtifact{{
				Title:   "Plan",
				Content: data.PlanMarkdown,
			}}
		}
	case tabDiff:
		if data.DiffContent != nil && strings.TrimSpace(*data.DiffContent) != "" {
			return []markdownArtifact{{
				Title:    "Current diff",
				Content:  *data.DiffContent,
				Language: "diff",
			}}
		}
	case tabVerdict:
		artifacts := make([]markdownArtifact, 0, len(data.Verdicts))
		for _, verdict := range data.Verdicts {
			artifacts = append(artifacts, markdownArtifact{
				Title:    "Verdict · " + verdict.Name,
				Content:  verdict.JSON,
				Language: "json",
			})
		}
		return artifacts
	}
	return nil
}

var artifactTabLabels = [artifactTabCount]string{"Plan", "Diff", "Verdict"}

// renderArtifactTabs draws the FEED box's tab strip. The active tab is Primary and
// bold — bold is the non-color half of the cue — and a tab with nothing behind it
// is wrapped in parentheses so "empty" does not rely on the Muted colour alone
// (color-system.md: never state by colour alone). T14 W3.
func renderArtifactTabs(current model) string {
	parts := make([]string, 0, artifactTabCount)
	for tab := artifactTab(0); tab < artifactTabCount; tab++ {
		label := fmt.Sprintf("%d %s", tab+1, artifactTabLabels[tab])
		if len(markdownArtifacts(current.artifacts, tab)) == 0 {
			label = "(" + label + ")"
		}
		style := current.theme.styles.Muted
		if tab == current.artifactTab {
			style = current.theme.styles.TabActive
		}
		parts = append(parts, style.Render(label))
	}
	return strings.Join(
		parts,
		current.theme.styles.Muted.Render(" · "),
	)
}

// renderLogLine renders one columnar feed row:
// `HH:MM:SS TAG    icon role#attempt message`. The tag column carries the
// cross-model color (claude/codex); role and attempt stay in the message
// prefix so no information from the old bracket prefix is lost.
// muteStream suppresses stream-based error styling. Activity-tail lines are
// progress output, not diagnostics: codex writes its progress to stderr, so
// treating stderr as failure paints a perfectly healthy step entirely red. This
// is what R5 anticipated — the earlier "stderr is empty" measurement only
// covered claude (live-smoke finding, 2026-07-25). Real failures still surface
// as the lifecycle feed's `× … exited N` entries, which carry an explicit icon.
func renderLogLine(
	currentTheme theme,
	line logEntry,
	width int,
	muteStream bool,
) string {
	timestamp := strings.Repeat(" ", 8)
	if !line.At.IsZero() {
		timestamp = line.At.Format("15:04:05")
	}
	timeColumn := currentTheme.styles.Muted.Render(timestamp)
	tagColumn := renderLogTag(currentTheme, line)
	iconColumn := renderLogIcon(currentTheme, line, muteStream)

	label := line.Role
	if label == "" {
		label = line.Step
	}
	if label == "" {
		label = "process"
	}
	if line.Attempt > 0 {
		label += "#" + strconv.Itoa(line.Attempt)
	}
	// The `role#n ·` prefix marks the meta/message boundary (plan §3); the
	// columns themselves stay space-aligned like the north-star mock-up.
	meta := currentTheme.styles.Label.Render(label + " ·")

	text := remapANSI16(line.Text, currentTheme.tokens.ANSI)
	if line.Stream == runner.StreamStderr && !muteStream {
		text = currentTheme.styles.PhaseError.Render(text)
	} else {
		text = currentTheme.styles.Value.Render(text)
	}
	return ansi.TruncateWc(
		timeColumn+" "+tagColumn+" "+iconColumn+" "+meta+" "+text,
		max(1, width),
		"…",
	)
}

func renderLogTag(currentTheme theme, line logEntry) string {
	tag := strings.ToUpper(strings.TrimSpace(line.CLI))
	style := currentTheme.styles.Muted
	switch strings.ToLower(strings.TrimSpace(line.CLI)) {
	case "claude":
		style = currentTheme.styles.Claude
	case "codex":
		style = currentTheme.styles.Codex
	case "coterix", "":
		// The tag column names the *source*, so the harness gets a source token
		// too. `INFO` was severity vocabulary and clashed with the icon column —
		// a failed row read `INFO × … failed` (T14 W11).
		tag = "CTRX"
	}
	tag = ansi.TruncateWc(tag, logTagCellWidth, "…")
	if pad := logTagCellWidth - ansi.StringWidth(tag); pad > 0 {
		tag += strings.Repeat(" ", pad)
	}
	return style.Render(tag)
}

func renderLogIcon(currentTheme theme, line logEntry, muteStream bool) string {
	switch line.Icon {
	case logIconStart:
		return currentTheme.styles.PhaseInfo.Render("▸")
	case logIconDone:
		return currentTheme.styles.PhaseSuccess.Render("✓")
	case logIconFail:
		return currentTheme.styles.PhaseError.Render("×")
	default:
		if line.Stream == runner.StreamStderr && !muteStream {
			return currentTheme.styles.PhaseError.Render("×")
		}
		return currentTheme.styles.Muted.Render("·")
	}
}

func renderSidebar(current model, width, height int) string {
	cardWidth := max(8, width-2)
	innerWidth := max(1, cardWidth-4)
	// The sidebar is part of the focus cycle, so it takes the focused chrome too
	// (T14 W2 · review T14a f3).
	focused := isWide(current.width, current.height) &&
		current.normalizedFocus() == boxSidebar
	pipelineCard := renderBoxCard(
		current.theme,
		"PIPELINE",
		"",
		renderStepper(current.theme, deriveStepper(current), innerWidth),
		cardWidth,
		focused,
	)
	statusCard := renderBoxCard(
		current.theme,
		"STATUS",
		"",
		renderSidebarBody(
			current.theme,
			deriveSidebar(current),
			innerWidth,
			true,
			false, // the main pane's PENDING box owns the question body
		),
		cardWidth,
		focused,
	)
	content := pipelineCard + "\n" + statusCard
	indented := make([]string, 0, strings.Count(content, "\n")+1)
	for _, line := range strings.Split(content, "\n") {
		indented = append(indented, " "+line)
	}
	return strings.Join(indented[:min(len(indented), max(1, height))], "\n")
}

// showPendingPrompt controls whether the question *body* is rendered here. The
// live dashboard passes false because its main pane owns the PENDING box (T14
// W1); the one-shot snapshot passes true because it has no main pane and would
// otherwise lose the prompt entirely.
func renderSidebarBody(
	currentTheme theme,
	data sidebarData,
	innerWidth int,
	showActionHints bool,
	showPendingPrompt bool,
) string {
	innerWidth = max(1, innerWidth)
	var content strings.Builder
	content.WriteString(renderSidebarSectionTitle(
		currentTheme,
		"RUN",
		innerWidth,
	))
	content.WriteString("\n")
	writeSidebarField(
		&content,
		currentTheme,
		"run",
		ansi.TruncateWc(data.RunID, innerWidth, "…"),
		innerWidth,
	)
	writeSidebarStyledField(
		&content,
		currentTheme,
		"",
		phaseValue(currentTheme, data.Phase),
		innerWidth,
	)
	content.WriteString(renderSidebarSectionTitle(
		currentTheme,
		"ROUTING",
		innerWidth,
	))
	content.WriteString("\n")
	writeSidebarStyledField(
		&content,
		currentTheme,
		"",
		cliRoleStyle(
			currentTheme,
			data.CLI,
			currentTheme.styles.Value,
		).Render(data.Role+" → "+data.CLI),
		innerWidth,
	)
	content.WriteString(renderSidebarSectionTitle(
		currentTheme,
		"TASK",
		innerWidth,
	))
	content.WriteString("\n")
	task := data.TaskID
	if string(data.TaskStatus) != "" {
		task += " · " + string(data.TaskStatus)
	}
	task += " · #" + strconv.Itoa(data.Attempt)
	writeSidebarField(
		&content,
		currentTheme,
		"task",
		task,
		innerWidth,
	)
	writeSidebarStyledField(
		&content,
		currentTheme,
		"gate / review",
		outcomeIcon(currentTheme, data.Gate)+"  "+
			outcomeIcon(currentTheme, data.Review),
		innerWidth,
	)
	writeSidebarStyledField(
		&content,
		currentTheme,
		"progress",
		renderProgressValue(currentTheme, data, innerWidth),
		innerWidth,
	)

	if data.AwaitingApproval {
		content.WriteString(
			currentTheme.styles.PhaseWarning.Render("! APPROVAL NEEDED"),
		)
		content.WriteString("\n")
		if showActionHints {
			content.WriteString(
				currentTheme.styles.PhaseWarning.Render("a approve · r reject"),
			)
			content.WriteString("\n")
		}
	} else if data.PendingKind != "" {
		content.WriteString(
			currentTheme.styles.PhaseWarning.Render(
				pendingChipText(data.PendingKind),
			),
		)
		content.WriteString("\n")
		// On the live dashboard this is a signal only — at ~30 cells the body
		// wrapped into an unreadable vertical ribbon and ate the 26-row budget,
		// so the main pane's PENDING box renders it instead (T14 W1).
		if showPendingPrompt {
			content.WriteString(
				currentTheme.styles.PhaseWarning.Render(
					ansi.HardwrapWc(data.PendingPrompt, innerWidth, false),
				),
			)
			content.WriteString("\n")
		}
	}
	if data.LastError != "" {
		content.WriteString(
			currentTheme.styles.PhaseError.Render(
				"× " + ansi.HardwrapWc(
					data.LastError,
					max(1, innerWidth-2),
					false,
				),
			),
		)
		content.WriteString("\n")
	}

	return content.String()
}

func deriveSidebar(current model) sidebarData {
	data := sidebarData{
		RunID:       "starting…",
		Phase:       state.PhasePlanning,
		Role:        "—",
		CLI:         "—",
		TaskID:      "—",
		TaskStatus:  state.TaskOpen,
		Gate:        current.artifacts.GateOutcome,
		Review:      current.artifacts.ReviewOutcome,
		PendingKind: "",
	}
	if current.activeRole != "" || current.activeStep != "" {
		data.Role = current.activeRole
		if data.Role == "" {
			data.Role = current.activeStep
		}
		data.CLI = current.activeCLI
	}
	if !current.hasStatus {
		return data
	}

	statusData := deriveStatusFields(current.status)
	statusGate := statusData.Gate
	statusReview := statusData.Review
	statusData.Gate = current.artifacts.GateOutcome
	statusData.Review = current.artifacts.ReviewOutcome
	if statusGate == evidencePass {
		statusData.Gate = statusGate
		statusData.Review = statusReview
	}
	if current.activeRole != "" || current.activeStep != "" {
		statusData.Role = current.activeRole
		if statusData.Role == "" {
			statusData.Role = current.activeStep
		}
		statusData.CLI = current.activeCLI
	}
	if current.activeStep == "" {
		switch {
		case statusData.AwaitingApproval:
			statusData.Role = "human_gate"
			statusData.CLI = "operator"
		case statusData.PendingKind != "":
			statusData.Role = "pending_action"
			statusData.CLI = "operator"
		case current.status.Phase == state.PhaseDone ||
			current.status.Phase == state.PhaseFailed:
			statusData.Role = "—"
			statusData.CLI = "—"
		}
	}
	return statusData
}

func deriveStatusFields(status pipeline.RunStatus) sidebarData {
	data := sidebarData{
		RunID:       status.RunID,
		Phase:       status.Phase,
		Role:        "—",
		CLI:         "—",
		TaskID:      "—",
		TaskStatus:  state.TaskOpen,
		Gate:        evidenceUnknown,
		Review:      evidenceUnknown,
		PlanRound:   status.PlanRound,
		Total:       len(status.TaskOrder),
		PendingKind: "",
	}
	for _, taskID := range status.TaskOrder {
		if task, exists := status.Tasks[taskID]; exists &&
			task.Status == state.TaskConfirmed {
			data.Confirmed++
		}
	}
	if status.CurrentTaskID != nil {
		data.TaskID = *status.CurrentTaskID
		if task, exists := status.Tasks[*status.CurrentTaskID]; exists {
			data.TaskStatus = task.Status
			data.Attempt = task.Attempt
			if task.Status == state.TaskConfirmed {
				data.Gate = evidencePass
				data.Review = evidencePass
			}
		}
	}
	data.AwaitingApproval = status.Phase == state.PhaseAwaitingApproval
	if status.PendingAction != nil {
		data.PendingKind = status.PendingAction.Kind
		data.PendingPrompt = status.PendingAction.Prompt
	}
	if status.LastError != nil {
		data.LastError = *status.LastError
	}
	switch {
	case data.AwaitingApproval:
		data.Role = "human_gate"
		data.CLI = "operator"
	case data.PendingKind != "":
		data.Role = "pending_action"
		data.CLI = "operator"
	}
	return data
}

// renderProgressValue draws the task progress as a block bar *inside* the existing
// progress row — the sidebar's row budget does not grow (T14 W7 · R2). The counts
// come first: when the rail is too narrow for a bar, the bar is what goes, because
// `round 3 · 2/5` still answers the question and a clipped bar does not.
func renderProgressValue(
	currentTheme theme,
	data sidebarData,
	innerWidth int,
) string {
	// "▌ " + "progress: " is the row's chrome; whatever is left holds the counts and,
	// only if there is still room, the bar.
	const rowChrome = 2 + len("progress: ")
	available := innerWidth - rowChrome
	counts := fmt.Sprintf(
		"round %d · %d/%d",
		data.PlanRound,
		data.Confirmed,
		data.Total,
	)
	spare := available - ansi.StringWidth(counts) - 1
	// No plan yet means no proportion to draw. The width guard is what keeps
	// strings.Repeat from being handed a negative count; the sidebar rail is a fixed
	// 32 cells, so it is a safety floor rather than a layout the user will meet.
	if data.Total <= 0 || spare < 2 {
		return currentTheme.styles.Value.Render(counts)
	}
	cells := min(spare, 10)
	filled := data.Confirmed * cells / data.Total
	filled = min(max(filled, 0), cells)
	bar := currentTheme.styles.PhaseSuccess.Render(strings.Repeat("■", filled)) +
		currentTheme.styles.Muted.Render(strings.Repeat("□", cells-filled))
	return bar + " " + currentTheme.styles.Value.Render(counts)
}

func writeSidebarField(
	builder *strings.Builder,
	currentTheme theme,
	label string,
	value string,
	width int,
) {
	if value == "" {
		value = "—"
	}
	writeSidebarStyledField(
		builder,
		currentTheme,
		label,
		currentTheme.styles.Value.Render(value),
		width,
	)
}

func writeSidebarStyledField(
	builder *strings.Builder,
	currentTheme theme,
	label string,
	value string,
	width int,
) {
	prefix := currentTheme.styles.SectionTitle.Render("▌") + " "
	if label != "" {
		prefix += currentTheme.styles.Label.Render(label + ": ")
	}
	builder.WriteString(prefix)
	available := max(1, width-ansi.StringWidth(prefix))
	builder.WriteString(ansi.TruncateWc(value, available, "…"))
	builder.WriteString("\n")
}

func phaseValue(currentTheme theme, phase state.Phase) string {
	return phaseDot(currentTheme, phase) + " " +
		currentTheme.styles.Value.Render(string(phase))
}

func phaseDot(currentTheme theme, phase state.Phase) string {
	switch phase {
	case state.PhaseDone:
		return currentTheme.styles.PhaseSuccess.Render("●")
	case state.PhaseFailed:
		return currentTheme.styles.PhaseError.Render("●")
	case state.PhaseAwaitingApproval, state.PhasePausedForInput:
		return currentTheme.styles.PhaseWarning.Render("●")
	default:
		return currentTheme.styles.PhaseInfo.Render("●")
	}
}

func outcomeIcon(currentTheme theme, outcome evidenceOutcome) string {
	switch outcome {
	case evidencePass:
		return currentTheme.styles.PhaseSuccess.Render("✓")
	case evidenceFail:
		return currentTheme.styles.PhaseError.Render("×")
	default:
		return currentTheme.styles.PhaseInfo.Render("⋯")
	}
}

func renderCompactHeader(current model, width, height int) string {
	data := deriveSidebar(current)
	task := data.TaskID
	if task == "" {
		task = "—"
	}
	left := gradientText(current.theme, "COTERIX")
	middle := phaseValue(current.theme, data.Phase)
	right := current.theme.styles.Value.Render(
		fmt.Sprintf("%s · %d/%d", task, data.Confirmed, data.Total),
	)
	line := ansi.TruncateWc(
		left+"  "+middle+"  "+right,
		max(1, width-2),
		"…",
	)
	second := current.theme.styles.Muted.Render(
		ansi.TruncateWc(
			data.Role+" → "+data.CLI+
				" · gate "+plainOutcome(data.Gate)+
				" · review "+plainOutcome(data.Review),
			max(1, width-2),
			"…",
		),
	)
	return current.theme.styles.Header.
		Width(max(1, width-2)).
		Height(max(1, height)).
		Padding(0, 1).
		Render(line + "\n" + second)
}

// renderTaskCapChoices draws the task_cap answer as a pick instead of something to
// type (T14 W5). The chosen option carries `▸` and bold as well as the Primary
// colour, so the choice survives a terminal that drops colour.
func renderTaskCapChoices(current model, budget int) string {
	parts := make([]string, 0, len(taskCapChoices))
	for _, choice := range taskCapChoices {
		if choice == current.promptValue {
			parts = append(parts, current.theme.styles.TabActive.Render("▸ "+choice))
			continue
		}
		parts = append(parts, current.theme.styles.Muted.Render("  "+choice))
	}
	return ansi.TruncateWc(strings.Join(parts, " "), max(1, budget), "…")
}

func renderStatusBar(current model, width, height int) string {
	innerWidth := max(1, width-2)
	if current.prompt != promptNone {
		label := "Reject feedback"
		switch {
		case current.prompt == promptApproveConfirm:
			label = "Approve this plan?"
		case current.prompt == promptResume:
			kind := ""
			if current.status.PendingAction != nil {
				kind = string(current.status.PendingAction.Kind)
			}
			label = "Response · " + kind
		}
		// At very narrow widths the label alone can exceed the row, which wraps
		// and pushes the footer out of the region (review round-3 f2).
		label = ansi.TruncateWc(label, max(1, innerWidth-8), "…")
		// The chrome that shares the row must all come out of the budget, or the
		// input wraps and pushes the footer out of the prompt region (review
		// T13a-1-followup f1): StatusBar padding, the `label: ` suffix, and the
		// frame. Measure the label in cells — len() counts bytes and it holds `·`.
		const promptChrome = 2 + // StatusBar Padding(0, 1)
			2 + // ": " after the label
			2 + // input frame, both sides
			1 // trailing cell
		budget := max(1, innerWidth-ansi.StringWidth(label)-promptChrome)
		heading := current.theme.styles.Label.Render(label + ": ")
		var input string
		switch {
		case current.usesTextarea():
			// The editor owns its own rows, so it goes on the line below the label
			// instead of sharing one (T14 W5).
			area := current.promptArea
			area.SetWidth(max(4, innerWidth-2))
			input = area.View()
			heading = current.theme.styles.Label.Render(label) + "\n"
		case current.prompt == promptApproveConfirm:
			// Nothing to type — the footer names the two keys.
			input = current.theme.styles.Muted.Render("(no response needed)")
		default: // task_cap — the only remaining single-row input.
			input = renderTaskCapChoices(current, budget)
		}
		footerText := "enter confirm · esc cancel"
		switch {
		case current.usesTextarea():
			footerText = "enter submit · ctrl+j newline · esc cancel"
		case current.isTaskCapPrompt():
			footerText = "←/→ choose · enter submit · esc cancel"
		}
		footerStyle := current.theme.styles.Hint
		if current.promptError != "" {
			footerText = "× " + current.promptError
			footerStyle = current.theme.styles.PhaseError
		}
		// A long validation error (e.g. the task_cap message) wrapped to a second
		// row and pushed the last line out of the 4-row prompt region, so the
		// footer is clamped to one row (review round-3 f2).
		footer := footerStyle.Render(
			ansi.TruncateWc(footerText, max(1, innerWidth-2), "…"),
		)
		return current.theme.styles.StatusBar.
			Width(innerWidth).
			Height(max(1, height)).
			Padding(0, 1).
			Render(heading + input + "\n" + footer)
	}

	primary := statusSignal(current)
	contentWidth := max(1, innerWidth-2)
	// The hint line is derived from the same table as the `?` overlay, so a key
	// cannot be advertised in one and missing from the other (T14 W6).
	hints := keyHintLine(current)
	content := alignStatusLine(
		primary,
		current.theme.styles.Hint.Render(hints),
		contentWidth,
	)
	if detail := statusDetail(current, contentWidth); detail != "" {
		content += "\n" + detail
	}
	return current.theme.styles.StatusBar.
		Width(innerWidth).
		Height(max(1, height)).
		Padding(0, 1).
		Render(content)
}

func statusSignal(current model) string {
	if current.stopping {
		return statusChip(
			current.theme.styles.Warning,
			"⟳ STOPPING SAFELY",
		)
	}
	if current.isWorking() {
		step := current.activeRole
		if step == "" {
			step = current.activeStep
		}
		return workingStatusChip(
			current.theme,
			current.spinner.View()+workingGradientText(
				current.theme,
				" "+strings.ToUpper(step)+" WORKING",
			),
		)
	}
	if !current.hasStatus {
		return statusChip(
			current.theme.styles.Info,
			"◇ STARTING PIPELINE",
		)
	}
	if current.status.Phase == state.PhaseAwaitingApproval {
		return statusChip(
			current.theme.styles.Warning,
			"! APPROVAL NEEDED",
		)
	}
	if current.status.PendingAction != nil {
		return statusChip(
			current.theme.styles.Warning,
			pendingChipText(current.status.PendingAction.Kind),
		)
	}
	if current.operationErr != nil {
		return statusChip(
			current.theme.styles.Error,
			"× OPERATION FAILED",
		)
	}
	return statusChip(
		phaseChipStyle(current.theme, current.status.Phase),
		"● "+strings.ToUpper(
			strings.ReplaceAll(string(current.status.Phase), "_", " "),
		),
	)
}

// renderTopBar draws the brand bar from the north-star mock-up: gradient
// wordmark on the left, one orchestration-activity signal in the middle, the
// run identity on the right, and a gradient underline under the wordmark.
func renderTopBar(current model, width, height int) string {
	innerWidth := max(1, width-2)
	contentWidth := max(1, innerWidth-2)
	wordmark := gradientText(current.theme, wordmarkText)

	activity := current.theme.styles.Muted.Render("● orchestration idle")
	if current.isWorking() {
		activity = current.theme.styles.PhaseBusy.Render(
			"● orchestration active",
		)
	}

	runID := "starting…"
	if current.hasStatus && current.status.RunID != "" {
		runID = current.status.RunID
	}
	identity := current.theme.styles.Label.Render("run: ") +
		current.theme.styles.Value.Render(
			ansi.TruncateWc(runID, max(1, contentWidth/3), "…"),
		)

	first := alignTriple(wordmark, activity, identity, contentWidth)
	wordmarkWidth := ansi.StringWidth(wordmarkText)
	underline := gradientText(
		current.theme,
		strings.Repeat("─", wordmarkWidth),
	)
	if rest := contentWidth - wordmarkWidth; rest > 0 {
		underline += current.theme.styles.Separator.Render(
			strings.Repeat("─", rest),
		)
	}
	return current.theme.styles.Header.
		Width(innerWidth).
		Height(max(1, height)).
		Padding(0, 1).
		Render(first + "\n" + underline)
}

// alignTriple lays out left, center, and right segments on one line. When the
// budget is tight the center yields first, then the right.
func alignTriple(left, center, right string, width int) string {
	if width <= 0 {
		return ""
	}
	leftWidth := ansi.StringWidth(left)
	rightWidth := ansi.StringWidth(right)
	centerWidth := ansi.StringWidth(center)
	if leftWidth+centerWidth+rightWidth+2 > width {
		return alignStatusLine(left, right, width)
	}
	centerStart := max(leftWidth+1, (width-centerWidth)/2)
	if centerStart+centerWidth+1+rightWidth > width {
		centerStart = width - rightWidth - 1 - centerWidth
	}
	rightStart := width - rightWidth
	return left +
		strings.Repeat(" ", centerStart-leftWidth) +
		center +
		strings.Repeat(" ", max(1, rightStart-centerStart-centerWidth)) +
		right
}

func renderSidebarSectionTitle(
	currentTheme theme,
	label string,
	width int,
) string {
	title := brandEndpointGradient(currentTheme, "╱╱╱") + " " +
		currentTheme.styles.SectionTitle.Render(label)
	remaining := width - ansi.StringWidth(title) - 1
	if remaining <= 0 {
		return ansi.TruncateWc(title, max(1, width), "…")
	}
	return title + " " + currentTheme.styles.Separator.Render(
		strings.Repeat("─", remaining),
	)
}

func cliRoleStyle(
	currentTheme theme,
	cliName string,
	fallback lipgloss.Style,
) lipgloss.Style {
	switch strings.ToLower(strings.TrimSpace(cliName)) {
	case "claude":
		return currentTheme.styles.Claude
	case "codex":
		return currentTheme.styles.Codex
	default:
		return fallback
	}
}

func statusDetail(current model, width int) string {
	if width <= 0 {
		return ""
	}
	parts := make([]string, 0, 2)
	if current.hasStatus && current.status.PendingAction != nil {
		if prompt := strings.TrimSpace(current.status.PendingAction.Prompt); prompt != "" {
			parts = append(
				parts,
				current.theme.styles.PhaseWarning.Render(prompt),
			)
		}
	}
	if current.hasStatus && current.status.LastError != nil {
		if message := strings.TrimSpace(*current.status.LastError); message != "" {
			parts = append(
				parts,
				current.theme.styles.PhaseError.Render("× "+message),
			)
		}
	} else if current.operationErr != nil {
		parts = append(
			parts,
			current.theme.styles.PhaseError.Render(
				"× "+current.operationErr.Error(),
			),
		)
	}
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		return ansi.TruncateWc(parts[0], width, "…")
	}
	separator := " · "
	available := width - ansi.StringWidth(separator)
	if available < 2 {
		return ansi.TruncateWc(strings.Join(parts, separator), width, "…")
	}
	leftWidth := available / 2
	rightWidth := available - leftWidth
	return ansi.TruncateWc(parts[0], leftWidth, "…") +
		separator +
		ansi.TruncateWc(parts[1], rightWidth, "…")
}

func alignStatusLine(left, right string, width int) string {
	if width <= 0 {
		return ""
	}
	leftWidth := ansi.StringWidth(left)
	if leftWidth >= width {
		return ansi.TruncateWc(left, width, "…")
	}
	available := width - leftWidth - 1
	if available <= 0 {
		return left
	}
	right = ansi.TruncateWc(right, available, "…")
	padding := max(1, width-leftWidth-ansi.StringWidth(right))
	return left + strings.Repeat(" ", padding) + right
}

func statusChip(style lipgloss.Style, content string) string {
	return style.Bold(true).Padding(0, 1).Render(content)
}

func workingStatusChip(currentTheme theme, content string) string {
	padding := currentTheme.styles.Busy.Render(" ")
	return padding + content + padding
}

func phaseChipStyle(currentTheme theme, phase state.Phase) lipgloss.Style {
	switch phase {
	case state.PhaseAwaitingApproval, state.PhasePausedForInput:
		return currentTheme.styles.Warning
	case state.PhaseDone:
		return currentTheme.styles.Success
	case state.PhaseFailed:
		return currentTheme.styles.Error
	default:
		return currentTheme.styles.Info
	}
}

func pendingChipText(kind state.PendingKind) string {
	switch kind {
	case state.PendingPlanQuestion:
		return "? PENDING · plan_question"
	case state.PendingPlanCap:
		return "⟳ PENDING · plan_cap"
	case state.PendingTaskCap:
		return "▶ PENDING · task_cap"
	case state.PendingAuth:
		return "◇ PENDING · auth"
	default:
		return "! PENDING"
	}
}

func gradientText(currentTheme theme, text string) string {
	runes := []rune(text)
	if len(runes) == 0 {
		return ""
	}
	stops := make([]color.Color, 0, len(currentTheme.tokens.Gradient.BrandLeftToRight))
	for _, token := range currentTheme.tokens.Gradient.BrandLeftToRight {
		stops = append(stops, lipgloss.Color(token))
	}
	colors := lipgloss.Blend1D(len(runes), stops...)
	if len(colors) != len(runes) {
		return currentTheme.styles.Logo.Render(text)
	}
	var output strings.Builder
	for index, character := range runes {
		output.WriteString(
			lipgloss.NewStyle().
				Foreground(colors[index]).
				Bold(true).
				Render(string(character)),
		)
	}
	return output.String()
}

func workingGradientText(currentTheme theme, text string) string {
	runes := []rune(text)
	if len(runes) == 0 {
		return ""
	}
	stops := make(
		[]color.Color,
		0,
		len(currentTheme.tokens.Gradient.BrandLeftToRight),
	)
	for _, token := range currentTheme.tokens.Gradient.BrandLeftToRight {
		stops = append(stops, lipgloss.Color(token))
	}
	colors := lipgloss.Blend1D(len(runes), stops...)
	if len(colors) != len(runes) {
		return currentTheme.styles.Busy.Render(text)
	}
	background := lipgloss.Color(currentTheme.tokens.Status.Busy.BG)
	var output strings.Builder
	for index, character := range runes {
		output.WriteString(
			lipgloss.NewStyle().
				Foreground(colors[index]).
				Background(background).
				Bold(true).
				Render(string(character)),
		)
	}
	return output.String()
}

func brandEndpointGradient(currentTheme theme, text string) string {
	runes := []rune(text)
	stops := currentTheme.tokens.Gradient.BrandLeftToRight
	if len(runes) == 0 || len(stops) == 0 {
		return ""
	}
	colors := lipgloss.Blend1D(
		len(runes),
		lipgloss.Color(stops[0]),
		lipgloss.Color(stops[len(stops)-1]),
	)
	if len(colors) != len(runes) {
		return currentTheme.styles.SectionTitle.Render(text)
	}
	var output strings.Builder
	for index, character := range runes {
		output.WriteString(
			lipgloss.NewStyle().
				Foreground(colors[index]).
				Bold(true).
				Render(string(character)),
		)
	}
	return output.String()
}

func visibleLines(content string, height, scroll int) string {
	if height <= 0 {
		return ""
	}
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	if len(lines) <= height {
		return strings.Join(lines, "\n")
	}
	maxStart := len(lines) - height
	start := maxStart - max(0, scroll)
	if start < 0 {
		start = 0
	}
	end := min(len(lines), start+height)
	return strings.Join(lines[start:end], "\n")
}

func plainOutcome(outcome evidenceOutcome) string {
	switch outcome {
	case evidencePass:
		return "✓"
	case evidenceFail:
		return "×"
	default:
		return "⋯"
	}
}

// Keep the status type visible in this file so layout tests can construct
// snapshots without importing internal model implementation details.
var _ = pipeline.RunStatus{}
