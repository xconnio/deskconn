package terminal

// Mirror follows what a client writes to its terminal from an attachment, to tell how to
// leave that terminal as a shell would have it: a client detaching from a full-screen
// program, or losing its connection, leaves the terminal in the program's modes.
type Mirror struct{ s *screen }

// NewMirror returns a Mirror of a terminal of cols × rows.
func NewMirror(cols, rows int) *Mirror { return &Mirror{s: newScreen(cols, rows, 0)} }

// Write follows p written to the terminal.
func (m *Mirror) Write(p []byte) { m.s.write(p) }

// Resize follows the terminal being resized.
func (m *Mirror) Resize(cols, rows int) { m.s.resize(cols, rows) }

// Reset is what returns the terminal to the normal screen, with the modes the output set
// turned off, where its cursor is: what's left of the session's display stays on it.
func (m *Mirror) Reset() []byte {
	s := m.s
	var b []byte
	if s.altActive {
		b = appendPrivateMode(b, s.altMode, false)
	}
	for _, mode := range restoredModes {
		if s.privateModes[mode] {
			b = appendPrivateMode(b, mode, false)
		}
	}
	if s.appKeypad {
		b = append(b, "\x1b>"...)
	}
	if s.modifyKeys != 0 {
		b = append(b, "\x1b[>4m"...)
	}
	if len(s.kittyKeys) > 0 {
		b = append(b, "\x1b[<99u"...)
	}
	if s.cursorStyle != 0 {
		b = append(b, "\x1b[0 q"...)
	}
	if s.insert {
		b = append(b, "\x1b[4l"...)
	}
	if s.newline {
		b = append(b, "\x1b[20l"...)
	}
	if !s.autowrap {
		b = append(b, "\x1b[?7h"...)
	}
	// Both home the cursor: it's put back.
	if s.top != 0 || s.bottom != s.rows-1 || s.cur.origin {
		b = append(b, "\x1b7\x1b[?6l\x1b[r\x1b8"...)
	}
	return append(b, "\x1b[?25h\x1b[0m\x1b(B\x1b)B\x0f"...)
}
