package top

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// topView is what desk top shows in its frame: the machines, or a machine's resources,
// logs, or the port forwards.
type topView interface {
	// start begins what the view keeps up to date, once it's shown.
	start(m *Model) tea.Cmd
	// close stops it, once the view is gone.
	close()
	// crumb names the view in the breadcrumbs.
	crumb() string
	// title is the frame's title.
	title(m *Model) string
	// hints are the view's keys, for the header and help.
	hints(m *Model) []topHint
	// key handles key, reporting whether it did.
	key(m *Model, key string) (tea.Cmd, bool)
	// handle handles a message other than a key, reporting whether it was the view's.
	handle(m *Model, msg tea.Msg) (tea.Cmd, bool)
	// body renders the view in width×height.
	body(m *Model, width, height int) []string
}

// topFilterable views filter what they show by what's typed after /.
type topFilterable interface {
	filter() string
	setFilter(filter string)
}

type topHint struct{ key, label string }

// Keys and names more than one view has.
const (
	keyEsc      = "esc"
	keyEnter    = "enter"
	keyCtrlC    = "ctrl+c"
	keyDown     = "down"
	hintFilter  = "Filter"
	topMachines = "machines"
	topMachine  = "machine"
)

type topStyles struct {
	text, dim, bold                lipgloss.Style
	logo, infoLabel, infoValue     lipgloss.Style
	hintKey, hintLabel             lipgloss.Style
	border, title, titleFilter     lipgloss.Style
	titleCount, header, cursor     lipgloss.Style
	crumb, crumbActive             lipgloss.Style
	flash, flashErr                lipgloss.Style
	good, warn, bad, accent, faint lipgloss.Style
}

func newTopStyles() topStyles {
	text := lipgloss.AdaptiveColor{Light: "#1C1C1C", Dark: "#E4E4E4"}
	dim := lipgloss.AdaptiveColor{Light: "#7A7A7A", Dark: "#8A8A8A"}
	faint := lipgloss.AdaptiveColor{Light: "#BDBDBD", Dark: "#444444"}
	orange := lipgloss.Color("#FFA500")
	blue := lipgloss.Color("#1E90FF")
	aqua := lipgloss.AdaptiveColor{Light: "#008B8B", Dark: "#00FFFF"}
	style := lipgloss.NewStyle
	return topStyles{
		text:        style().Foreground(text),
		dim:         style().Foreground(dim),
		bold:        style().Foreground(text).Bold(true),
		logo:        style().Foreground(orange).Bold(true),
		infoLabel:   style().Foreground(orange),
		infoValue:   style().Foreground(text).Bold(true),
		hintKey:     style().Foreground(blue).Bold(true),
		hintLabel:   style().Foreground(dim),
		border:      style().Foreground(blue),
		title:       style().Foreground(aqua).Bold(true),
		titleFilter: style().Foreground(lipgloss.Color("#EE82EE")).Bold(true),
		titleCount:  style().Foreground(text).Bold(true),
		header:      style().Foreground(text).Bold(true),
		cursor:      style().Foreground(lipgloss.Color("#000000")).Background(lipgloss.Color("#00CED1")).Bold(true),
		crumb: style().Foreground(lipgloss.Color("#000000")).Background(lipgloss.Color("#5F9EA0")).
			Padding(0, 1),
		crumbActive: style().Foreground(lipgloss.Color("#000000")).Background(orange).Bold(true).Padding(0, 1),
		flash:       style().Foreground(lipgloss.AdaptiveColor{Light: "#8B5A00", Dark: "#FFDEAD"}),
		flashErr:    style().Foreground(lipgloss.Color("#FF4500")).Bold(true),
		good:        style().Foreground(lipgloss.AdaptiveColor{Light: "#1A7F37", Dark: "#32CD32"}),
		warn:        style().Foreground(lipgloss.AdaptiveColor{Light: "#9A6700", Dark: "#FFD700"}),
		bad:         style().Foreground(lipgloss.Color("#FF4500")),
		accent:      style().Foreground(aqua),
		faint:       style().Foreground(faint),
	}
}

// topLogo is "desk", drawn in the header's corner like k9s's.
const topLogo = `     _           _
  __| | ___  ___| | __
 / _` + "`" + ` |/ _ \/ __| |/ /
| (_| |  __/\__ \   <
 \__,_|\___||___/_|\_\`

// topHeaderHeight is the header's lines: the info, the key hints and the logo side by side.
const topHeaderHeight = 6

func (m *Model) View() string {
	if m.width == 0 {
		return ""
	}
	var sections []string
	header := m.headerView()
	sections = append(sections, header)
	if m.prompt != nil {
		sections = append(sections, m.promptView())
	}
	sections = append(sections, "", "") // the crumbs and the flash line, measured below
	frameHeight := m.height - lipgloss.Height(strings.Join(sections, "\n"))
	frame := m.frameView(max(3, frameHeight))
	sections = append(sections[:len(sections)-2], frame, m.crumbsView(), m.flashView())
	return strings.Join(sections, "\n")
}

func (m *Model) headerView() string {
	machines := 0
	for _, machine := range m.machines {
		if !isEmptyMachine(machine) {
			machines++
		}
	}
	account := "not logged in"
	if m.loggedIn {
		account = "logged in"
	}
	daemon := "not running"
	if m.daemonUp {
		daemon = fmt.Sprintf("running, %d connected", len(m.connected))
	}
	info := [][2]string{
		{"Account:", account},
		{"Machines:", fmt.Sprint(machines)},
		{"Profiles:", fmt.Sprint(len(m.profiles))},
		{"Forwards:", fmt.Sprint(m.runningForwards())},
		{"Daemon:", daemon},
		{"Version:", m.version},
	}
	var infoLines []string
	for _, row := range info {
		infoLines = append(infoLines, m.styles.infoLabel.Render(fmt.Sprintf("%-10s", row[0]))+
			m.styles.infoValue.Render(row[1]))
	}
	infoBlock := lipgloss.NewStyle().PaddingLeft(1).PaddingRight(4).Render(strings.Join(infoLines, "\n"))

	logo := m.styles.logo.Render(topLogo)
	hintsWidth := m.width - lipgloss.Width(infoBlock) - lipgloss.Width(logo) - 2
	if hintsWidth < 20 {
		logo = ""
		hintsWidth = m.width - lipgloss.Width(infoBlock)
	}
	hints := m.hintsView(hintsWidth)
	gap := max(0, m.width-lipgloss.Width(infoBlock)-lipgloss.Width(hints)-lipgloss.Width(logo)-1)
	return lipgloss.JoinHorizontal(lipgloss.Top, infoBlock, hints, strings.Repeat(" ", gap), logo)
}

// hintsView lays the top view's keys, then the ones every view has, out in columns of
// topHeaderHeight rows, as many as fit in width, each column's labels lined up as k9s's are.
func (m *Model) hintsView(width int) string {
	hints := append(m.top().hints(m), topHint{"?", "Help"})
	if len(m.views) > 1 {
		hints = append(hints, topHint{keyEsc, "Back"})
	}
	var columns []string
	for start := 0; start < len(hints); start += topHeaderHeight {
		column := hints[start:min(len(hints), start+topHeaderHeight)]
		keyWidth := 0
		for _, hint := range column {
			keyWidth = max(keyWidth, len([]rune(hint.key))+2)
		}
		var lines []string
		for _, hint := range column {
			lines = append(lines, m.styles.hintKey.Render(fmt.Sprintf("%-*s", keyWidth, "<"+hint.key+">"))+" "+
				m.styles.hintLabel.Render(hint.label))
		}
		rendered := lipgloss.NewStyle().PaddingRight(3).Render(strings.Join(lines, "\n"))
		if lipgloss.Width(strings.Join(append(columns, rendered), "")) > width {
			break
		}
		columns = append(columns, rendered)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, columns...)
}

func (m *Model) promptView() string {
	prefix, placeholder := "/", "filter"
	if m.prompt.command {
		prefix, placeholder = ":", "machines, pf, quit"
	}
	text := m.styles.bold.Render(m.prompt.text) + m.styles.accent.Render("█")
	if m.prompt.text == "" {
		text = m.styles.accent.Render("█") + m.styles.faint.Render(placeholder)
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#EE82EE")).
		Width(max(1, m.width-2))
	return box.Render(m.styles.titleFilter.Render(prefix+" ") + text)
}

// frameView draws the top view, or the help, in a bordered frame titled like k9s's, with
// any dialog over it.
func (m *Model) frameView(height int) string {
	innerWidth, innerHeight := max(1, m.width-4), max(1, height-2)
	var title string
	var lines []string
	if m.help {
		title = m.styles.title.Render("Help")
		lines = m.helpLines()
	} else {
		title = m.top().title(m)
		lines = m.top().body(m, innerWidth, innerHeight)
	}
	for len(lines) < innerHeight {
		lines = append(lines, "")
	}
	lines = lines[:innerHeight]
	if m.dialog != nil {
		lines = overlay(lines, m.dialog.view(m), innerWidth)
	}

	b := m.styles.border
	title = " " + ansi.Truncate(title, max(1, m.width-8), "…") + " "
	side := max(0, m.width-2-lipgloss.Width(title))
	var out strings.Builder
	out.WriteString(b.Render("┌"+strings.Repeat("─", side/2)) + title +
		b.Render(strings.Repeat("─", side-side/2)+"┐") + "\n")
	for _, line := range lines {
		out.WriteString(b.Render("│") + " " + fit(line, innerWidth) + " " + b.Render("│") + "\n")
	}
	out.WriteString(b.Render("└" + strings.Repeat("─", max(0, m.width-2)) + "┘"))
	return out.String()
}

func (m *Model) helpLines() []string {
	section := func(name string, hints []topHint) []string {
		lines := []string{m.styles.header.Render(name)}
		for _, hint := range hints {
			lines = append(lines, "  "+m.styles.hintKey.Render(fmt.Sprintf("%-13s", "<"+hint.key+">"))+
				m.styles.text.Render(hint.label))
		}
		return append(lines, "")
	}
	lines := section(strings.ToUpper(m.top().crumb()), m.top().hints(m))
	general := []topHint{
		{":machines", "Machines and profiles"}, {":pf", "Port forwards (f on a machine)"}, {"/", hintFilter},
		{keyEsc, "Back, or clear the filter"}, {"ctrl-r", "Fetch your machines from the cloud"},
		{"q", "Quit"},
	}
	lines = append(lines, section("GENERAL", general)...)
	moving := []topHint{{"j/↓ k/↑", "Down, up"}, {"g G", "Top, bottom"}, {"pgdn pgup", "Page down, up"}}
	return append(lines, section("MOVING", moving)...)
}

// titled is a frame title like k9s's: name(filter)[count].
func (m *Model) titled(name, filter string, count int) string {
	title := m.styles.title.Render(name)
	if filter != "" {
		title += m.styles.titleFilter.Render("(/" + filter + ")")
	}
	if count >= 0 {
		title += m.styles.titleCount.Render(fmt.Sprintf("[%d]", count))
	}
	return title
}

func (m *Model) crumbsView() string {
	var crumbs []string
	for i, view := range m.views {
		style := m.styles.crumb
		if i == len(m.views)-1 {
			style = m.styles.crumbActive
		}
		crumbs = append(crumbs, style.Render("<"+view.crumb()+">"))
	}
	return " " + strings.Join(crumbs, " ")
}

func (m *Model) flashView() string {
	if m.flash == "" {
		return ""
	}
	if m.flashErr {
		return " " + m.styles.flashErr.Render(fit(m.flash, m.width-2))
	}
	return " " + m.styles.flash.Render(fit(m.flash, m.width-2))
}

// fit truncates s, which may be styled, to width cells, and pads it out to them.
func fit(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = ansi.Truncate(s, width, "…")
	if gap := width - ansi.StringWidth(s); gap > 0 {
		s += strings.Repeat(" ", gap)
	}
	return s
}

// overlay draws box over the middle of lines, which are width cells wide.
func overlay(lines []string, box string, width int) []string {
	boxLines := strings.Split(box, "\n")
	boxWidth := lipgloss.Width(box)
	top := max(0, (len(lines)-len(boxLines))/2)
	left := max(0, (width-boxWidth)/2)
	out := append([]string(nil), lines...)
	for i, boxLine := range boxLines {
		if top+i >= len(out) {
			break
		}
		line := fit(out[top+i], width)
		out[top+i] = ansi.Truncate(line, left, "") + boxLine + ansi.TruncateLeft(line, left+boxWidth, "")
	}
	return out
}

// topList is the cursor and scroll position of a list.
type topList struct {
	cursor int
	offset int
}

// move moves the cursor for key over n rows with height of them shown, reporting whether
// key was a move.
func (l *topList) move(key string, n, height int) bool {
	page := max(1, height-1)
	switch key {
	case "up", "k":
		l.cursor--
	case keyDown, "j":
		l.cursor++
	case "home", "g":
		l.cursor = 0
	case "end", "G":
		l.cursor = n - 1
	case "pgup", "ctrl+b":
		l.cursor -= page
	case "pgdown", "ctrl+f":
		l.cursor += page
	default:
		return false
	}
	l.clamp(n)
	return true
}

func (l *topList) clamp(n int) {
	l.cursor = max(0, min(l.cursor, n-1))
}

// window is the rows shown of n with height of them fitting, scrolled to the cursor.
func (l *topList) window(n, height int) (start, end int) {
	l.clamp(n)
	height = max(1, height)
	if l.cursor < l.offset {
		l.offset = l.cursor
	}
	if l.cursor >= l.offset+height {
		l.offset = l.cursor - height + 1
	}
	l.offset = max(0, min(l.offset, n-height))
	return l.offset, min(n, l.offset+height)
}

type topColumn struct {
	title string
	right bool // aligned right, for numbers
	max   int  // widest it gets, or 0 for as wide as its widest cell
	flex  bool // takes the width left over
}

// topRow is one row of a table: its cells, and the style the row has when it's not the
// cursor's.
type topRow struct {
	cells []string
	style lipgloss.Style
}

// table renders rows under their column headers in width×height, scrolled to the cursor
// of list, which it highlights when focused.
func (m *Model) table(columns []topColumn, rows []topRow, list *topList, width, height int) []string {
	widths := make([]int, len(columns))
	for i, column := range columns {
		widths[i] = ansi.StringWidth(column.title)
		for _, row := range rows {
			widths[i] = max(widths[i], ansi.StringWidth(row.cells[i]))
		}
		if column.max > 0 {
			widths[i] = min(widths[i], column.max)
		}
	}
	const gap = 2
	total := gap * (len(columns) - 1)
	for _, w := range widths {
		total += w
	}
	for i, column := range columns {
		if column.flex {
			widths[i] = max(4, widths[i]+width-total)
			break
		}
	}

	render := func(cells []string) string {
		parts := make([]string, len(cells))
		for i, cell := range cells {
			if columns[i].right {
				parts[i] = fmt.Sprintf("%*s", widths[i], ansi.Truncate(cell, widths[i], "…"))
			} else {
				parts[i] = fit(cell, widths[i])
			}
		}
		return fit(strings.Join(parts, strings.Repeat(" ", gap)), width)
	}

	titles := make([]string, len(columns))
	for i, column := range columns {
		titles[i] = column.title
	}
	lines := []string{m.styles.header.Render(render(titles))}
	start, end := list.window(len(rows), height-1)
	for i := start; i < end; i++ {
		line := render(rows[i].cells)
		if i == list.cursor {
			lines = append(lines, m.styles.cursor.Render(line))
		} else {
			lines = append(lines, rows[i].style.Render(line))
		}
	}
	return lines
}

// topDialog is a box over the frame that takes the keys until it's done.
type topDialog interface {
	update(m *Model, msg tea.KeyMsg) tea.Cmd
	view(m *Model) string
}

// dialogBox draws a dialog's lines in a box titled title.
func (m *Model) dialogBox(title string, lines []string) string {
	width := 0
	for _, line := range lines {
		width = max(width, lipgloss.Width(line))
	}
	width = max(width, lipgloss.Width(title)+4, 36)
	body := lipgloss.NewStyle().Width(width).Render(strings.Join(lines, "\n"))
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#FFA500")).
		Padding(0, 1).Render(body)
	// The title goes into the box's top border.
	boxLines := strings.Split(box, "\n")
	label := " " + m.styles.logo.Render(title) + " "
	border := lipgloss.NewStyle().Foreground(lipgloss.Color("#FFA500"))
	boxWidth := lipgloss.Width(boxLines[0])
	rest := max(0, boxWidth-3-lipgloss.Width(label))
	boxLines[0] = border.Render("╭─") + label + border.Render(strings.Repeat("─", rest)+"╮")
	return strings.Join(boxLines, "\n")
}

func (m *Model) buttons(labels []string, focused int) string {
	var parts []string
	for i, label := range labels {
		if i == focused {
			parts = append(parts, m.styles.cursor.Render(" "+label+" "))
		} else {
			parts = append(parts, m.styles.dim.Render(" "+label+" "))
		}
	}
	return strings.Join(parts, "  ")
}

// topConfirm asks yes or no.
type topConfirm struct {
	title, message string
	yes            func(m *Model) tea.Cmd
	no             bool // the focus is on No
	quitting       bool // it asks whether to quit
}

func (d *topConfirm) update(m *Model, msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "y":
		m.dialog = nil
		return d.yes(m)
	case "n", keyEsc, "q":
		m.dialog = nil
	case "left", "right", "tab", "shift+tab", "h", "l":
		d.no = !d.no
	case keyEnter:
		m.dialog = nil
		if !d.no {
			return d.yes(m)
		}
	}
	return nil
}

func (d *topConfirm) view(m *Model) string {
	focused := 0
	if d.no {
		focused = 1
	}
	return m.dialogBox(d.title, []string{m.styles.text.Render(d.message), "", m.buttons([]string{"Yes", "No"}, focused)})
}

// topPasswordDialog asks for a profile's password.
type topPasswordDialog struct {
	authID, name string
	password     string
	entered      func(password string) tea.Cmd
}

func (d *topPasswordDialog) update(m *Model, msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		m.dialog = nil
	case tea.KeyEnter:
		if d.password == "" {
			return nil
		}
		m.dialog = nil
		return d.entered(d.password)
	case tea.KeyBackspace:
		if d.password != "" {
			d.password = string([]rune(d.password)[:len([]rune(d.password))-1])
		}
	case tea.KeyRunes, tea.KeySpace:
		d.password += string(msg.Runes)
	}
	return nil
}

func (d *topPasswordDialog) view(m *Model) string {
	return m.dialogBox("Password", []string{
		m.styles.text.Render(fmt.Sprintf("Password for %s on %s:", d.authID, d.name)),
		m.styles.bold.Render(strings.Repeat("•", len([]rune(d.password)))) + m.styles.accent.Render("█"),
		"",
		m.styles.dim.Render("enter connects · esc cancels"),
	})
}
