package top_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/cmd/deskconn/top"
	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
)

const (
	labProfile = "lab"
	bob        = "bob"
	keyEnter   = "enter"
	keyEsc     = "esc"
	width      = 120
	height     = 30
	authFailed = "wamp.error.authentication_failed"
)

// fakeRemote stands in for the machines desk top acts on.
type fakeRemote struct {
	mu      sync.Mutex
	results map[string]any // what Call returns, by procedure
	err     error          // what Call and Ping fail with, if set
	calls   []string       // the procedures Call was called with, each with its args as JSON

	logLines  chan string // what Logs writes, a chunk at a time; closing it ends the log
	logErr    error       // what the log ends with
	logSource string      // the source Logs was last asked for

	forwardErrs map[string]error // what Forward fails with at once, by listen port; others run until stopped
	forwards    []string         // the forwards started, as "realm forward|reverse listen:to"
	stopped     atomic.Int32     // the forwards stopped
}

func newFakeRemote() *fakeRemote {
	return &fakeRemote{results: map[string]any{}, logLines: make(chan string, 200), forwardErrs: map[string]error{}}
}

func (f *fakeRemote) set(apply func(f *fakeRemote)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	apply(f)
}

func (f *fakeRemote) Call(_, procedure string, args, result any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	encoded, _ := json.Marshal(args)
	f.calls = append(f.calls, procedure+" "+string(encoded))
	if f.err != nil {
		return f.err
	}
	data, err := json.Marshal(f.results[procedure])
	if err != nil {
		return err
	}
	return json.Unmarshal(data, result)
}

// callsOf are the args, as JSON, of each call of procedure.
func (f *fakeRemote) callsOf(procedure string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var calls []string
	for _, call := range f.calls {
		if strings.HasPrefix(call, procedure+" ") {
			calls = append(calls, strings.TrimPrefix(call, procedure+" "))
		}
	}
	return calls
}

func (f *fakeRemote) Ping(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *fakeRemote) Logs(ctx context.Context, _, _, source string, out io.Writer) error {
	f.mu.Lock()
	f.logSource = source
	lines := f.logLines
	f.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case line, ok := <-lines:
			if !ok {
				f.mu.Lock()
				defer f.mu.Unlock()
				return f.logErr
			}
			if _, err := io.WriteString(out, line); err != nil {
				return err
			}
		}
	}
}

func (f *fakeRemote) Forward(ctx context.Context, _, realm string, reverse bool, listen, to string) error {
	f.mu.Lock()
	direction := "forward"
	if reverse {
		direction = "reverse"
	}
	f.forwards = append(f.forwards, realm+" "+direction+" "+listen+":"+to)
	err := f.forwardErrs[listen]
	f.mu.Unlock()
	if err != nil {
		return err
	}
	<-ctx.Done()
	f.stopped.Add(1)
	return ctx.Err()
}

func (f *fakeRemote) Close() {}

// driver runs desk top as Bubble Tea does: keys and messages go to the model one at a time,
// and the commands it returns run on their own, their messages coming back to it.
type driver struct {
	t      *testing.T
	m      *top.Model
	remote *fakeRemote
	msgs   chan tea.Msg
	quit   atomic.Bool
}

// newDriver is desk top on a config with two machines (and an empty entry, which desk ls
// skips too) and the profiles, on a width×height terminal.
func newDriver(t *testing.T, profiles ...common.Profile) *driver {
	t.Helper()
	cfgDirectory := t.TempDir()
	require.NoError(t, deskconn.CacheMachines(cfgDirectory, []common.Machine{
		{Authid: "aaaa1111bbbb", Name: "office", Alias: "work", Realm: "realm.office"},
		{Authid: "cccc2222dddd", Name: "laptop", Realm: "realm.laptop"},
		{},
	}))
	if len(profiles) == 0 {
		profiles = []common.Profile{{Name: labProfile, URL: "quic://10.0.0.5:18080", AuthID: bob, PrivateKey: "k"}}
	}
	for _, profile := range profiles {
		require.NoError(t, deskconn.AddProfile(cfgDirectory, profile))
	}

	d := &driver{t: t, remote: newFakeRemote(), msgs: make(chan tea.Msg, 1000)}
	d.m = top.NewModel(cfgDirectory, "v-test", d.remote)
	t.Cleanup(d.m.Close)
	d.m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	d.run(d.m.Init())
	d.settle()
	return d
}

// run runs cmd on its own, as Bubble Tea does.
func (d *driver) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		switch msg := cmd().(type) {
		case nil:
		case tea.BatchMsg:
			for _, cmd := range msg {
				d.run(cmd)
			}
		case tea.QuitMsg:
			d.quit.Store(true)
		default:
			d.msgs <- msg
		}
	}()
}

// settle hands the model the messages that come until none has for a moment.
func (d *driver) settle() {
	for {
		select {
		case msg := <-d.msgs:
			_, cmd := d.m.Update(msg)
			d.run(cmd)
		case <-time.After(50 * time.Millisecond):
			return
		}
	}
}

// wait settles for as long as duration.
func (d *driver) wait(duration time.Duration) {
	end := time.Now().Add(duration)
	for time.Now().Before(end) {
		d.settle()
	}
}

// press presses each of keys: a key by its name (enter, esc, ctrl+d, …), or the characters
// typed, which arrive together.
func (d *driver) press(keys ...string) {
	named := map[string]tea.KeyType{
		keyEnter: tea.KeyEnter, keyEsc: tea.KeyEsc, "backspace": tea.KeyBackspace, "tab": tea.KeyTab,
		"right": tea.KeyRight, "down": tea.KeyDown, "ctrl+c": tea.KeyCtrlC, "ctrl+d": tea.KeyCtrlD,
		"ctrl+k": tea.KeyCtrlK,
	}
	for _, key := range keys {
		msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
		if keyType, ok := named[key]; ok {
			msg = tea.KeyMsg{Type: keyType}
		}
		_, cmd := d.m.Update(msg)
		d.run(cmd)
	}
	d.settle()
}

func (d *driver) paste(text string) {
	_, cmd := d.m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text), Paste: true})
	d.run(cmd)
	d.settle()
}

// screen is what's on the screen, checked to be exactly the terminal's size.
func (d *driver) screen() string {
	d.t.Helper()
	view := d.m.View()
	lines := strings.Split(view, "\n")
	require.Len(d.t, lines, height, view)
	for _, line := range lines {
		require.LessOrEqual(d.t, lipgloss.Width(line), width, line)
	}
	return ansi.Strip(view)
}

// selected is the name of the machine or profile the cursor is on, as the port forward
// dialog has it.
func (d *driver) selected() string {
	d.t.Helper()
	d.press("F")
	match := regexp.MustCompile(`Forward a port to or from (\S+)\.`).FindStringSubmatch(d.screen())
	require.NotNil(d.t, match, d.screen())
	d.press(keyEsc)
	return match[1]
}

// requireOrder checks each of words is on screen, in that order.
func requireOrder(t *testing.T, screen string, words ...string) {
	t.Helper()
	last := -1
	for _, word := range words {
		i := strings.Index(screen, word)
		require.Greater(t, i, last, "%q isn't after %v in:\n%s", word, words, screen)
		last = i
	}
}

func TestTopMachines(t *testing.T) {
	d := newDriver(t)
	screen := d.screen()
	for _, want := range []string{"Machines[3]", "office", "work", "laptop", labProfile, "quic://10.0.0.5:18080",
		"key as bob", "<s> Shell", "<shift-f> Port forward", "<machines>", "Version:  v-test",
		"Daemon:   not running"} {
		require.Contains(t, screen, want)
	}
	requireOrder(t, screen, "office", "laptop", "quic://")

	empty := top.NewModel(t.TempDir(), "", newFakeRemote())
	empty.Update(tea.WindowSizeMsg{Width: width, Height: height})
	require.Contains(t, ansi.Strip(empty.View()), "desk account login", "points the way when there's nothing")
}

func TestTopFilter(t *testing.T) {
	d := newDriver(t)
	d.press("/", "w", "o", "r", "k")
	screen := d.screen()
	require.Contains(t, screen, "Machines(/work)[1]", "matches the alias")
	require.NotContains(t, screen, "laptop")

	d.press(keyEnter)
	require.Contains(t, d.screen(), "Machines(/work)[1]", "enter keeps the filter")
	d.press(keyEsc)
	require.Contains(t, d.screen(), "Machines[3]", "esc clears it")

	d.press("/", "1", "0", ".", "0")
	require.Contains(t, d.screen(), "Machines(/10.0)[1]", "matches a profile's URL")
	d.press("backspace")
	require.Contains(t, d.screen(), "Machines(/10.)[1]")
	d.press(keyEsc)
	require.Contains(t, d.screen(), "Machines[3]", "esc while typing drops the filter")

	d.press("/")
	d.paste("laptop")
	require.Contains(t, d.screen(), "Machines(/laptop)[1]", "a paste goes in whole")
}

func TestTopMove(t *testing.T) {
	d := newDriver(t)
	d.press("k")
	require.Equal(t, "office", d.selected(), "stays on the list at the top")
	d.press("j", "down")
	require.Equal(t, labProfile, d.selected())
	d.press("j")
	require.Equal(t, labProfile, d.selected(), "and at the bottom")
	d.press("g")
	require.Equal(t, "office", d.selected())
	d.press("G")
	require.Equal(t, labProfile, d.selected())

	d.press("g", "jj")
	require.Equal(t, labProfile, d.selected(), "keys that arrive together count one by one")
	d.press("/lab")
	require.Contains(t, d.screen(), "Machines(/lab)[1]", "the rest of the keys go to the filter")
}

func TestTopCommands(t *testing.T) {
	d := newDriver(t)
	d.press(":", "p", "f", keyEnter)
	screen := d.screen()
	require.Contains(t, screen, "No port forwards.")
	require.Contains(t, screen, "<port forwards>")

	d.press(keyEsc, "f")
	require.Contains(t, d.screen(), "PortForwards[0]", "f on the machines shows them too, as in k9s")

	d.press(":", "m", "a", "tab", keyEnter)
	require.Contains(t, d.screen(), "Machines[3]", "tab completes, :machines goes back to them")

	d.press(":", "n", "o", "p", "e", keyEnter)
	require.Contains(t, d.screen(), `No command "nope"`)

	d.press("?")
	require.Contains(t, d.screen(), "GENERAL")
	d.press(keyEsc)
	require.NotContains(t, d.screen(), "GENERAL")

	d.press("q")
	require.True(t, d.quit.Load(), "q quits")
}

func TestTopPing(t *testing.T) {
	d := newDriver(t)
	d.press("G", "p")
	require.Regexp(t, `lab replied in \d+`, d.screen())

	d.remote.set(func(f *fakeRemote) { f.err = errors.New("timeout") })
	d.press("p")
	require.Contains(t, d.screen(), "Ping lab: timeout")
}

func TestTopForwarding(t *testing.T) {
	d := newDriver(t)
	d.press("G", "F")
	screen := d.screen()
	require.Contains(t, screen, "Forward a port to or from lab.")
	require.Contains(t, screen, "▸ Listen on   127.0.0.1:█", "starts on the port to listen on")
	require.Contains(t, screen, "on lab's localhost")

	d.press("8", "x", "0", "tab", "0", keyEnter)
	screen = d.screen()
	require.Contains(t, screen, "127.0.0.1:80 ", "only digits")
	require.Contains(t, screen, `invalid port "0"`, "port 0 is refused")
	d.press("backspace", "8", "0", keyEnter)
	require.Contains(t, d.screen(), "Forwarding 127.0.0.1:80 → lab:80.")

	d.remote.set(func(f *fakeRemote) { f.forwardErrs["9000"] = errors.New("address already in use") })
	d.press("F", "right")
	require.Contains(t, d.screen(), "on lab, all interfaces", "←/→ turns it around")
	d.press("9", "0", "0", "0", "tab", "3", "0", "0", "0", keyEnter)
	require.Contains(t, d.screen(), "Forward lab:9000 → 127.0.0.1:3000: address already in use")
	require.Equal(t, []string{common.ProfileRealm(labProfile) + " forward 80:80",
		common.ProfileRealm(labProfile) + " reverse 9000:3000"}, d.remote.forwards)
	require.Contains(t, d.screen(), "Forwards: 1")

	d.press("f")
	screen = d.screen()
	require.Contains(t, screen, "PortForwards[2]")
	table := screen[strings.Index(screen, "PortForwards["):]
	requireOrder(t, table, "local → remote", "127.0.0.1:80", "lab:80", "running",
		"remote → local", "lab:9000", "127.0.0.1:3000", "failed: address already in use")

	d.press("ctrl+d")
	require.Contains(t, d.screen(), "Stop forwarding 127.0.0.1:80 → lab:80?", "a running one asks first")
	d.press("y")
	require.Equal(t, int32(1), d.remote.stopped.Load())
	screen = d.screen()
	require.Contains(t, screen, "PortForwards[1]", "a stopped one goes")
	require.Contains(t, screen, "Stopped forwarding 127.0.0.1:80 → lab:80.")

	d.press("ctrl+d")
	require.Contains(t, d.screen(), "No port forwards.", "a failed one goes without asking")
}

func TestTopQuitWithForwards(t *testing.T) {
	d := newDriver(t)
	d.press("G", "F", "8", "0", "8", "0", "tab", "8", "0", keyEnter)

	d.press("q")
	require.Contains(t, d.screen(), "1 port forward(s) are running", "asks first")
	d.press("n")
	require.NotContains(t, d.screen(), "are running")
	require.False(t, d.quit.Load())

	d.press("F", "ctrl+c")
	screen := d.screen()
	require.Contains(t, screen, "1 port forward(s) are running", "from a dialog too")
	require.NotContains(t, screen, "Forward a port", "instead of the dialog")
	d.press("ctrl+c")
	require.True(t, d.quit.Load(), "a second ctrl+c quits")

	d.m.Close()
	require.Eventually(t, func() bool { return d.remote.stopped.Load() == 1 }, time.Second, 10*time.Millisecond,
		"quitting stops them")
}

// process is a process as a machine's process list has it.
type process struct {
	PID        int     `json:"pid"`
	Name       string  `json:"name"`
	User       string  `json:"user"`
	Cmdline    string  `json:"cmdline"`
	CPUPercent float64 `json:"cpu_percent"`
	MemRSS     uint64  `json:"mem_rss"`
}

// resources are a machine's resources and processes, as it reports them.
func resources() map[string]any {
	return map[string]any{
		common.ProcedureMachineInfo: common.MachineInfo{
			CPUModel: "Test CPU", CPUPhysical: 2, CPULogical: 4, CPUUsages: []float64{10, 55, 90, 0},
			RAMTotal: 16 << 30, RAMAvailable: 6 << 30, SwapTotal: 2 << 30, DiskTotal: 100 << 30, DiskUsed: 40 << 30,
			Battery: &common.BatteryInfo{Percentage: 80, Status: "Charging"},
		},
		common.ProcedureProcessList: []process{
			{PID: 30, Name: "zsh", User: bob, CPUPercent: 1, MemRSS: 10 << 20},
			{PID: 10, Name: "firefox", User: bob, CPUPercent: 60, MemRSS: 900 << 20, Cmdline: "/usr/bin/firefox"},
			{PID: 20, Name: "Xorg", User: "root", CPUPercent: 5, MemRSS: 2 << 30},
		},
		common.ProcedureProcessSignal: []map[string]any{{"pid": 20, "ok": true}},
	}
}

func TestTopResources(t *testing.T) {
	d := newDriver(t)
	d.remote.set(func(f *fakeRemote) { f.results = resources() })
	d.press("G", keyEnter)
	screen := d.screen()
	for _, want := range []string{"Resources(lab)[3]", "Mem[", "10.0G/16.0G", "Swp[", "Dsk[", "40.0G/100G",
		"Tasks: 3", "80% Charging", "Test CPU (2 cores, 4 threads)", "CPU%↓", "/usr/bin/firefox", "900M",
		"2.0G", "10.0M", "<resources>"} {
		require.Contains(t, screen, want)
	}

	requireOrder(t, screen, "firefox", "Xorg", "zsh")
	d.press("M")
	requireOrder(t, d.screen(), "Xorg", "firefox", "zsh")
	d.press("N")
	requireOrder(t, d.screen(), "firefox", "Xorg", "zsh")
	d.press("I")
	requireOrder(t, d.screen(), "firefox", "Xorg", "zsh")
	d.press("I")
	requireOrder(t, d.screen(), "zsh", "Xorg", "firefox")

	d.press("/", "r", "o", "o", "t", keyEnter)
	screen = d.screen()
	require.Contains(t, screen, "Resources(lab)(/root)[1]", "filters by user too")
	require.NotContains(t, screen, "firefox")

	d.press("t")
	require.Contains(t, d.screen(), "Terminate Xorg (20) on lab?")
	d.press("n")
	require.Empty(t, d.remote.callsOf(common.ProcedureProcessSignal), "no is no")
	d.press("t", "y")
	require.Contains(t, d.screen(), "Sent TERM to Xorg.")
	d.press("ctrl+k")
	require.Contains(t, d.screen(), "Kill Xorg (20) on lab?")
	d.remote.set(func(f *fakeRemote) {
		f.results[common.ProcedureProcessSignal] = []map[string]any{
			{"pid": 20, "ok": false, "error": "operation not permitted"}}
	})
	d.press("y")
	require.Contains(t, d.screen(), "KILL Xorg (20): operation not permitted")
	require.Equal(t, []string{`{"pids":[20],"signal":"term"}`, `{"pids":[20],"signal":"kill"}`},
		d.remote.callsOf(common.ProcedureProcessSignal))

	// It keeps up to date, every second at the shortest.
	d.press("-")
	require.Contains(t, d.screen(), "Updating every 1s.")
	fetched := len(d.remote.callsOf(common.ProcedureMachineInfo))
	d.wait(2500 * time.Millisecond)
	require.Greater(t, len(d.remote.callsOf(common.ProcedureMachineInfo)), fetched)

	d.remote.set(func(f *fakeRemote) { f.err = errors.New("connection lost") })
	d.wait(2500 * time.Millisecond)
	screen = d.screen()
	require.Contains(t, screen, "Lost lab: connection lost. Retrying…")
	require.Contains(t, screen, "Xorg", "keeping what it had")

	d.press(keyEsc, keyEsc)
	require.Contains(t, d.screen(), "Machines[3]", "esc clears the filter, then goes back")
}

func TestTopLogs(t *testing.T) {
	d := newDriver(t)
	d.press("G", "l")
	require.Contains(t, d.screen(), "Waiting for the journal of lab…")

	d.remote.logLines <- "alpha\nbra"
	d.settle()
	screen := d.screen()
	require.Contains(t, screen, "alpha")
	require.NotContains(t, screen, "bra", "a line isn't shown until it's whole")
	d.remote.logLines <- "vo\r\n\x1b[31mred\x1b[0m\tcharlie\n"
	d.settle()
	requireOrder(t, d.screen(), "alpha", "bravo", "red    charlie")

	for range 100 {
		d.remote.logLines <- "filler\n"
	}
	d.settle()
	require.Contains(t, d.screen(), "following")
	d.press("k")
	require.NotContains(t, d.screen(), "following", "scrolling up stops following")
	d.press("G")
	require.Contains(t, d.screen(), "following")

	d.press("c", "w")
	d.remote.logLines <- strings.Repeat("x", 250) + "\n"
	d.settle()
	require.Equal(t, 3, strings.Count(d.screen(), "│ xxxxxxxxxx"), "wraps to the width")

	d.press("o")
	d.paste("/var/log/syslog")
	d.press(keyEnter)
	require.Contains(t, d.screen(), "Logs(lab) /var/log/syslog")
	d.remote.mu.Lock()
	source := d.remote.logSource
	d.remote.mu.Unlock()
	require.Equal(t, "/var/log/syslog", source, "the log starts again from the source")

	d.remote.set(func(f *fakeRemote) { f.logErr = errors.New("no such file") })
	close(d.remote.logLines)
	d.settle()
	require.Contains(t, d.screen(), "The log ended: no such file. Press r to start it again.")
}

func TestTopPassword(t *testing.T) {
	d := newDriver(t, common.Profile{Name: "pw", URL: "quic://10.0.0.6:18080", AuthID: "alice"})
	d.remote.set(func(f *fakeRemote) { f.results = resources() })
	d.press("G", keyEnter)
	require.Contains(t, d.screen(), "Password for alice on pw")
	require.Empty(t, d.remote.callsOf(common.ProcedureMachineInfo), "not connecting yet")

	d.press(keyEnter)
	require.Contains(t, d.screen(), "Password for alice on pw", "not without a password")
	d.press("s", "e", "c", "r", "e", "t")
	require.NotContains(t, d.screen(), "secret", "the password isn't shown")
	d.press(keyEnter)
	require.Contains(t, d.screen(), "Resources(pw)", "then it goes on")

	target, ok := common.StandaloneTargetFor(common.ProfileRealm("pw"))
	require.True(t, ok)
	require.False(t, target.NeedsPassword())

	// Refused for its password: back to the machines, closing what was opened over the
	// resources too, and the password forgotten.
	d.press("-", "l")
	require.Contains(t, d.screen(), "<resources>   <logs>")
	d.remote.set(func(f *fakeRemote) { f.err = errors.New(authFailed) })
	d.wait(2500 * time.Millisecond)
	screen := d.screen()
	require.Contains(t, screen, "Machines[3]")
	require.Contains(t, screen, "Resources of pw: "+authFailed)
	require.True(t, target.NeedsPassword())

	d.press("p")
	require.Contains(t, d.screen(), "Password for alice on pw", "asked for again")
	d.press("x", keyEnter)
	require.Contains(t, d.screen(), "Ping pw: "+authFailed)
	require.True(t, target.NeedsPassword(), "a ping refused for it forgets it too")

	d.remote.set(func(f *fakeRemote) { f.err = errors.New("timeout") })
	d.press("p", "y", keyEnter)
	require.Contains(t, d.screen(), "Ping pw: timeout")
	require.False(t, target.NeedsPassword(), "but not a failure that isn't about it")
}
