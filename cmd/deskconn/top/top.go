// Package top is desk top: a terminal UI, like k9s, over the account's machines and the
// standalone profiles, showing their resources and logs and running shells and port forwards
// on them.
package top

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	log "github.com/sirupsen/logrus"
	"golang.org/x/term"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
)

// Run runs desk top: a full-screen list of the account's machines and the standalone
// profiles (from cfgDirectory), whose resources, logs, shells and port forwards it shows and
// runs itself. version is desk's, for the header.
func Run(cfgDirectory, version string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return errors.New("desk top needs a terminal")
	}

	// Anything logged would be drawn over the screen.
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)

	model := NewModel(cfgDirectory, version, newTopSessions(cfgDirectory))
	defer model.Close()
	_, err := tea.NewProgram(model, tea.WithAltScreen()).Run()
	return err
}

// topFlashFor is how long a message stays in the flash line.
const topFlashFor = 5 * time.Second

// Model is desk top's state, as a Bubble Tea model.
type Model struct {
	cfgDirectory string
	version      string
	styles       topStyles
	width        int
	height       int

	machines   []common.Machine
	profiles   []common.Profile
	targets    map[string]*common.StandaloneTarget // the profiles', by name
	connected  map[string]any                      // the daemon's connected machines, by realm
	daemonUp   bool
	loggedIn   bool
	refreshing bool

	remote   Remote
	forwards []*topForward
	nextID   int

	// views is the stack of what's shown, the machines at the bottom: esc goes back.
	views  []topView
	prompt *topPrompt // the : command or / filter being typed
	dialog topDialog
	help   bool

	flash    string
	flashErr bool
	flashID  int
}

// NewModel is desk top on the machines and profiles config.yml in cfgDirectory has, doing
// what it does on them through remote. version is desk's, for the header.
func NewModel(cfgDirectory, version string, remote Remote) *Model {
	m := &Model{
		cfgDirectory: cfgDirectory,
		version:      version,
		styles:       newTopStyles(),
		targets:      map[string]*common.StandaloneTarget{},
		remote:       remote,
	}
	m.load()
	m.views = []topView{&topMachinesView{}}
	return m
}

// load reads the machines and profiles config.yml has, as the starting point.
func (m *Model) load() {
	_, _, err := common.ReadCredentials(m.cfgDirectory)
	m.loggedIn = err == nil
	if machines, err := common.MachinesFromCfg(m.cfgDirectory); err == nil {
		m.machines = machines
	}
	if profiles, err := deskconn.Profiles(m.cfgDirectory); err == nil {
		m.profiles = profiles
	}
	for _, profile := range m.profiles {
		authID := profile.AuthID
		if authID == "" {
			authID = common.CurrentUsername()
		}
		// Without ReadPassword: desk top asks for a password itself, before connecting.
		target := &common.StandaloneTarget{URL: profile.URL, CertHash: profile.CertHash, AuthID: authID,
			PrivateKey: profile.PrivateKey}
		m.targets[profile.Name] = target
		common.SetStandaloneTargetFor(common.ProfileRealm(profile.Name), target)
	}
}

// Close stops everything desk top started.
func (m *Model) Close() {
	for _, view := range m.views {
		view.close()
	}
	for _, forward := range m.forwards {
		forward.stop()
	}
	m.remote.Close()
}

func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.fetchConnected()}
	// No cached machines yet: fetch them, as desk ls would.
	if len(m.machines) == 0 && m.loggedIn {
		cmds = append(cmds, m.refresh())
	}
	return tea.Batch(cmds...)
}

type (
	topMachinesMsg struct {
		machines []common.Machine
		err      error
	}
	topConnectedMsg struct {
		connected map[string]any
		daemonUp  bool
	}
	topFlashExpiredMsg int
	// topShellDoneMsg is sent when a shell ends and desk top has the terminal back.
	topShellDoneMsg struct {
		target topTarget
		err    error
	}
	topPingMsg struct {
		target topTarget
		rtt    time.Duration
		err    error
	}
)

func (m *Model) refresh() tea.Cmd {
	if m.refreshing {
		return nil
	}
	m.refreshing = true
	cached := m.machines
	return tea.Batch(m.setFlash("Fetching your machines…", false), func() tea.Msg {
		machines, err := deskconn.RefreshMachines(m.cfgDirectory, cached)
		if err == nil {
			err = deskconn.CacheMachines(m.cfgDirectory, machines)
		}
		return topMachinesMsg{machines: machines, err: err}
	})
}

// setFlash shows message in the flash line for a while.
func (m *Model) setFlash(message string, isErr bool) tea.Cmd {
	m.flashID++
	m.flash, m.flashErr = message, isErr
	id := m.flashID
	return tea.Tick(topFlashFor, func(time.Time) tea.Msg { return topFlashExpiredMsg(id) })
}

func (m *Model) flashErrorf(format string, args ...any) tea.Cmd {
	return m.setFlash(fmt.Sprintf(format, args...), true)
}

func (m *Model) top() topView { return m.views[len(m.views)-1] }

func (m *Model) push(view topView) tea.Cmd {
	m.views = append(m.views, view)
	return view.start(m)
}

// pop goes back to the view under the top one.
func (m *Model) pop() {
	if len(m.views) == 1 {
		return
	}
	m.top().close()
	m.views = m.views[:len(m.views)-1]
}

// closeView closes view and the ones over it, going back to the one under it.
func (m *Model) closeView(view topView) {
	i := slices.Index(m.views, view)
	if i < 1 {
		return
	}
	for len(m.views) > i {
		m.pop()
	}
}

func (m *Model) newID() int {
	m.nextID++
	return m.nextID
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		return m, m.updateKeys(msg)
	case topFlashExpiredMsg:
		if int(msg) == m.flashID {
			m.flash = ""
		}
		return m, nil
	case topMachinesMsg:
		m.refreshing = false
		// Credentials the cloud refused are removed.
		_, _, err := common.ReadCredentials(m.cfgDirectory)
		m.loggedIn = err == nil
		if msg.err != nil {
			return m, m.flashErrorf("Couldn't fetch your machines: %v", msg.err)
		}
		m.machines = msg.machines
		return m, m.setFlash(fmt.Sprintf("Fetched %d machine(s).", len(msg.machines)), false)
	case topConnectedMsg:
		m.connected, m.daemonUp = msg.connected, msg.daemonUp
		return m, nil
	case topShellDoneMsg:
		var cmd tea.Cmd
		if msg.err != nil {
			cmd = m.failed(msg.target, "Shell on "+msg.target.name, msg.err)
		} else {
			cmd = m.setFlash("Shell on "+msg.target.name+" ended.", false)
		}
		return m, tea.Batch(cmd, m.fetchConnected())
	case topPingMsg:
		if msg.err != nil {
			return m, m.failed(msg.target, "Ping "+msg.target.name, msg.err)
		}
		return m, m.setFlash(fmt.Sprintf("%s replied in %s.", msg.target.name, msg.rtt.Round(time.Millisecond)), false)
	case topForwardEndedMsg:
		return m, m.forwardEnded(msg)
	}

	for _, view := range m.views {
		if cmd, ok := view.handle(m, msg); ok {
			return m, cmd
		}
	}
	return m, nil
}

func (m *Model) updateKeys(msg tea.KeyMsg) tea.Cmd {
	// Characters typed in quick succession (fast key repeat, typing ahead) arrive as one
	// message: take them a key at a time.
	if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 && !msg.Paste {
		var cmds []tea.Cmd
		for _, r := range msg.Runes {
			cmds = append(cmds, m.updateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: msg.Alt}))
		}
		return tea.Batch(cmds...)
	}
	return m.updateKey(msg)
}

func (m *Model) updateKey(msg tea.KeyMsg) tea.Cmd {
	// Ctrl+C quits from anywhere, asking first about running port forwards unless that's
	// what's being asked.
	if msg.Type == tea.KeyCtrlC {
		if confirm, ok := m.dialog.(*topConfirm); ok && confirm.quitting {
			return tea.Quit
		}
		m.dialog, m.prompt, m.help = nil, nil, false
		return m.quit()
	}

	switch {
	case m.dialog != nil:
		return m.dialog.update(m, msg)
	case m.prompt != nil:
		return m.updatePrompt(msg)
	case m.help:
		if key := msg.String(); key == keyEsc || key == "?" || key == "q" || key == keyEnter {
			m.help = false
		}
		return nil
	}

	key := msg.String()
	if cmd, ok := m.top().key(m, key); ok {
		return cmd
	}
	switch key {
	case "q":
		return m.quit()
	case keyEsc:
		m.pop()
	case "?":
		m.help = true
	case ":":
		m.prompt = &topPrompt{command: true}
	case "/":
		if view, ok := m.top().(topFilterable); ok {
			m.prompt = &topPrompt{text: view.filter()}
		}
	case "ctrl+r":
		return tea.Batch(m.refresh(), m.fetchConnected())
	}
	return nil
}

// quit leaves desk top, asking first if that would stop port forwards.
func (m *Model) quit() tea.Cmd {
	if running := m.runningForwards(); running > 0 {
		m.dialog = &topConfirm{
			title:    "Quit",
			message:  fmt.Sprintf("%d port forward(s) are running. Quit and stop them?", running),
			yes:      func(*Model) tea.Cmd { return tea.Quit },
			quitting: true,
		}
		return nil
	}
	return tea.Quit
}

// topPrompt is the line where a : command or a / filter is typed.
type topPrompt struct {
	command bool
	text    string
}

func (m *Model) updatePrompt(msg tea.KeyMsg) tea.Cmd {
	p := m.prompt
	filterable, _ := m.top().(topFilterable)
	switch msg.Type {
	case tea.KeyEsc:
		m.prompt = nil
		if !p.command && filterable != nil {
			filterable.setFilter("")
		}
		return nil
	case tea.KeyEnter:
		m.prompt = nil
		if p.command {
			return m.runCommand(strings.TrimSpace(p.text))
		}
		return nil
	case tea.KeyBackspace:
		if p.text != "" {
			p.text = string([]rune(p.text)[:len([]rune(p.text))-1])
		}
	case tea.KeyRunes, tea.KeySpace:
		p.text += string(msg.Runes)
	case tea.KeyTab:
		if p.command {
			p.text = completeTopCommand(p.text)
		}
	}
	if !p.command && filterable != nil {
		filterable.setFilter(p.text)
	}
	return nil
}

// topCommandNames are the views and actions : takes, each by its name and then its aliases.
func topCommandNames() [][]string {
	return [][]string{
		{topMachines, topMachine, "m", "ls"},
		{"pf", "forwards", "portforwards"},
		{"quit", "q", "q!"},
		{"help", "h"},
	}
}

func completeTopCommand(text string) string {
	for _, names := range topCommandNames() {
		if text != "" && strings.HasPrefix(names[0], text) {
			return names[0]
		}
	}
	return text
}

func (m *Model) runCommand(command string) tea.Cmd {
	name := ""
	for _, names := range topCommandNames() {
		for _, alias := range names {
			if command == alias {
				name = names[0]
			}
		}
	}
	switch name {
	case topMachines:
		for len(m.views) > 1 {
			m.pop()
		}
	case "pf":
		if _, ok := m.top().(*topForwardsView); ok {
			return nil
		}
		for len(m.views) > 1 {
			m.pop()
		}
		return m.push(&topForwardsView{})
	case "quit":
		return m.quit()
	case "help":
		m.help = true
	case "":
		if command != "" {
			return m.flashErrorf("No command %q. Try :machines, :pf or :quit.", command)
		}
	}
	return nil
}
