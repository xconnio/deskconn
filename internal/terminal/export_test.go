package terminal

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// ValidID reports whether id could be a session ID.
func ValidID(id string) bool { return validID(id) }

// Dir is the directory m keeps its sessions' records in.
func (m *Manager) Dir() string { return m.dir }

// SessionIDs lists the sessions with records.
func (m *Manager) SessionIDs() []string {
	ids, _ := recordIDs(m.dir)
	return ids
}

// RecordPath and SocketPath are session id's record and socket.
func (m *Manager) RecordPath(id string) string { return recordPath(m.dir, id) }
func (m *Manager) SocketPath(id string) string { return socketPath(m.dir, id) }

// WorkerPID is the PID session id's record gives for its worker.
func (m *Manager) WorkerPID(id string) (int, error) {
	rec, err := readRecord(m.dir, id)
	return rec.PID, err
}

// AttachWithSecret attaches to session id presenting secret instead of its record's.
func (m *Manager) AttachWithSecret(ctx context.Context, id, secret string) error {
	rec, err := readRecord(m.dir, id)
	if err != nil {
		return err
	}
	rec.Secret = secret
	conn, _, err := m.request(ctx, rec, message{Op: opAttach})
	if conn != nil {
		_ = conn.Close()
	}
	return err
}

// Screen is the terminal emulator a worker keeps its session's display in.
type Screen = screen

// NewScreen returns a screen of cols × rows keeping historyLimit bytes of history.
func NewScreen(cols, rows, historyLimit int) *Screen { return newScreen(cols, rows, historyLimit) }

func (s *screen) Write(p []byte)         { s.write(p) }
func (s *screen) Resize(cols, rows int)  { s.resize(cols, rows) }
func (s *screen) Offset() uint64         { return s.offset }
func (s *screen) AltActive() bool        { return s.altActive }
func (s *screen) Cursor() (x, y int)     { return s.cur.x, s.cur.y }
func (s *screen) HistorySize() int       { return s.history.size }
func (s *screen) HistoryDropped() uint64 { return s.history.dropped }

// Snapshot is the screen's snapshot: all the history, or what scrolled off after since.
func (s *screen) Snapshot(since *uint64) ([]byte, bool) { return s.snapshot(since) }

// Text is what the active screen shows, a line per row, without trailing blanks.
func (s *screen) Text() string { return s.linesText(s.lines()) }

// MainText is what the normal screen shows.
func (s *screen) MainText() string { return s.linesText(s.main) }

// History is the history's lines, as text.
func (s *screen) History() []string {
	lines, _ := s.history.all()
	texts := make([]string, len(lines))
	for i, l := range lines {
		texts[i] = ansi.Strip(string(l))
	}
	return texts
}

// State is everything a snapshot restores but the screens' text, for comparing two screens.
func (s *screen) State() string {
	return fmt.Sprintf("cur=%+v saved=%+v alt=%v/%d region=%d-%d wrap=%v ins=%v lnm=%v vis=%v style=%d "+
		"keypad=%v modkeys=%d kitty=%v modes=%v title=%q pens=%s", s.cur, s.saved, s.altActive, s.altMode,
		s.top, s.bottom, s.autowrap, s.insert, s.newline, s.cursorVisible, s.cursorStyle, s.appKeypad,
		s.modifyKeys, s.kittyKeys, s.privateModes, s.title, s.pens())
}

func (s *screen) pens() string {
	var b strings.Builder
	for _, lines := range [][]line{s.main, s.alt} {
		for _, l := range lines {
			b.Write(s.encodeLine(nil, l.cells, true))
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func (s *screen) linesText(lines []line) string {
	rows := make([]string, len(lines))
	for i, l := range lines {
		rows[i] = strings.TrimRight(ansi.Strip(string(s.encodeLine(nil, l.cells, true))), " ")
	}
	return strings.Join(rows, "\n")
}

// HistoryLimit is the history session id was given.
func (m *Manager) HistoryLimit(id string) (int, error) {
	rec, err := readRecord(m.dir, id)
	return rec.HistoryLimit, err
}

// Modes is the screen's modes, but for the cursor's position and the title.
func (s *screen) Modes() string {
	return fmt.Sprintf("pen=%+v origin=%v charsets=%v/%v region=%d-%d wrap=%v ins=%v lnm=%v vis=%v style=%d "+
		"keypad=%v modkeys=%d kitty=%d modes=%v", s.cur.pen, s.cur.origin, s.cur.charsets, s.cur.shifted,
		s.top, s.bottom, s.autowrap, s.insert, s.newline, s.cursorVisible, s.cursorStyle, s.appKeypad,
		s.modifyKeys, len(s.kittyKeys), s.privateModes)
}
