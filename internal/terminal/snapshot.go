package terminal

import (
	"slices"
	"strconv"
	"unicode/utf8"
)

// snapshot is what brings a client's terminal to the screen's state: the history (all of
// it, or what scrolled off after since), then the normal screen, then the alternate one if
// it's in use, the cursor and the modes. gap tells that history the client would have been
// sent is gone.
//
// The client's screen is redrawn from its top-left corner, and the history and the normal
// screen are written as lines, so the history ends up in the client's own scrollback.
func (s *screen) snapshot(since *uint64) (data []byte, gap bool) {
	var lines [][]byte
	if since != nil {
		lines, gap = s.history.since(*since)
	} else {
		lines, gap = s.history.all()
	}

	b := make([]byte, 0, 4096)
	// Back to a known state, on the normal screen, before drawing anything.
	b = append(b, "\x1b[0m\x1b[?1049l\x1b[r\x1b[?6l\x1b[?7h\x1b[4l\x1b(B\x1b)B\x0f\x1b[H\x1b[2J"...)
	size := 0
	for _, l := range lines {
		size += len(l) + 2
	}
	b = slices.Grow(b, size+(s.cols+16)*s.rows)
	for _, l := range lines {
		b = append(b, l...)
		b = append(b, "\r\n"...)
	}
	// Each row is at most the width, so the normal screen fills the client's exactly.
	for y, l := range s.main {
		b = s.encodeLine(b, l.cells, true)
		if y < s.rows-1 {
			b = append(b, "\r\n"...)
		}
	}
	b = append(b, "\x1b[0m"...)

	if s.altActive {
		// The normal screen's cursor, for leaving the alternate one to restore: 1049 saves it
		// as it switches, the others leave it to DECSC.
		if s.altMode == 1049 {
			b = appendCursorState(b, s.saved[0].cursor)
		} else {
			b = s.appendCursor(b, s.saved[0])
		}
		b = append(b, "\x1b[?"...)
		b = strconv.AppendInt(b, int64(s.altMode), 10)
		b = append(b, "h\x1b[H\x1b[2J"...)
		for y, l := range s.alt {
			b = appendCUP(b, 0, y)
			b = s.encodeLine(b, l.cells, true)
		}
		b = s.appendCursor(b, s.saved[1])
	} else {
		b = s.appendCursor(b, s.saved[0])
	}
	return s.appendState(b), gap
}

// appendCursor sets up a DECSC saved cursor, if there is one, for a later DECRC.
func (s *screen) appendCursor(b []byte, saved savedCursor) []byte {
	if !saved.set {
		return b
	}
	b = appendCursorState(b, saved.cursor)
	b = append(b, "\x1b7"...)
	if saved.origin {
		b = append(b, "\x1b[?6l"...)
	}
	return append(b, "\x1b[0m\x1b(B\x1b)B\x0f"...)
}

// appendCursorState puts the client's cursor where c is, drawing as c does. The scroll
// region is still the whole screen.
func appendCursorState(b []byte, c cursor) []byte {
	b = appendCUP(b, c.x, c.y)
	b = appendCharsets(b, c)
	b = appendPen(b, c.pen)
	if c.origin {
		// DECSC saves origin mode, but setting it homes the cursor: set it, then position
		// again, relative to the region, which is the whole screen.
		b = append(b, "\x1b[?6h"...)
		b = appendCUP(b, c.x, c.y)
	}
	return b
}

// appendState sets the modes, the scroll region, the tab stops and lastly the cursor.
func (s *screen) appendState(b []byte) []byte {
	if s.title != "" {
		b = append(b, "\x1b]2;"...)
		b = append(b, sanitizeTitle(s.title)...)
		b = append(b, '\x07')
	}
	if !tabsDefault(s.tabs) {
		b = append(b, "\x1b[3g"...)
		for x, stop := range s.tabs {
			if stop {
				b = appendCUP(b, x, 0)
				b = append(b, "\x1bH"...)
			}
		}
	}
	for _, mode := range restoredModes {
		if s.privateModes[mode] {
			b = appendPrivateMode(b, mode, true)
		}
	}
	if s.appKeypad {
		b = append(b, "\x1b="...)
	}
	if s.modifyKeys != 0 {
		b = append(b, "\x1b[>4;"...)
		b = strconv.AppendInt(b, int64(s.modifyKeys), 10)
		b = append(b, 'm')
	}
	for _, flags := range s.kittyKeys {
		b = append(b, "\x1b[>"...)
		b = strconv.AppendInt(b, int64(flags), 10)
		b = append(b, 'u')
	}
	if s.cursorStyle != 0 {
		b = append(b, "\x1b["...)
		b = strconv.AppendInt(b, int64(s.cursorStyle), 10)
		b = append(b, " q"...)
	}
	if s.insert {
		b = append(b, "\x1b[4h"...)
	}
	if s.newline {
		b = append(b, "\x1b[20h"...)
	}

	cur := s.cur
	if s.top != 0 || s.bottom != s.rows-1 {
		b = append(b, "\x1b["...)
		b = strconv.AppendInt(b, int64(s.top+1), 10)
		b = append(b, ';')
		b = strconv.AppendInt(b, int64(s.bottom+1), 10)
		b = append(b, 'r')
	}
	if cur.wrapNext {
		// The cursor wrote the last column: writing that cell again leaves the client the
		// same, with the next character to wrap.
		row := s.lines()[cur.y].cells
		x := s.cols - 1
		if x > 0 && row[x-1].width == 2 {
			x--
		}
		b = appendCUP(b, x, cur.y)
		b = appendPen(b, row[x].pen)
		b = s.appendCell(b, row[x])
		b = append(b, "\x1b[0m"...)
	} else {
		b = appendCUP(b, cur.x, cur.y)
	}
	if cur.origin {
		// Setting origin mode homes the cursor: it's set last, then the cursor positioned in
		// the region.
		b = append(b, "\x1b[?6h"...)
		b = appendCUP(b, cur.x, cur.y-s.top)
	}
	if !s.autowrap {
		b = append(b, "\x1b[?7l"...)
	}
	b = appendCharsets(b, cur)
	b = appendPen(b, cur.pen)
	if !s.cursorVisible {
		b = append(b, "\x1b[?25l"...)
	} else {
		b = append(b, "\x1b[?25h"...)
	}
	return b
}

func appendCharsets(b []byte, c cursor) []byte {
	if c.charsets[0] {
		b = append(b, "\x1b(0"...)
	}
	if c.charsets[1] {
		b = append(b, "\x1b)0"...)
	}
	if c.shifted {
		b = append(b, '\x0e')
	}
	return b
}

func appendPrivateMode(b []byte, mode int, set bool) []byte {
	b = append(b, "\x1b[?"...)
	b = strconv.AppendInt(b, int64(mode), 10)
	if set {
		return append(b, 'h')
	}
	return append(b, 'l')
}

func appendCUP(b []byte, x, y int) []byte {
	b = append(b, "\x1b["...)
	b = strconv.AppendInt(b, int64(y+1), 10)
	b = append(b, ';')
	b = strconv.AppendInt(b, int64(x+1), 10)
	return append(b, 'H')
}

func tabsDefault(tabs []bool) bool {
	for x, stop := range tabs {
		if stop != (x > 0 && x%8 == 0) {
			return false
		}
	}
	return true
}

// sanitizeTitle keeps a title from ending the OSC early or carrying controls.
func sanitizeTitle(title string) []byte {
	out := make([]byte, 0, len(title))
	for i := 0; i < len(title); i++ {
		if c := title[i]; c >= 0x20 && c != 0x7f {
			out = append(out, c)
		}
	}
	return out
}

// encodeLine appends cells as a terminal would be sent them, from the cursor at the first:
// their text, with SGR wherever the pen changes, and the pen reset after. trim leaves out
// the trailing blanks a cleared line has anyway, which a row wrapped onto the next one
// doesn't have: they're part of its text.
func (s *screen) encodeLine(b []byte, cells []cell, trim bool) []byte {
	end := len(cells)
	for trim && end > 0 && cells[end-1].r == 0 && blankPen(cells[end-1].pen) {
		end--
	}
	b = slices.Grow(b, end+16)
	var current pen
	for i := range cells[:end] {
		c := &cells[i]
		if c.r == 0 && i > 0 && cells[i-1].width == 2 {
			// The right half of a wide character, written with it.
			continue
		}
		if c.pen != current {
			b = appendPen(b, c.pen)
			current = c.pen
		}
		if c.r > 0 && c.r < utf8.RuneSelf {
			b = append(b, byte(c.r))
		} else {
			b = s.appendCell(b, *c)
		}
	}
	if current != (pen{}) {
		b = append(b, "\x1b[0m"...)
	}
	return b
}

// blankPen reports whether a blank drawn with p looks like no cell at all.
func blankPen(p pen) bool {
	return p.bg == colorDefault && p.attrs&(attrReverse|attrOverline|attrStrike) == 0 && p.underline == 0
}

// appendPen appends the SGR that sets p, from any pen.
func appendPen(b []byte, p pen) []byte {
	b = append(b, "\x1b[0"...)
	for _, a := range []struct {
		attr uint16
		code string
	}{{attrBold, ";1"}, {attrFaint, ";2"}, {attrItalic, ";3"}, {attrBlink, ";5"}, {attrRapidBlink, ";6"},
		{attrReverse, ";7"}, {attrConceal, ";8"}, {attrStrike, ";9"}, {attrOverline, ";53"}} {
		if p.attrs&a.attr != 0 {
			b = append(b, a.code...)
		}
	}
	switch p.underline {
	case 0:
	case 1:
		b = append(b, ";4"...)
	default:
		b = append(b, ";4:"...)
		b = strconv.AppendInt(b, int64(p.underline), 10)
	}
	b = appendColor(b, p.fg, 30, 90, 38)
	b = appendColor(b, p.bg, 40, 100, 48)
	if p.ulColor != colorDefault {
		b = appendColor(b, p.ulColor, -1, -1, 58)
	}
	return append(b, 'm')
}

// appendColor appends c as SGR: base+n and bright+n-8 for the first 16 indexed colors where
// those exist, extended (5;n or 2;r;g;b) otherwise.
func appendColor(b []byte, c color, base, bright, extended int) []byte {
	value := int(c &^ colorKind)
	switch c & colorKind {
	case colorIndexed:
		switch {
		case value < 8 && base >= 0:
			b = append(b, ';')
			return strconv.AppendInt(b, int64(base+value), 10)
		case value < 16 && bright >= 0:
			b = append(b, ';')
			return strconv.AppendInt(b, int64(bright+value-8), 10)
		}
		b = append(b, ';')
		b = strconv.AppendInt(b, int64(extended), 10)
		b = append(b, ";5;"...)
		return strconv.AppendInt(b, int64(value), 10)
	case colorRGB:
		b = append(b, ';')
		b = strconv.AppendInt(b, int64(extended), 10)
		b = append(b, ";2;"...)
		b = strconv.AppendInt(b, int64(value>>16&0xff), 10)
		b = append(b, ';')
		b = strconv.AppendInt(b, int64(value>>8&0xff), 10)
		b = append(b, ';')
		return strconv.AppendInt(b, int64(value&0xff), 10)
	}
	return b
}
