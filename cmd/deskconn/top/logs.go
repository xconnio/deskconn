package top

import (
	"context"
	"errors"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/xconnio/deskconn/internal/common"
)

const (
	// topLogTail is how many lines a log starts with.
	topLogTail = 200
	// topLogKeep is how many lines the logs view keeps.
	topLogKeep = 10000
)

// topLogsView streams a machine's logs, following them as they come.
type topLogsView struct {
	target topTarget
	mode   string
	remote Remote
	id     int

	source  string // a service or file, or "" for the system journal
	lines   []string
	partial string // the start of a line still coming
	ended   bool
	err     error

	stream topLogStream

	follow bool // scrolled to the end, staying there as lines come
	top    int  // the first line shown when not following
	wrap   bool
	search string
	width  int
	height int
}

// topLogStream is one run of a log: its output, and its error once it ends.
type topLogStream struct {
	chunks chan []byte
	done   chan error
	cancel context.CancelFunc
}

type (
	topLogMsg struct {
		id   int
		data []byte
	}
	topLogEndMsg struct {
		id  int
		err error
	}
)

func newTopLogsView(m *Model, target topTarget) *topLogsView {
	return &topLogsView{target: target, mode: m.streamMode(target), remote: m.remote, follow: true}
}

func (v *topLogsView) start(m *Model) tea.Cmd {
	v.id = m.newID()
	v.lines, v.partial, v.ended, v.err = nil, "", false, nil

	ctx, cancel := context.WithCancel(context.Background())
	stream := topLogStream{chunks: make(chan []byte, 64), done: make(chan error, 1), cancel: cancel}
	v.stream = stream
	common.SafeGo(func() {
		err := v.remote.Logs(ctx, v.mode, v.target.realm, v.source, chanWriter{ctx: ctx, chunks: stream.chunks})
		stream.done <- err
		close(stream.chunks)
	})
	return v.wait()
}

// chanWriter sends what's written to it to chunks, until ctx is done.
type chanWriter struct {
	ctx    context.Context
	chunks chan<- []byte
}

func (w chanWriter) Write(p []byte) (int, error) {
	select {
	case w.chunks <- append([]byte(nil), p...):
		return len(p), nil
	case <-w.ctx.Done():
		return 0, w.ctx.Err()
	}
}

// wait waits for the next of the log, taking whatever else has come with it.
func (v *topLogsView) wait() tea.Cmd {
	id, stream := v.id, v.stream
	return func() tea.Msg {
		data, ok := <-stream.chunks
		if !ok {
			return topLogEndMsg{id: id, err: <-stream.done}
		}
		for {
			select {
			case more, ok := <-stream.chunks:
				if !ok {
					// The end comes on the next wait.
					return topLogMsg{id: id, data: data}
				}
				data = append(data, more...)
			default:
				return topLogMsg{id: id, data: data}
			}
		}
	}
}

func (v *topLogsView) close() {
	if v.stream.cancel != nil {
		v.stream.cancel()
	}
	v.id = -1
}

func (v *topLogsView) crumb() string  { return "logs" }
func (v *topLogsView) filter() string { return v.search }

func (v *topLogsView) setFilter(filter string) {
	v.search = filter
	v.follow = true
}

func (v *topLogsView) sourceName() string {
	if v.source == "" {
		return "journal"
	}
	return v.source
}

func (v *topLogsView) title(m *Model) string {
	title := m.titled("Logs("+v.target.name+")", v.search, -1) + m.styles.dim.Render(" "+v.sourceName())
	if v.follow && !v.ended {
		title += m.styles.good.Render(" following")
	}
	return title
}

func (v *topLogsView) hints(*Model) []topHint {
	return []topHint{
		{"s", "Autoscroll"}, {"w", "Wrap"}, {"c", "Clear"}, {"o", "Source"}, {"r", "Restart"},
		{"g/G", "Top, bottom"}, {"/", hintFilter},
	}
}

func (v *topLogsView) handle(m *Model, msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case topLogMsg:
		if msg.id != v.id {
			return nil, false
		}
		v.add(string(msg.data))
		return v.wait(), true
	case topLogEndMsg:
		if msg.id != v.id {
			return nil, false
		}
		v.ended = true
		if v.partial != "" {
			v.lines = append(v.lines, v.partial)
			v.partial = ""
		}
		if msg.err != nil && !errors.Is(msg.err, context.Canceled) {
			v.err = msg.err
			if strings.Contains(strings.ToLower(msg.err.Error()), "auth") {
				m.closeView(v)
				return m.failed(v.target, "Logs of "+v.target.name, msg.err), true
			}
		}
		return nil, true
	}
	return nil, false
}

// add adds data to the lines, cleaned of what would upset the screen.
func (v *topLogsView) add(data string) {
	data = v.partial + data
	parts := strings.Split(data, "\n")
	v.partial = parts[len(parts)-1]
	for _, line := range parts[:len(parts)-1] {
		line = strings.ReplaceAll(ansi.Strip(strings.TrimSuffix(line, "\r")), "\t", "    ")
		v.lines = append(v.lines, line)
	}
	if over := len(v.lines) - topLogKeep; over > 0 {
		v.lines = append([]string(nil), v.lines[over:]...)
		v.top = max(0, v.top-over)
	}
}

func (v *topLogsView) key(m *Model, key string) (tea.Cmd, bool) {
	shown := len(v.shown(v.width))
	page := max(1, v.height-1)
	last := max(0, shown-v.height)
	if v.follow {
		v.top = last
	}
	switch key {
	case "up", "k":
		v.top, v.follow = max(0, v.top-1), false
	case keyDown, "j":
		v.top = min(last, v.top+1)
		v.follow = v.top == last
	case "pgup", "ctrl+b":
		v.top, v.follow = max(0, v.top-page), false
	case "pgdown", "ctrl+f":
		v.top = min(last, v.top+page)
		v.follow = v.top == last
	case "home", "g":
		v.top, v.follow = 0, false
	case "end", "G":
		v.follow = true
	case "s":
		v.follow = !v.follow
	case "w":
		v.wrap = !v.wrap
	case "c":
		v.lines, v.top = nil, 0
	case "r":
		v.close()
		return v.start(m), true
	case "o":
		m.dialog = &topInputDialog{
			title: "Log source",
			label: "Service (e.g. nginx) or file (e.g. /var/log/syslog); empty for the system journal:",
			text:  v.source,
			entered: func(m *Model, source string) tea.Cmd {
				v.close()
				v.source = strings.TrimSpace(source)
				return v.start(m)
			},
		}
	case keyEsc:
		if v.search == "" {
			return nil, false
		}
		v.setFilter("")
	default:
		return nil, false
	}
	return nil, true
}

// shown are the lines that match the filter, wrapped to width if wrapping (and width > 0).
func (v *topLogsView) shown(width int) []string {
	search := strings.ToLower(v.search)
	var lines []string
	for _, line := range v.lines {
		if search != "" && !strings.Contains(strings.ToLower(line), search) {
			continue
		}
		if v.wrap && width > 0 {
			lines = append(lines, strings.Split(ansi.Hardwrap(line, width, true), "\n")...)
		} else {
			lines = append(lines, line)
		}
	}
	return lines
}

func (v *topLogsView) body(m *Model, width, height int) []string {
	v.width, v.height = width, height
	lines := v.shown(width)
	var status string
	switch {
	case v.err != nil:
		status = m.styles.bad.Render("The log ended: " + v.err.Error() + ". Press r to start it again.")
	case v.ended:
		status = m.styles.dim.Render("The log ended. Press r to start it again.")
	case len(v.lines) == 0 && v.partial == "":
		status = m.styles.dim.Render("Waiting for the " + v.sourceName() + " of " + v.target.name + "…")
	}
	if status != "" {
		height--
	}

	last := max(0, len(lines)-height)
	if v.follow {
		v.top = last
	}
	v.top = max(0, min(v.top, last))
	out := make([]string, 0, height+1)
	for _, line := range lines[v.top:min(len(lines), v.top+height)] {
		out = append(out, m.styles.text.Render(line))
	}
	if status != "" {
		out = append(out, status)
	}
	return out
}

// topInputDialog asks for a line of text.
type topInputDialog struct {
	title, label string
	text         string
	entered      func(m *Model, text string) tea.Cmd
}

func (d *topInputDialog) update(m *Model, msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyEsc:
		m.dialog = nil
	case tea.KeyEnter:
		m.dialog = nil
		return d.entered(m, d.text)
	case tea.KeyBackspace:
		if d.text != "" {
			d.text = string([]rune(d.text)[:len([]rune(d.text))-1])
		}
	case tea.KeyCtrlU:
		d.text = ""
	case tea.KeyRunes, tea.KeySpace:
		d.text += string(msg.Runes)
	}
	return nil
}

func (d *topInputDialog) view(m *Model) string {
	return m.dialogBox(d.title, []string{
		m.styles.text.Render(d.label),
		m.styles.bold.Render(d.text) + m.styles.accent.Render("█"),
		"",
		m.styles.dim.Render("enter · esc cancels"),
	})
}
