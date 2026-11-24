package terminal

import (
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/parser"
	"github.com/mattn/go-runewidth"
)

// screen is a terminal emulator for a session's output: what a terminal showing it would
// show, both screens, the cursor and the modes that change what input means, so that a
// client attaching later can be shown the same (see snapshot). Lines scrolling off the top
// of the normal screen go into its history; the alternate screen keeps none, as full-screen
// programs redraw it.
//
// The parser keeps its state between writes, so an escape sequence or a character split
// across reads is put together as a terminal would.
type screen struct {
	cols, rows int
	main, alt  []line
	altActive  bool
	altMode    int // the private mode that switched to the alternate screen: 47, 1047 or 1049

	cur   cursor
	saved [2]savedCursor // DECSC's, for the main and the alternate screen

	top, bottom int // scroll region, inclusive
	tabs        []bool

	autowrap      bool
	insert        bool // IRM
	newline       bool // LNM: line feed also returns the carriage
	cursorVisible bool
	cursorStyle   int // DECSCUSR
	appKeypad     bool
	modifyKeys    int // xterm's modifyOtherKeys
	kittyKeys     []int
	privateModes  map[int]bool // the restoredModes that are set
	title         string
	lastPrinted   rune

	history *history
	parser  *ansi.Parser
	// offset counts the bytes written; history lines carry the offset that scrolled them off.
	offset uint64

	// clusters are the characters with combining marks the cells use, by -cell.r-1.
	clusters   []string
	clusterIDs map[string]rune
	// spare and scratch are reused while scrolling.
	spare   []line
	scratch []byte
}

// cursor is the cursor's position and what's drawn with it.
type cursor struct {
	x, y int
	pen  pen
	// wrapNext is the cursor having written the last column: the next character wraps.
	wrapNext bool
	origin   bool    // DECOM: positions are relative to the scroll region
	charsets [2]bool // G0 and G1: whether each is DEC special graphics
	shifted  bool    // SO: G1 is in use
}

type savedCursor struct {
	cursor
	set bool
}

// line is one row of cells. wrapped means the row continues on the next one: its text was
// wrapped there rather than ended by a line feed.
type line struct {
	cells   []cell
	wrapped bool
}

// cell is one column. A wide character takes two: its own and, after it, one of width 0.
// Cells hold no pointers, for a screen to cost the garbage collector nothing to scan.
type cell struct {
	// r is the character: 0 is blank, and a negative one is a character with combining
	// marks, one of the screen's clusters.
	r     rune
	width uint8
	pen   pen
}

// pen is how a cell is drawn (SGR).
type pen struct {
	attrs     uint16
	underline uint8 // style: 0 none, 1 single, 2 double, 3 curly, 4 dotted, 5 dashed
	fg, bg    color
	ulColor   color
}

const (
	attrBold uint16 = 1 << iota
	attrFaint
	attrItalic
	attrBlink
	attrRapidBlink
	attrReverse
	attrConceal
	attrStrike
	attrOverline
)

// color is a pen's color: colorDefault, colorIndexed|n, or colorRGB|0xRRGGBB.
type color uint32

const (
	colorDefault color = 0
	colorIndexed color = 1 << 24
	colorRGB     color = 2 << 24
	colorKind    color = 0xff << 24
)

const (
	// maxCols and maxRows bound a screen whatever size a client asks for.
	maxCols = 1000
	maxRows = 500
	// maxCellText bounds the combining marks piled on one cell.
	maxCellText = 32
	// maxClusters bounds the distinct characters with combining marks a screen keeps; any
	// more are shown without their marks.
	maxClusters = 4096
	// maxTitle bounds the window title kept.
	maxTitle = 1024
	// maxKittyKeys bounds the kitty keyboard protocol's stack, as kitty does.
	maxKittyKeys = 16
	// parserData bounds an OSC, DCS or similar string's data kept while parsing it; longer
	// data is cut, which only matters for titles here.
	parserData = 4096
)

// restoredModes are the DEC private modes restored as they were, besides the ones handled
// on their own (6, 7, 25 and the alternate screen): those that change what keys, the mouse
// or pasting send, or how the screen looks.
var restoredModes = []int{ //nolint:gochecknoglobals // a read-only table
	1, 5, 9, 12, 1000, 1001, 1002, 1003, 1004, 1005, 1006, 1007, 1015, 1016, 2004,
}

// widths are character widths as terminals lay them out, whatever the locale says about
// East Asian ambiguous ones.
var widths = &runewidth.Condition{EastAsianWidth: false} //nolint:gochecknoglobals // read-only

func newScreen(cols, rows, historyLimit int) *screen {
	cols, rows = clampSize(cols, rows)
	s := &screen{history: newHistory(historyLimit), parser: ansi.NewParser()}
	s.parser.SetDataSize(parserData)
	s.parser.SetHandler(ansi.Handler{
		Print:     s.print,
		Execute:   s.execute,
		HandleCsi: s.csi,
		HandleEsc: s.esc,
		HandleOsc: s.osc,
	})
	s.reset(cols, rows)
	return s
}

func clampSize(cols, rows int) (int, int) {
	return min(max(cols, 1), maxCols), min(max(rows, 1), maxRows)
}

// reset puts the screen in a terminal's initial state, keeping its size and history (RIS).
func (s *screen) reset(cols, rows int) {
	s.cols, s.rows = cols, rows
	s.main, s.alt = newLines(cols, rows), newLines(cols, rows)
	s.altActive, s.altMode = false, 0
	s.cur = cursor{}
	s.saved = [2]savedCursor{}
	s.top, s.bottom = 0, rows-1
	s.tabs = defaultTabs(cols)
	s.autowrap, s.insert, s.newline = true, false, false
	s.cursorVisible, s.cursorStyle = true, 0
	s.appKeypad, s.modifyKeys, s.kittyKeys = false, 0, nil
	s.privateModes = map[int]bool{}
	s.title = ""
}

func newLines(cols, rows int) []line {
	lines := make([]line, rows)
	for i := range lines {
		lines[i] = line{cells: make([]cell, cols)}
	}
	return lines
}

func defaultTabs(cols int) []bool {
	tabs := make([]bool, cols)
	for x := 8; x < cols; x += 8 {
		tabs[x] = true
	}
	return tabs
}

// write runs output through the screen.
func (s *screen) write(p []byte) {
	for i := 0; i < len(p); {
		// Printable ASCII between sequences, most of any output, goes straight on the screen.
		if p[i] >= 0x20 && p[i] < 0x7f && s.parser.State() == parser.GroundState {
			j := i + 1
			for j < len(p) && p[j] >= 0x20 && p[j] < 0x7f {
				j++
			}
			s.printASCII(p[i:j])
			i = j
			continue
		}
		s.offset++
		s.parser.Advance(p[i])
		i++
	}
}

// printASCII prints printable ASCII, as print does, a row's worth at a time.
func (s *screen) printASCII(text []byte) {
	for len(text) > 0 {
		if s.cur.wrapNext || s.insert || s.cur.charsets[s.charset()] {
			s.offset++
			s.print(rune(text[0]))
			text = text[1:]
			continue
		}
		row := s.lines()[s.cur.y].cells
		x := s.cur.x
		n := min(len(text), s.cols-x)
		s.clearWide(row, x)
		s.clearWide(row, x+n-1)
		p := s.cur.pen
		for i, b := range text[:n] {
			row[x+i] = cell{r: rune(b), width: 1, pen: p}
		}
		s.offset += uint64(n) //nolint:gosec // a length
		s.lastPrinted = rune(text[n-1])
		if x+n < s.cols {
			s.cur.x = x + n
		} else {
			s.cur.x, s.cur.wrapNext = s.cols-1, s.autowrap
		}
		text = text[n:]
	}
}

func (s *screen) lines() []line {
	if s.altActive {
		return s.alt
	}
	return s.main
}

// blank is an erased cell: erasing keeps the current background, as xterm does.
func (s *screen) blank() cell {
	return cell{pen: pen{bg: s.cur.pen.bg}}
}

func (s *screen) print(r rune) {
	if s.cur.charsets[s.charset()] && r >= 0x5f && r <= 0x7e {
		r = decGraphics[r-0x5f]
	}
	width := widths.RuneWidth(r)
	if width == 0 {
		s.combine(r)
		return
	}
	s.lastPrinted = r
	lines := s.lines()
	if s.cur.wrapNext && s.autowrap {
		lines[s.cur.y].wrapped = true
		s.cur.x = 0
		s.index()
	}
	s.cur.wrapNext = false
	if width == 2 && s.cur.x == s.cols-1 {
		if !s.autowrap || s.cols < 2 {
			return
		}
		// A wide character doesn't fit in the last column: it goes on the next line.
		lines[s.cur.y].cells[s.cur.x] = s.blank()
		lines[s.cur.y].wrapped = true
		s.cur.x = 0
		s.index()
	}
	row := lines[s.cur.y].cells
	if s.insert {
		copy(row[s.cur.x+width:], row[s.cur.x:])
	}
	s.clearWide(row, s.cur.x)
	if width == 2 {
		s.clearWide(row, s.cur.x+1)
	}
	row[s.cur.x] = cell{r: r, width: uint8(width), pen: s.cur.pen} //nolint:gosec // 1 or 2
	if width == 2 {
		row[s.cur.x+1] = cell{width: 0, pen: s.cur.pen}
	}
	s.cur.x += width
	if s.cur.x >= s.cols {
		s.cur.x = s.cols - 1
		s.cur.wrapNext = s.autowrap
	}
}

// clearWide blanks the other half of a wide character that writing column x breaks.
func (s *screen) clearWide(row []cell, x int) {
	if x >= len(row) {
		return
	}
	switch {
	case row[x].width == 2 && x+1 < len(row):
		row[x+1] = cell{pen: row[x+1].pen}
	case row[x].width == 0 && row[x].r == 0 && x > 0 && row[x-1].width == 2:
		row[x-1] = cell{pen: row[x-1].pen}
	}
}

// combine adds a zero-width character, such as a combining mark, to the last one written.
func (s *screen) combine(r rune) {
	x := s.cur.x
	if !s.cur.wrapNext {
		x--
	}
	row := s.lines()[s.cur.y].cells
	if x > 0 && row[x].width == 0 && row[x-1].width == 2 {
		x--
	}
	if x < 0 || row[x].r == 0 {
		return
	}
	text := s.cellText(row[x]) + string(r)
	if len(text) > maxCellText {
		return
	}
	if id, ok := s.clusterIDs[text]; ok {
		row[x].r = id
		return
	}
	if len(s.clusters) >= maxClusters {
		return
	}
	if s.clusterIDs == nil {
		s.clusterIDs = map[string]rune{}
	}
	s.clusters = append(s.clusters, text)
	id := -rune(len(s.clusters)) //nolint:gosec // at most maxClusters
	s.clusterIDs[text] = id
	row[x].r = id
}

// cellText is c's character, with its combining marks; "" for a blank.
func (s *screen) cellText(c cell) string {
	switch {
	case c.r == 0:
		return ""
	case c.r < 0:
		return s.clusters[-c.r-1]
	default:
		return string(c.r)
	}
}

// appendCell appends c's character, or a space for a blank.
func (s *screen) appendCell(b []byte, c cell) []byte {
	switch {
	case c.r > 0 && c.r < utf8.RuneSelf:
		return append(b, byte(c.r))
	case c.r == 0:
		return append(b, ' ')
	case c.r < 0:
		return append(b, s.clusters[-c.r-1]...)
	default:
		return utf8.AppendRune(b, c.r)
	}
}

func (s *screen) charset() int {
	if s.cur.shifted {
		return 1
	}
	return 0
}

func (s *screen) execute(b byte) {
	switch b {
	case ansi.BS:
		s.cur.wrapNext = false
		if s.cur.x > 0 {
			s.cur.x--
		}
	case ansi.HT:
		s.tab(1)
	case ansi.LF, ansi.VT, ansi.FF:
		s.index()
		if s.newline {
			s.cur.x = 0
		}
	case ansi.CR:
		s.cur.x, s.cur.wrapNext = 0, false
	case ansi.SO:
		s.cur.shifted = true
	case ansi.SI:
		s.cur.shifted = false
	}
}

// index moves the cursor down a line, scrolling at the bottom of the scroll region.
func (s *screen) index() {
	s.cur.wrapNext = false
	switch {
	case s.cur.y == s.bottom:
		s.scrollUp(1)
	case s.cur.y < s.rows-1:
		s.cur.y++
	}
}

func (s *screen) reverseIndex() {
	s.cur.wrapNext = false
	switch {
	case s.cur.y == s.top:
		s.scrollDown(1)
	case s.cur.y > 0:
		s.cur.y--
	}
}

// scrollUp scrolls the scroll region up n lines. Lines leaving the top of the normal screen,
// when the region is the whole screen, go into the history.
func (s *screen) scrollUp(n int) {
	n = min(n, s.bottom-s.top+1)
	if !s.altActive && s.top == 0 && s.bottom == s.rows-1 {
		for _, l := range s.main[:n] {
			s.pushHistory(l)
		}
	}
	s.rotateUp(s.top, s.bottom, n)
}

func (s *screen) scrollDown(n int) {
	n = min(n, s.bottom-s.top+1)
	s.rotateDown(s.top, s.bottom, n)
}

func (s *screen) pushHistory(l line) {
	s.scratch = s.encodeLine(s.scratch[:0], l.cells, !l.wrapped)
	s.history.push(s.scratch, l.wrapped, s.offset)
}

// rotateUp moves rows top+n to bottom up n, blanking the n rows that come in at the bottom:
// those that went out at the top, reused.
func (s *screen) rotateUp(top, bottom, n int) {
	lines := s.lines()
	s.spare = append(s.spare[:0], lines[top:top+n]...)
	copy(lines[top:], lines[top+n:bottom+1])
	for i, l := range s.spare {
		lines[bottom-n+1+i] = s.clearLine(l)
	}
}

// rotateDown moves rows top to bottom-n down n, blanking the n rows that come in at the top.
func (s *screen) rotateDown(top, bottom, n int) {
	lines := s.lines()
	s.spare = append(s.spare[:0], lines[bottom-n+1:bottom+1]...)
	copy(lines[top+n:bottom+1], lines[top:bottom+1-n])
	for i, l := range s.spare {
		lines[top+i] = s.clearLine(l)
	}
}

func (s *screen) clearLine(l line) line {
	blank := s.blank()
	if blank == (cell{}) {
		clear(l.cells)
	} else {
		for i := range l.cells {
			l.cells[i] = blank
		}
	}
	return line{cells: l.cells}
}

func (s *screen) tab(n int) {
	s.cur.wrapNext = false
	for ; n > 0 && s.cur.x < s.cols-1; n-- {
		s.cur.x++
		for s.cur.x < s.cols-1 && !s.tabs[s.cur.x] {
			s.cur.x++
		}
	}
}

func (s *screen) backTab(n int) {
	s.cur.wrapNext = false
	for ; n > 0 && s.cur.x > 0; n-- {
		s.cur.x--
		for s.cur.x > 0 && !s.tabs[s.cur.x] {
			s.cur.x--
		}
	}
}

// moveTo moves the cursor to x, y: y relative to the scroll region in origin mode.
func (s *screen) moveTo(x, y int) {
	minY, maxY := 0, s.rows-1
	if s.cur.origin {
		y += s.top
		minY, maxY = s.top, s.bottom
	}
	s.cur.x = min(max(x, 0), s.cols-1)
	s.cur.y = min(max(y, minY), maxY)
	s.cur.wrapNext = false
}

// moveRows moves the cursor n rows, stopping at the scroll region's edge if it starts in it.
func (s *screen) moveRows(n int) {
	minY, maxY := 0, s.rows-1
	if s.cur.y >= s.top && s.cur.y <= s.bottom {
		minY, maxY = s.top, s.bottom
	}
	s.cur.y = min(max(s.cur.y+n, minY), maxY)
	s.cur.wrapNext = false
}

func (s *screen) moveCols(n int) {
	s.cur.x = min(max(s.cur.x+n, 0), s.cols-1)
	s.cur.wrapNext = false
}

func (s *screen) eraseCells(y, from, to int) {
	row := s.lines()[y].cells
	from, to = max(from, 0), min(to, s.cols)
	if from >= to {
		return
	}
	s.clearWide(row, from)
	s.clearWide(row, to-1)
	blank := s.blank()
	for x := from; x < to; x++ {
		row[x] = blank
	}
}

func (s *screen) eraseLines(from, to int) {
	lines := s.lines()
	for y := max(from, 0); y < min(to, s.rows); y++ {
		lines[y] = s.clearLine(lines[y])
	}
}

func (s *screen) csi(cmd ansi.Cmd, params ansi.Params) {
	n := func(i int) int { // a count: missing or 0 is 1
		v, _, _ := params.Param(i, 1)
		return max(v, 1)
	}
	switch cmd.Prefix() {
	case '?':
		s.privateCSI(cmd, params)
		return
	case '>':
		s.xtermCSI(cmd, params)
		return
	case '<', '=':
		if cmd.Final() == 'u' {
			s.kittyCSI(cmd, params)
		}
		return
	}
	if cmd.Intermediate() != 0 {
		s.intermediateCSI(cmd, params)
		return
	}
	lines := s.lines()
	switch cmd.Final() {
	case '@': // ICH
		row := lines[s.cur.y].cells
		count := min(n(0), s.cols-s.cur.x)
		s.clearWide(row, s.cur.x)
		copy(row[s.cur.x+count:], row[s.cur.x:])
		s.eraseCells(s.cur.y, s.cur.x, s.cur.x+count)
		s.cur.wrapNext = false
	case 'A': // CUU
		s.moveRows(-n(0))
	case 'B', 'e': // CUD, VPR
		s.moveRows(n(0))
	case 'C', 'a': // CUF, HPR
		s.moveCols(n(0))
	case 'D': // CUB
		s.moveCols(-n(0))
	case 'E': // CNL
		s.moveRows(n(0))
		s.cur.x = 0
	case 'F': // CPL
		s.moveRows(-n(0))
		s.cur.x = 0
	case 'G', '`': // CHA, HPA
		s.cur.x, s.cur.wrapNext = min(n(0)-1, s.cols-1), false
	case 'H', 'f': // CUP
		s.moveTo(n(1)-1, n(0)-1)
	case 'I': // CHT
		s.tab(n(0))
	case 'Z': // CBT
		s.backTab(n(0))
	case 'J': // ED
		s.eraseDisplay(params)
	case 'K': // EL
		mode, _, _ := params.Param(0, 0)
		switch mode {
		case 0:
			s.eraseCells(s.cur.y, s.cur.x, s.cols)
		case 1:
			s.eraseCells(s.cur.y, 0, s.cur.x+1)
		case 2:
			s.eraseCells(s.cur.y, 0, s.cols)
		}
		s.cur.wrapNext = false
	case 'L': // IL
		if s.cur.y >= s.top && s.cur.y <= s.bottom {
			s.rotateDown(s.cur.y, s.bottom, min(n(0), s.bottom-s.cur.y+1))
			s.cur.x, s.cur.wrapNext = 0, false
		}
	case 'M': // DL
		if s.cur.y >= s.top && s.cur.y <= s.bottom {
			// Deleted lines don't go into the history: they're not scrolled off.
			s.rotateUp(s.cur.y, s.bottom, min(n(0), s.bottom-s.cur.y+1))
			s.cur.x, s.cur.wrapNext = 0, false
		}
	case 'P': // DCH
		row := lines[s.cur.y].cells
		count := min(n(0), s.cols-s.cur.x)
		s.clearWide(row, s.cur.x)
		s.clearWide(row, s.cur.x+count-1)
		copy(row[s.cur.x:], row[s.cur.x+count:])
		s.eraseCells(s.cur.y, s.cols-count, s.cols)
		s.cur.wrapNext = false
	case 'S': // SU
		s.scrollUp(n(0))
	case 'T': // SD
		if len(params) <= 1 {
			s.scrollDown(n(0))
		}
	case 'X': // ECH
		s.eraseCells(s.cur.y, s.cur.x, s.cur.x+n(0))
		s.cur.wrapNext = false
	case 'b': // REP
		if s.lastPrinted != 0 {
			for range min(n(0), s.cols*s.rows) {
				s.print(s.lastPrinted)
			}
		}
	case 'd': // VPA
		s.moveTo(s.cur.x, n(0)-1)
	case 'g': // TBC
		mode, _, _ := params.Param(0, 0)
		switch mode {
		case 0:
			s.tabs[s.cur.x] = false
		case 3:
			clear(s.tabs)
		}
	case 'h', 'l': // SM, RM
		set := cmd.Final() == 'h'
		params.ForEach(0, func(_, mode int, _ bool) {
			switch mode {
			case 4:
				s.insert = set
			case 20:
				s.newline = set
			}
		})
	case 'm': // SGR
		s.cur.pen = applySGR(s.cur.pen, params)
	case 'r': // DECSTBM
		top, _, _ := params.Param(0, 1)
		bottom, _, _ := params.Param(1, s.rows)
		top, bottom = max(top, 1)-1, min(max(bottom, 1), s.rows)-1
		if top < bottom {
			s.top, s.bottom = top, bottom
			s.moveTo(0, 0)
		}
	case 's': // SCOSC
		s.saveCursor()
	case 'u': // SCORC
		s.restoreCursor()
	}
}

func (s *screen) eraseDisplay(params ansi.Params) {
	mode, _, _ := params.Param(0, 0)
	switch mode {
	case 0:
		s.eraseCells(s.cur.y, s.cur.x, s.cols)
		s.eraseLines(s.cur.y+1, s.rows)
	case 1:
		s.eraseLines(0, s.cur.y)
		s.eraseCells(s.cur.y, 0, s.cur.x+1)
	case 2:
		s.eraseLines(0, s.rows)
	case 3:
		s.history.clear()
	}
	s.cur.wrapNext = false
}

func (s *screen) privateCSI(cmd ansi.Cmd, params ansi.Params) {
	switch cmd.Final() {
	case 'h', 'l':
		set := cmd.Final() == 'h'
		params.ForEach(0, func(_, mode int, _ bool) { s.setPrivateMode(mode, set) })
	case 'J':
		s.eraseDisplay(params)
	case 'K':
		s.csi(ansi.Cmd(ansi.Command(0, 0, 'K')), params)
	}
}

func (s *screen) setPrivateMode(mode int, set bool) {
	switch mode {
	case 6:
		s.cur.origin = set
		s.moveTo(0, 0)
	case 7:
		s.autowrap = set
		if !set {
			s.cur.wrapNext = false
		}
	case 25:
		s.cursorVisible = set
	case 47, 1047:
		s.switchScreen(set, mode, false)
	case 1049:
		s.switchScreen(set, mode, true)
	default:
		for _, restored := range restoredModes {
			if mode == restored {
				if set {
					s.privateModes[mode] = true
				} else {
					delete(s.privateModes, mode)
				}
				return
			}
		}
	}
}

// switchScreen switches to the alternate screen or back. 1049 saves the cursor first, and
// clears the alternate screen; 1047 clears it on leaving it.
func (s *screen) switchScreen(alt bool, mode int, saveCursor bool) {
	if alt == s.altActive {
		return
	}
	if alt {
		if saveCursor {
			s.saveCursor()
		}
		s.altActive, s.altMode = true, mode
		if saveCursor {
			s.alt = newLines(s.cols, s.rows)
		}
		return
	}
	if mode == 1047 || mode == 1049 {
		s.alt = newLines(s.cols, s.rows)
	}
	s.altActive, s.altMode = false, 0
	if saveCursor {
		s.restoreCursor()
	}
}

func (s *screen) xtermCSI(cmd ansi.Cmd, params ansi.Params) {
	switch cmd.Final() {
	case 'm': // XTMODKEYS
		resource, _, _ := params.Param(0, 0)
		value, _, ok := params.Param(1, 0)
		if resource == 4 {
			if !ok {
				value = 0
			}
			s.modifyKeys = value
		}
	case 'u': // kitty keyboard protocol: push flags
		flags, _, _ := params.Param(0, 0)
		if len(s.kittyKeys) >= maxKittyKeys {
			s.kittyKeys = s.kittyKeys[1:]
		}
		s.kittyKeys = append(s.kittyKeys, flags)
	}
}

func (s *screen) kittyCSI(cmd ansi.Cmd, params ansi.Params) {
	switch cmd.Prefix() {
	case '<': // pop
		count, _, _ := params.Param(0, 1)
		s.kittyKeys = s.kittyKeys[:max(len(s.kittyKeys)-max(count, 1), 0)]
	case '=': // set the current flags
		flags, _, _ := params.Param(0, 0)
		how, _, _ := params.Param(1, 1)
		if len(s.kittyKeys) == 0 {
			s.kittyKeys = []int{0}
		}
		top := &s.kittyKeys[len(s.kittyKeys)-1]
		switch how {
		case 1:
			*top = flags
		case 2:
			*top |= flags
		case 3:
			*top &^= flags
		}
	}
}

func (s *screen) intermediateCSI(cmd ansi.Cmd, params ansi.Params) {
	switch {
	case cmd.Intermediate() == ' ' && cmd.Final() == 'q': // DECSCUSR
		s.cursorStyle, _, _ = params.Param(0, 0)
	case cmd.Intermediate() == '!' && cmd.Final() == 'p': // DECSTR
		s.softReset()
	}
}

// softReset is DECSTR: modes back to their defaults, the screen's content left alone.
func (s *screen) softReset() {
	s.cur.pen, s.cur.origin, s.cur.charsets, s.cur.shifted, s.cur.wrapNext = pen{}, false, [2]bool{}, false, false
	s.top, s.bottom = 0, s.rows-1
	s.autowrap, s.insert, s.cursorVisible, s.appKeypad = true, false, true, false
	delete(s.privateModes, 1)
	s.saved[s.screenIndex()] = savedCursor{}
}

func (s *screen) screenIndex() int {
	if s.altActive {
		return 1
	}
	return 0
}

func (s *screen) saveCursor() {
	s.saved[s.screenIndex()] = savedCursor{cursor: s.cur, set: true}
}

func (s *screen) restoreCursor() {
	saved := s.saved[s.screenIndex()]
	if !saved.set {
		s.cur = cursor{}
		return
	}
	s.cur = saved.cursor
	s.cur.x, s.cur.y = min(s.cur.x, s.cols-1), min(s.cur.y, s.rows-1)
}

func (s *screen) esc(cmd ansi.Cmd) {
	switch inter, final := cmd.Intermediate(), cmd.Final(); {
	case inter == '(' || inter == ')':
		s.cur.charsets[inter-'('] = final == '0'
	case inter == '#' && final == '8': // DECALN
		for y := range s.rows {
			for x := range s.cols {
				s.lines()[y].cells[x] = cell{r: 'E', width: 1}
			}
		}
	case inter != 0:
	case final == '7':
		s.saveCursor()
	case final == '8':
		s.restoreCursor()
	case final == 'D': // IND
		s.index()
	case final == 'E': // NEL
		s.index()
		s.cur.x = 0
	case final == 'M': // RI
		s.reverseIndex()
	case final == 'H': // HTS
		s.tabs[s.cur.x] = true
	case final == 'c': // RIS
		s.reset(s.cols, s.rows)
	case final == '=': // DECKPAM
		s.appKeypad = true
	case final == '>': // DECKPNM
		s.appKeypad = false
	}
}

func (s *screen) osc(cmd int, data []byte) {
	if cmd != 0 && cmd != 2 {
		return
	}
	// data is the whole string, "2;title".
	for i, b := range data {
		if b == ';' {
			title := data[i+1:]
			s.title = string(title[:min(len(title), maxTitle)])
			return
		}
	}
}

// resize sets the screen's size. Lines aren't reflowed: they're cut or padded, as the
// programs drawing them redraw at the new size. A shorter normal screen scrolls lines into
// the history to keep the cursor's line in view.
func (s *screen) resize(cols, rows int) {
	cols, rows = clampSize(cols, rows)
	if cols == s.cols && rows == s.rows {
		return
	}
	if rows < s.rows {
		s.shrinkRows(rows)
	}
	for _, lines := range [][]line{s.main, s.alt} {
		for i := range lines {
			lines[i] = resizeLine(lines[i], cols, s.cols != cols)
		}
	}
	for len(s.main) < rows {
		s.main = append(s.main, line{cells: make([]cell, cols)})
		s.alt = append(s.alt, line{cells: make([]cell, cols)})
	}
	oldCols := s.cols
	s.cols, s.rows = cols, rows
	s.top, s.bottom = 0, rows-1
	tabs := defaultTabs(cols)
	copy(tabs, s.tabs[:min(oldCols, cols)])
	s.tabs = tabs
	clampCursor := func(c *cursor) {
		c.x, c.y = min(c.x, cols-1), min(c.y, rows-1)
		c.wrapNext = c.wrapNext && cols == oldCols
	}
	clampCursor(&s.cur)
	clampCursor(&s.saved[0].cursor)
	clampCursor(&s.saved[1].cursor)
}

// shrinkRows drops rows: blank ones below the cursor first, then the top ones, into the
// history for the normal screen.
func (s *screen) shrinkRows(rows int) {
	drop := s.rows - rows
	below := 0
	lines := s.lines()
	for y := s.rows - 1; y > s.cur.y && below < drop && lineBlank(lines[y]); y-- {
		below++
	}
	fromTop := drop - below
	if !s.altActive {
		for i := range fromTop {
			s.pushHistory(s.main[i])
		}
	}
	trim := func(lines []line, fromTop int) []line {
		return lines[fromTop : fromTop+rows]
	}
	if s.altActive {
		s.alt = trim(s.alt, fromTop)
		s.main = trim(s.main, 0)
	} else {
		s.main = trim(s.main, fromTop)
		s.alt = trim(s.alt, 0)
	}
	s.cur.y = max(s.cur.y-fromTop, 0)
}

func lineBlank(l line) bool {
	for _, c := range l.cells {
		if c.r != 0 || c.pen.bg != colorDefault || c.pen.attrs&attrReverse != 0 {
			return false
		}
	}
	return true
}

func resizeLine(l line, cols int, widthChanged bool) line {
	cells := make([]cell, cols)
	copy(cells, l.cells)
	if cols < len(l.cells) && cells[cols-1].width == 2 {
		cells[cols-1] = cell{pen: cells[cols-1].pen}
	}
	// A wrapped line no longer fills the new width, or overflows it: it's ended there.
	return line{cells: cells, wrapped: l.wrapped && !widthChanged}
}

// applySGR applies SGR parameters to p.
func applySGR(p pen, params ansi.Params) pen {
	if len(params) == 0 {
		return pen{}
	}
	for i := 0; i < len(params); i++ {
		v := params[i].Param(0)
		switch {
		case v == 0:
			p = pen{}
		case v == 1:
			p.attrs |= attrBold
		case v == 2:
			p.attrs |= attrFaint
		case v == 3:
			p.attrs |= attrItalic
		case v == 4:
			p.underline = 1
			if params[i].HasMore() && i+1 < len(params) {
				i++
				p.underline = uint8(min(max(params[i].Param(1), 0), 5)) //nolint:gosec // 0 to 5
			}
		case v == 5:
			p.attrs |= attrBlink
		case v == 6:
			p.attrs |= attrRapidBlink
		case v == 7:
			p.attrs |= attrReverse
		case v == 8:
			p.attrs |= attrConceal
		case v == 9:
			p.attrs |= attrStrike
		case v == 21:
			p.underline = 2
		case v == 22:
			p.attrs &^= attrBold | attrFaint
		case v == 23:
			p.attrs &^= attrItalic
		case v == 24:
			p.underline = 0
		case v == 25:
			p.attrs &^= attrBlink | attrRapidBlink
		case v == 27:
			p.attrs &^= attrReverse
		case v == 28:
			p.attrs &^= attrConceal
		case v == 29:
			p.attrs &^= attrStrike
		case v >= 30 && v <= 37:
			p.fg = colorIndexed | color(v-30)
		case v == 38, v == 48, v == 58:
			var c color
			c, i = extendedColor(params, i)
			switch v {
			case 38:
				p.fg = c
			case 48:
				p.bg = c
			default:
				p.ulColor = c
			}
		case v == 39:
			p.fg = colorDefault
		case v >= 40 && v <= 47:
			p.bg = colorIndexed | color(v-40)
		case v == 49:
			p.bg = colorDefault
		case v == 53:
			p.attrs |= attrOverline
		case v == 55:
			p.attrs &^= attrOverline
		case v == 59:
			p.ulColor = colorDefault
		case v >= 90 && v <= 97:
			p.fg = colorIndexed | color(v-90+8)
		case v >= 100 && v <= 107:
			p.bg = colorIndexed | color(v-100+8)
		}
	}
	return p
}

// extendedColor reads the color of the 38, 48 or 58 at params[i], given as 5;n or 2;r;g;b,
// with semicolons or colons (2::r:g:b with a color space). It returns the color and the
// index of its last parameter.
func extendedColor(params ansi.Params, i int) (color, int) {
	colon := params[i].HasMore()
	next := func() (int, bool) {
		if i+1 >= len(params) {
			return 0, false
		}
		i++
		return params[i].Param(0), true
	}
	kind, ok := next()
	if !ok {
		return colorDefault, i
	}
	switch kind {
	case 5:
		n, ok := next()
		if !ok {
			return colorDefault, i
		}
		return colorIndexed | color(n&0xff), i
	case 2:
		if colon && i+4 < len(params) && params[i+3].HasMore() {
			i++ // the color space, in 2:cs:r:g:b
		}
		r, _ := next()
		g, _ := next()
		b, _ := next()
		return colorRGB | color(r&0xff)<<16 | color(g&0xff)<<8 | color(b&0xff), i
	}
	return colorDefault, i
}

// decGraphics is DEC special graphics, for 0x5f to 0x7e.
var decGraphics = [...]rune{ //nolint:gochecknoglobals // a read-only table
	' ', '◆', '▒', '␉', '␌', '␍', '␊', '°', '±', '␤', '␋', '┘', '┐', '┌', '└', '┼',
	'⎺', '⎻', '─', '⎼', '⎽', '├', '┤', '┴', '┬', '│', '≤', '≥', 'π', '≠', '£', '·',
}
