package top

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xconnio/deskconn/internal/common"
)

// topMachinesView lists the account's machines, then the profiles.
type topMachinesView struct {
	list   topList
	search string
	height int // the rows the table had when last drawn, for paging
}

// topEntry is a row of the machines view: a machine of the account or a profile.
type topEntry struct {
	machine *common.Machine
	profile *common.Profile
}

func (e topEntry) target() topTarget {
	if e.profile != nil {
		return profileTarget(*e.profile)
	}
	return machineTarget(*e.machine)
}

func (e topEntry) matches(filter string) bool {
	var fields []string
	if e.profile != nil {
		fields = []string{e.profile.Name, e.profile.URL, e.profile.AuthID}
	} else {
		fields = []string{e.machine.Name, e.machine.Alias, e.machine.Authid}
	}
	for _, field := range fields {
		if strings.Contains(strings.ToLower(field), strings.ToLower(filter)) {
			return true
		}
	}
	return false
}

// isEmptyMachine reports an entry of config.yml with nothing to show, which desk ls skips too.
func isEmptyMachine(machine common.Machine) bool {
	return machine.Name == "" && machine.Authid == "" && machine.Alias == ""
}

// entries are the machines, then the profiles, that match the filter.
func (v *topMachinesView) entries(m *Model) []topEntry {
	var entries []topEntry
	for i := range m.machines {
		machine := &m.machines[i]
		if entry := (topEntry{machine: machine}); !isEmptyMachine(*machine) && entry.matches(v.search) {
			entries = append(entries, entry)
		}
	}
	for i := range m.profiles {
		if entry := (topEntry{profile: &m.profiles[i]}); entry.matches(v.search) {
			entries = append(entries, entry)
		}
	}
	return entries
}

func (v *topMachinesView) selected(m *Model) (topEntry, bool) {
	entries := v.entries(m)
	if len(entries) == 0 {
		return topEntry{}, false
	}
	v.list.clamp(len(entries))
	return entries[v.list.cursor], true
}

func (v *topMachinesView) start(*Model) tea.Cmd { return nil }
func (v *topMachinesView) close()               {}
func (v *topMachinesView) crumb() string        { return topMachines }
func (v *topMachinesView) filter() string       { return v.search }

func (v *topMachinesView) setFilter(filter string) {
	v.search = filter
	v.list = topList{}
}

func (v *topMachinesView) title(m *Model) string {
	return m.titled("Machines", v.search, len(v.entries(m)))
}

func (v *topMachinesView) hints(*Model) []topHint {
	return []topHint{
		{keyEnter, "Resources"}, {"s", "Shell"}, {"l", "Logs"}, {"shift-f", "Port forward"}, {"f", "Show port forwards"},
		{"p", "Ping"}, {"ctrl-r", "Refresh"}, {"/", hintFilter},
	}
}

func (v *topMachinesView) key(m *Model, key string) (tea.Cmd, bool) {
	entries := v.entries(m)
	if v.list.move(key, len(entries), v.height) {
		return nil, true
	}
	if key == keyEsc && v.search != "" {
		v.setFilter("")
		return nil, true
	}
	entry, ok := v.selected(m)
	if !ok {
		return nil, false
	}
	target := entry.target()
	switch key {
	case keyEnter:
		return m.withTarget(target, func() tea.Cmd { return m.push(newTopResourcesView(target)) }), true
	case "s":
		return m.shell(target), true
	case "l":
		return m.withTarget(target, func() tea.Cmd { return m.push(newTopLogsView(m, target)) }), true
	case "F":
		m.dialog = newTopForwardDialog(target)
		return nil, true
	case "f":
		return m.runCommand("pf"), true
	case "p":
		return m.ping(target), true
	}
	return nil, false
}

func (v *topMachinesView) handle(*Model, tea.Msg) (tea.Cmd, bool) { return nil, false }

func (v *topMachinesView) body(m *Model, width, height int) []string {
	v.height = height - 1
	entries := v.entries(m)
	if len(entries) == 0 {
		return v.emptyLines(m)
	}

	columns := []topColumn{
		{title: "NAME", max: 32}, {title: "KIND"}, {title: "ALIAS", max: 24}, {title: "ID"},
		{title: "ADDRESS", max: 48}, {title: "LOGIN", max: 24}, {title: "CONNECTED"},
	}
	rows := make([]topRow, 0, len(entries))
	for _, entry := range entries {
		if profile := entry.profile; profile != nil {
			rows = append(rows, topRow{style: m.styles.text, cells: []string{
				profile.Name, "profile", "", "", profile.URL, profileLogin(*profile), "",
			}})
			continue
		}
		machine := entry.machine
		since := common.ConnectedSince(m.connected, machine.Realm)
		style := m.styles.text
		if since != "" {
			style = m.styles.good
		}
		rows = append(rows, topRow{style: style, cells: []string{
			machine.Name, topMachine, machine.Alias, common.ShortID(machine.Authid), "via the cloud", "account", since,
		}})
	}
	return m.table(columns, rows, &v.list, width, height)
}

// profileLogin is how profile logs in, e.g. "key as bob".
func profileLogin(profile common.Profile) string {
	as := profile.AuthID
	if as == "" {
		as = common.CurrentUsername()
	}
	if profile.PrivateKey != "" {
		return "key as " + as
	}
	return "password as " + as
}

func (v *topMachinesView) emptyLines(m *Model) []string {
	switch {
	case v.search != "":
		return []string{m.styles.dim.Render("Nothing matches. Press esc to clear the filter.")}
	case m.refreshing:
		return []string{m.styles.dim.Render("Fetching your machines…")}
	case !m.loggedIn:
		return []string{
			m.styles.text.Render("No machines or profiles yet."),
			"",
			m.styles.dim.Render("Log in with `desk account login`, or add a standalone machine with `desk profile add`."),
		}
	default:
		return []string{
			m.styles.text.Render("No machines or profiles yet."),
			"",
			m.styles.dim.Render("Press ctrl-r to fetch your machines, or add a standalone machine with " +
				"`desk profile add`."),
		}
	}
}
