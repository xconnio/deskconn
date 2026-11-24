package terminal_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	. "github.com/xconnio/deskconn/internal/terminal"
)

const historyLimit = 1 << 20

// restored is a fresh screen of s's size after s's snapshot: what a client attaching shows.
func restored(t *testing.T, s *Screen, cols, rows int) *Screen {
	t.Helper()
	snapshot, _ := s.Snapshot(nil)
	client := NewScreen(cols, rows, historyLimit)
	client.Write(snapshot)
	return client
}

// requireSame checks that client shows what s does, and would go on to the same.
func requireSame(t *testing.T, s, client *Screen) {
	t.Helper()
	require.Equal(t, s.MainText(), client.MainText(), "normal screen")
	require.Equal(t, s.Text(), client.Text(), "active screen")
	require.Equal(t, s.History(), client.History(), "history")
	require.Equal(t, s.State(), client.State(), "cursor, modes and pens")
}

// sessionOutput is output exercising what a screen keeps: colors, Unicode, a scroll
// region, the alternate screen, modes and a long wrapped line.
var sessionOutput = []string{ //nolint:gochecknoglobals // read-only test data
	"plain\r\n",
	"\x1b[1;31mbold red\x1b[0m \x1b[38;5;208morange\x1b[39m \x1b[48;2;10;20;30mrgb bg\x1b[0m\r\n",
	"\x1b[4:3;58:2::255:0:0mcurly\x1b[0m café 中文 é 🙂 end\r\n",
	strings.Repeat("wrapped-", 30) + "\r\n",
	"\x1b]2;my title\x07",
	"\x1b[?2004h\x1b[?1h\x1b=\x1b[?1000;1006h\x1b[>4;2m\x1b[>1u\x1b[5 q",
	"\x1b(0lqqk\x1b(B box\r\n",
	"tab\there\tand\there\r\n",
	"\x1b[?1049h\x1b[H\x1b[2Jalternate\x1b[5;10Hmiddle\x1b[3;20r\x1b[20;1Hscrolled\r\n",
}

func TestSnapshotRestoresScreen(t *testing.T) {
	for n := 1; n <= len(sessionOutput); n++ {
		t.Run(fmt.Sprintf("after %d writes", n), func(t *testing.T) {
			s := NewScreen(40, 12, historyLimit)
			for i := 0; i < 30; i++ {
				s.Write(fmt.Appendf(nil, "history line %d\r\n", i))
			}
			for _, out := range sessionOutput[:n] {
				s.Write([]byte(out))
			}
			requireSame(t, s, restored(t, s, 40, 12))
		})
	}
}

func TestSnapshotThenOutputMatches(t *testing.T) {
	s := NewScreen(40, 12, historyLimit)
	for _, out := range sessionOutput {
		s.Write([]byte(out))
	}
	client := restored(t, s, 40, 12)
	// What follows the snapshot does the same on the client: leaving the alternate screen
	// returns to the normal one, writing at a pending wrap wraps, and so on.
	more := "\x1b[r\x1b[?1049l\x1b[?7h" + strings.Repeat("x", 45) + "\r\nnext\r\n\x1b[?2004l\x1b[<u"
	s.Write([]byte(more))
	client.Write([]byte(more))
	requireSame(t, s, client)
	require.Contains(t, client.Text(), "box", "the normal screen is back")
}

func TestSplitWritesMatchWholeWrites(t *testing.T) {
	whole := NewScreen(40, 12, historyLimit)
	split := NewScreen(40, 12, historyLimit)
	for _, out := range sessionOutput {
		whole.Write([]byte(out))
		// Every escape sequence and multibyte character is split across writes.
		for i := 0; i < len(out); i++ {
			split.Write([]byte{out[i]})
		}
	}
	requireSame(t, whole, split)
	require.Equal(t, whole.Offset(), split.Offset())
}

func TestUnicode(t *testing.T) {
	s := NewScreen(10, 3, historyLimit)
	s.Write([]byte("中文é"))
	x, _ := s.Cursor()
	require.Equal(t, 5, x, "wide characters take two columns, combining marks none")
	require.Equal(t, "中文e\u0301", strings.Split(s.Text(), "\n")[0])

	// A wide character that doesn't fit in the last column goes on the next line.
	s.Write([]byte("\r\n123456789中"))
	require.Equal(t, []string{"中文e\u0301", "123456789", "中"}, strings.Split(s.Text(), "\n"))

	// Overwriting half of a wide character blanks the other half.
	s.Write([]byte("\x1b[1;2Hx"))
	require.Equal(t, " x文e\u0301", strings.Split(s.Text(), "\n")[0])
}

func TestAlternateScreenKeepsNoHistory(t *testing.T) {
	s := NewScreen(20, 5, historyLimit)
	s.Write([]byte("before\r\n"))
	s.Write([]byte("\x1b[?1049h"))
	for i := 0; i < 50; i++ {
		s.Write(fmt.Appendf(nil, "redraw %d\r\n", i))
	}
	require.True(t, s.AltActive())
	require.Empty(t, s.History(), "the alternate screen's scrolling isn't history")
	s.Write([]byte("\x1b[?1049l"))
	require.Equal(t, "before", strings.Split(s.Text(), "\n")[0], "the normal screen is as it was")
}

func TestHistoryIsBounded(t *testing.T) {
	const limit = 64 << 10
	s := NewScreen(80, 24, limit)
	line := strings.Repeat("0123456789", 7) + "\r\n"
	for i := 0; i < 20000; i++ {
		s.Write([]byte(line))
	}
	require.LessOrEqual(t, s.HistorySize(), limit)
	require.Positive(t, s.HistoryDropped())
	history := s.History()
	require.NotEmpty(t, history)
	require.Equal(t, strings.TrimSuffix(line, "\r\n"), history[len(history)-1], "the newest lines are kept")
	_, gap := s.Snapshot(nil)
	require.True(t, gap, "evicted history is a gap")

	// Blank lines cost their place: endless ones keep the history full, not empty.
	s = NewScreen(80, 24, limit)
	for i := 0; i < 2000; i++ {
		s.Write([]byte(strings.Repeat("\r\n", 100)))
	}
	require.LessOrEqual(t, s.HistorySize(), limit)
	require.Greater(t, len(s.History()), limit/50)

	// One endless line is bounded too.
	s = NewScreen(80, 24, limit)
	for i := 0; i < 20000; i++ {
		s.Write([]byte(line[:70]))
	}
	require.LessOrEqual(t, s.HistorySize(), limit)
}

func TestHistorySince(t *testing.T) {
	s := NewScreen(20, 3, historyLimit)
	for i := 0; i < 5; i++ {
		s.Write(fmt.Appendf(nil, "line %d\r\n", i))
	}
	seen := s.Offset()
	for i := 5; i < 8; i++ {
		s.Write(fmt.Appendf(nil, "line %d\r\n", i))
	}
	// A client that saw the output up to seen has the lines that had scrolled off by then.
	snapshot, gap := s.Snapshot(&seen)
	require.False(t, gap)
	client := NewScreen(20, 3, historyLimit)
	client.Write(snapshot)
	require.Equal(t, []string{"line 3", "line 4", "line 5"}, client.History())
	require.Equal(t, s.Text(), client.Text())

	// Lines it missed that are gone are a gap.
	s = NewScreen(20, 3, 200)
	start := s.Offset()
	for i := 0; i < 50; i++ {
		s.Write(fmt.Appendf(nil, "line %d\r\n", i))
	}
	_, gap = s.Snapshot(&start)
	require.True(t, gap)
	now := s.Offset()
	_, gap = s.Snapshot(&now)
	require.False(t, gap, "nothing it missed is gone")
}

func TestClearScrollback(t *testing.T) {
	s := NewScreen(20, 3, historyLimit)
	for i := 0; i < 10; i++ {
		s.Write(fmt.Appendf(nil, "line %d\r\n", i))
	}
	s.Write([]byte("\x1b[3J"))
	require.Empty(t, s.History())
	_, gap := s.Snapshot(nil)
	require.False(t, gap, "history the program cleared isn't a gap")
}

func TestResize(t *testing.T) {
	s := NewScreen(20, 5, historyLimit)
	s.Write([]byte("one\r\ntwo\r\nthree\r\nfour\r\n$ "))

	// Fewer rows: the top ones scroll into the history, keeping the cursor's line.
	s.Resize(20, 3)
	require.Equal(t, "three\nfour\n$", s.Text())
	require.Equal(t, []string{"one", "two"}, s.History())
	x, y := s.Cursor()
	require.Equal(t, [2]int{2, 2}, [2]int{x, y})

	// Narrower: lines are cut, the cursor kept in.
	s.Resize(3, 3)
	require.Equal(t, "thr\nfou\n$", s.Text())

	// Larger: blank space added.
	s.Resize(30, 6)
	require.Equal(t, "thr\nfou\n$\n\n\n", s.Text())
	requireSame(t, s, restored(t, s, 30, 6))

	// A client of another size is shown the screen at the size it's resized to.
	s.Resize(25, 4)
	requireSame(t, s, restored(t, s, 25, 4))

	// Absurd sizes are bounded.
	s.Resize(100000, 100000)
	big := restored(t, s, 100000, 100000)
	require.Equal(t, s.Text(), big.Text())
}

func TestShrinkDropsBlankRowsBelowCursor(t *testing.T) {
	s := NewScreen(20, 6, historyLimit)
	s.Write([]byte("top\r\n$ "))
	s.Resize(20, 3)
	require.Equal(t, "top\n$\n", s.Text(), "blank rows below the cursor go first")
	require.Empty(t, s.History())
}

func TestResetAndSoftReset(t *testing.T) {
	s := NewScreen(20, 3, historyLimit)
	s.Write([]byte("\x1b[?1h\x1b[4h\x1b[31mtext\x1b[!p"))
	client := restored(t, s, 20, 3)
	requireSame(t, s, client)
	s.Write([]byte("\x1bc"))
	require.Equal(t, "\n\n", s.Text())
	requireSame(t, s, restored(t, s, 20, 3))
}

func TestGarbageDoesNotBreakScreen(t *testing.T) {
	s := NewScreen(20, 5, historyLimit)
	// Truncated and invalid sequences, invalid UTF-8, huge parameters and a long OSC.
	s.Write([]byte("\x1b[999999999;999999999H\x1b[99999@\x1b[99999P\x1b[99999L\x1b[99999M\x1b[99999S\x1b[99999T"))
	s.Write([]byte("\xff\xfe\xc3\x28\xe2\x82\x1b[\x1b]2;" + strings.Repeat("t", 100000) + "\x07"))
	s.Write([]byte("\x1b[38:2:1:2m\x1b[38;5m\x1b[38;2m\x1b[58;5;300m\x1b[0;0r\x1b[5;2r\x1b[99999b"))
	s.Write([]byte("\x1b[<99999u\x1b[=1;9u\x1b[3g\tafter"))
	requireSame(t, s, restored(t, s, 20, 5))
}

func BenchmarkScreen(b *testing.B) {
	s := NewScreen(120, 40, 16<<20)
	chunk := []byte(strings.Repeat("\x1b[1;32mok\x1b[0m the quick brown fox jumps over the lazy dog 0123456789\r\n", 500))
	b.SetBytes(int64(len(chunk)))
	for b.Loop() {
		s.Write(chunk)
	}
}
