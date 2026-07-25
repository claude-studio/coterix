package ui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/ridenow/coterix/internal/state"
)

// The key documentation lives here, in one table, because the overlay and the
// status bar's hint line are two views of the same thing. Writing them separately
// is how a key ends up documented but unbound, or bound but undocumented (T14 W6).
//
// Every entry below must correspond to a real case in updateKey/updatePromptKey.
// `y` (copy run id) and `w` (wrap the tail) are specified for T13b and are *not*
// listed: they are not dispatched yet.

type keyHelpEntry struct {
	keys string
	what string
	// short is the abbreviated form for the status bar's one-line hint. Empty means
	// the key is only documented in the overlay.
	short string
}

type keyHelpGroup struct {
	title   string
	entries []keyHelpEntry
}

func keyHelpGroups(current model) []keyHelpGroup {
	groups := []keyHelpGroup{
		{title: "MOVE", entries: []keyHelpEntry{
			{
				keys: "tab · shift+tab",
				what: "focus the next / previous box (wide layouts)",
			},
			{
				keys:  "j · k",
				what:  "scroll the focused box — move the cursor in LIVE OUTPUT",
				short: "j/k scroll",
			},
			{
				keys:  "home · end",
				what:  "jump to the oldest line / back to the live edge",
				short: "home/end",
			},
		}},
		{title: "ARTIFACTS", entries: []keyHelpEntry{
			{keys: "1 · 2 · 3", what: "show the Plan / Diff / Verdict tab"},
			{keys: "[ · ]", what: "cycle the artifact tabs"},
		}},
		{title: "ENTRIES", entries: []keyHelpEntry{
			{
				keys: "enter",
				what: "expand the selected entry and open its artifact",
			},
			{keys: "esc", what: "drop the cursor and follow the live edge again"},
		}},
	}
	if actions := keyHelpActions(current); len(actions) > 0 {
		groups = append(groups, keyHelpGroup{title: "THIS RUN", entries: actions})
	}
	return append(groups, keyHelpGroup{title: "GENERAL", entries: []keyHelpEntry{
		{keys: "?", what: "open or close this help"},
		{keys: "q · ctrl+c", what: "stop the run safely", short: "q quit"},
	}})
}

// keyHelpActions are the keys that only exist in the current phase, so the overlay
// never offers an approval that would be refused.
func keyHelpActions(current model) []keyHelpEntry {
	if !current.hasStatus || current.operation != "" {
		return nil
	}
	switch current.status.Phase {
	case state.PhaseAwaitingApproval:
		return []keyHelpEntry{
			{
				keys:  "a",
				what:  "approve — asks to confirm with enter",
				short: "a approve",
			},
			{keys: "r", what: "reject with feedback", short: "r reject"},
		}
	case state.PhasePausedForInput:
		if current.status.PendingAction != nil &&
			current.status.PendingAction.Kind == state.PendingAuth {
			return []keyHelpEntry{{
				keys:  "enter",
				what:  "resume after logging the CLI in",
				short: "enter resume after login",
			}}
		}
		return []keyHelpEntry{{
			keys:  "enter",
			what:  "answer the pending question",
			short: "enter respond",
		}}
	}
	return nil
}

// keyHintLine is the status bar's one-line summary, drawn from the same table as
// the overlay. Phase actions lead because they are what the run is waiting on.
func keyHintLine(current model) string {
	hints := make([]string, 0, 6)
	for _, group := range keyHelpGroups(current) {
		for _, entry := range group.entries {
			if entry.short != "" {
				hints = append(hints, entry.short)
			}
		}
	}
	return strings.Join(hints, " · ")
}

// renderHelpOverlay draws the single key overlay (T14 W6). It is deliberately not a
// dialog stack: `?` and `esc` both close it and nothing can open on top of it.
func renderHelpOverlay(current model, width, height int) string {
	keyWidth := 0
	for _, group := range keyHelpGroups(current) {
		for _, entry := range group.entries {
			keyWidth = max(keyWidth, ansi.StringWidth(entry.keys))
		}
	}
	// Card border 2 + padding 2 on each side.
	innerWidth := max(8, width-6)
	rows := make([]string, 0, 24)
	for index, group := range keyHelpGroups(current) {
		if index > 0 {
			rows = append(rows, "")
		}
		rows = append(rows, current.theme.styles.SectionTitle.Render(group.title))
		for _, entry := range group.entries {
			pad := strings.Repeat(
				" ",
				max(0, keyWidth-ansi.StringWidth(entry.keys)),
			)
			rows = append(rows, ansi.TruncateWc(
				current.theme.styles.TabActive.Render(entry.keys)+pad+"  "+
					current.theme.styles.Value.Render(entry.what),
				innerWidth,
				"…",
			))
		}
	}
	// The overlay must not outgrow the pane it covers.
	if limit := max(1, height-2); len(rows) > limit {
		rows = rows[:limit]
	}
	return renderBoxCard(
		current.theme,
		"KEYS",
		current.theme.styles.Muted.Render("? or esc closes"),
		strings.Join(rows, "\n"),
		width,
		true,
	)
}
