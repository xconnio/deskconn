package top

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
)

// topForward is a port forward desk top runs: local to remote, or remote to local
// (reverse), until it's stopped or desk top quits.
type topForward struct {
	id      int
	target  topTarget
	reverse bool
	listen  string // the port listened on: locally, or on the machine if reverse
	to      string // the port connections go to: on the machine, or locally if reverse
	started time.Time
	ended   bool
	err     error
	stopped bool // by the user, so its end isn't a failure
	cancel  context.CancelFunc
}

// stop stops forwarding.
func (f *topForward) stop() {
	f.stopped = true
	f.cancel()
}

type topForwardEndedMsg struct {
	id  int
	err error
}

func (f *topForward) from() string {
	if f.reverse {
		return f.target.name + ":" + f.listen
	}
	return "127.0.0.1:" + f.listen
}

func (f *topForward) dest() string {
	if f.reverse {
		return "127.0.0.1:" + f.to
	}
	return f.target.name + ":" + f.to
}

// startForward forwards connections to listen on to port to on the other side.
func (m *Model) startForward(target topTarget, reverse bool, listen, to string) tea.Cmd {
	return m.withTarget(target, func() tea.Cmd {
		ctx, cancel := context.WithCancel(context.Background())
		forward := &topForward{id: m.newID(), target: target, reverse: reverse, listen: listen, to: to,
			started: time.Now(), cancel: cancel}
		m.forwards = append(m.forwards, forward)
		mode, realm, remote := m.streamMode(target), target.realm, m.remote
		run := func() tea.Msg {
			err := remote.Forward(ctx, mode, realm, reverse, listen, to)
			return topForwardEndedMsg{id: forward.id, err: err}
		}
		message := fmt.Sprintf("Forwarding %s → %s. See them all with f.", forward.from(), forward.dest())
		return tea.Batch(run, m.setFlash(message, false))
	})
}

func (m *Model) forwardEnded(msg topForwardEndedMsg) tea.Cmd {
	i := slices.IndexFunc(m.forwards, func(f *topForward) bool { return f.id == msg.id })
	if i < 0 {
		return nil
	}
	forward := m.forwards[i]
	if forward.stopped {
		// It goes from the list.
		m.forwards = slices.Delete(m.forwards, i, i+1)
		return nil
	}
	forward.ended, forward.err = true, msg.err
	if msg.err == nil {
		forward.err = errors.New("the machine ended it")
	}
	return m.failed(forward.target, fmt.Sprintf("Forward %s → %s", forward.from(), forward.dest()), forward.err)
}

func (m *Model) runningForwards() int {
	running := 0
	for _, forward := range m.forwards {
		if !forward.ended {
			running++
		}
	}
	return running
}

// topForwardsView lists the port forwards.
type topForwardsView struct {
	list   topList
	height int
}

func (v *topForwardsView) start(*Model) tea.Cmd { return nil }
func (v *topForwardsView) close()               {}
func (v *topForwardsView) crumb() string        { return "port forwards" }

func (v *topForwardsView) title(m *Model) string {
	return m.titled("PortForwards", "", len(m.forwards))
}

func (v *topForwardsView) hints(*Model) []topHint {
	return []topHint{{"ctrl-d", "Stop"}, {"shift-f", "New port forward"}}
}

func (v *topForwardsView) key(m *Model, key string) (tea.Cmd, bool) {
	if v.list.move(key, len(m.forwards), v.height) {
		return nil, true
	}
	switch key {
	case "ctrl+d", "d", "delete":
		if len(m.forwards) == 0 {
			return nil, true
		}
		v.list.clamp(len(m.forwards))
		forward := m.forwards[v.list.cursor]
		if forward.ended {
			m.forwards = slices.Delete(m.forwards, v.list.cursor, v.list.cursor+1)
			return nil, true
		}
		m.dialog = &topConfirm{
			title:   "Stop",
			message: fmt.Sprintf("Stop forwarding %s → %s?", forward.from(), forward.dest()),
			yes: func(m *Model) tea.Cmd {
				forward.stop()
				return m.setFlash(fmt.Sprintf("Stopped forwarding %s → %s.", forward.from(), forward.dest()), false)
			},
		}
		return nil, true
	case "F":
		if len(m.forwards) > 0 {
			v.list.clamp(len(m.forwards))
			m.dialog = newTopForwardDialog(m.forwards[v.list.cursor].target)
		} else {
			return m.setFlash("Pick a machine in :machines and press shift-f.", false), true
		}
		return nil, true
	}
	return nil, false
}

func (v *topForwardsView) handle(*Model, tea.Msg) (tea.Cmd, bool) { return nil, false }

func (v *topForwardsView) body(m *Model, width, height int) []string {
	v.height = height - 1
	if len(m.forwards) == 0 {
		return []string{
			m.styles.text.Render("No port forwards."),
			"",
			m.styles.dim.Render("Pick a machine in :machines and press shift-f to forward a port to it, or from it."),
		}
	}
	columns := []topColumn{
		{title: "MACHINE", max: 24}, {title: "DIRECTION"}, {title: "LISTEN"}, {title: "TO"},
		{title: "STATUS", flex: true}, {title: "AGE", right: true},
	}
	rows := make([]topRow, 0, len(m.forwards))
	for _, forward := range m.forwards {
		direction, status, style := "local → remote", "running", m.styles.good
		if forward.reverse {
			direction = "remote → local"
		}
		if forward.ended {
			status, style = "failed: "+forward.err.Error(), m.styles.bad
		}
		rows = append(rows, topRow{style: style, cells: []string{
			forward.target.name, direction, forward.from(), forward.dest(), status,
			common.FormatSince(time.Since(forward.started)),
		}})
	}
	return m.table(columns, rows, &v.list, width, height)
}

// topForwardDialog asks which ports to forward, and which way.
type topForwardDialog struct {
	target  topTarget
	reverse bool
	listen  string
	to      string
	focus   int // 0 the direction, 1 the listen port, 2 the port forwarded to
	err     string
}

// newTopForwardDialog starts on the port to listen on, the direction being local to remote
// unless changed.
func newTopForwardDialog(target topTarget) *topForwardDialog {
	return &topForwardDialog{target: target, focus: 1}
}

func (d *topForwardDialog) update(m *Model, msg tea.KeyMsg) tea.Cmd {
	field := &d.listen
	if d.focus == 2 {
		field = &d.to
	}
	switch key := msg.String(); key {
	case keyEsc:
		m.dialog = nil
	case keyCtrlC:
		return tea.Quit
	case "tab", keyDown:
		d.focus = (d.focus + 1) % 3
	case "shift+tab", "up":
		d.focus = (d.focus + 2) % 3
	case "left", "right":
		d.reverse = !d.reverse
	case " ":
		if d.focus == 0 {
			d.reverse = !d.reverse
		}
	case keyEnter:
		if d.focus < 2 {
			d.focus++
			return nil
		}
		listen, to, err := deskconn.ParsePortMapping(d.listen + ":" + d.to)
		if err != nil {
			d.err = err.Error()
			return nil
		}
		m.dialog = nil
		return m.startForward(d.target, d.reverse, listen, to)
	case "backspace":
		if d.focus > 0 && *field != "" {
			*field = (*field)[:len(*field)-1]
		}
	default:
		if d.focus > 0 && len(key) == 1 && key[0] >= '0' && key[0] <= '9' && len(*field) < 5 {
			*field += key
			d.err = ""
		}
	}
	return nil
}

func (d *topForwardDialog) view(m *Model) string {
	listenAt, toAt := "127.0.0.1:", d.target.name+":"
	listenNote, toNote := "on this machine", "on "+d.target.name+"'s localhost"
	if d.reverse {
		listenAt, toAt = toAt, listenAt
		listenNote, toNote = "on "+d.target.name+", all interfaces", "on this machine"
	}
	marker := func(i int) string {
		if d.focus == i {
			return m.styles.accent.Render("▸ ")
		}
		return "  "
	}
	field := func(i int, label, at, value, note string) string {
		if d.focus == i {
			value = m.styles.bold.Render(value) + m.styles.accent.Render("█")
		} else {
			value = m.styles.text.Render(value)
		}
		return marker(i) + m.styles.infoLabel.Render(fmt.Sprintf("%-12s", label)) + m.styles.dim.Render(at) +
			value + "  " + m.styles.faint.Render(note)
	}
	directions := m.buttons([]string{"local → remote", "remote → local"}, map[bool]int{false: 0, true: 1}[d.reverse])
	lines := []string{
		m.styles.text.Render("Forward a port to or from " + d.target.name + "."),
		"",
		marker(0) + m.styles.infoLabel.Render(fmt.Sprintf("%-12s", "Direction")) + directions,
		field(1, "Listen on", listenAt, d.listen, listenNote),
		field(2, "Forward to", toAt, d.to, toNote),
		"",
	}
	if d.err != "" {
		lines = append(lines, m.styles.bad.Render(d.err), "")
	}
	hint := "tab next field · ←/→ direction · enter forwards · esc cancels"
	return m.dialogBox("Port forward", append(lines, m.styles.dim.Render(hint)))
}
