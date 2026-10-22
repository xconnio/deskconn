package top

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/xconnio/deskconn/internal/common"
)

// topProcess is a process as the machine's process list has it.
type topProcess struct {
	PID        int32   `json:"pid"`
	Name       string  `json:"name"`
	User       string  `json:"user"`
	Status     string  `json:"status"`
	Cmdline    string  `json:"cmdline"`
	CPUPercent float64 `json:"cpu_percent"`
	MemRSS     uint64  `json:"mem_rss"`
	MemPercent float32 `json:"mem_percent"`
}

// topSignalResult is what the machine reports for each process it signaled.
type topSignalResult struct {
	PID   int32  `json:"pid"`
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// How processes are sorted.
const (
	topSortCPU  = "cpu"
	topSortMem  = "mem"
	topSortPID  = "pid"
	topSortName = "name"
)

const (
	topDefaultInterval = 2 * time.Second
	topMinInterval     = time.Second
	topMaxInterval     = 30 * time.Second
	// topHistory is how many samples the CPU and network sparklines show.
	topHistory = 40
)

// topResourcesView shows a machine's resources like htop: meters for its CPUs, memory,
// swap and disk over its processes, all kept up to date.
type topResourcesView struct {
	target topTarget
	id     int

	info    *common.MachineInfo
	procs   []topProcess
	err     error
	fetched time.Time

	cpuHistory []float64
	netHistory []float64

	list     topList
	search   string
	sortBy   string
	reverse  bool // sorted the other way: ascending for numbers, descending for names
	interval time.Duration
	height   int
}

type (
	topResourcesMsg struct {
		id    int
		info  *common.MachineInfo
		procs []topProcess
		err   error
	}
	topResourcesTickMsg struct{ id int }
	topSignalMsg        struct {
		id      int
		name    string
		signal  string
		results []topSignalResult
		err     error
	}
)

func newTopResourcesView(target topTarget) *topResourcesView {
	return &topResourcesView{target: target, sortBy: topSortCPU, interval: topDefaultInterval}
}

func (v *topResourcesView) start(m *Model) tea.Cmd {
	v.id = m.newID()
	return v.fetch(m)
}

// close stops the updates: their messages are for a view no longer shown.
func (v *topResourcesView) close() { v.id = -1 }

func (v *topResourcesView) crumb() string  { return "resources" }
func (v *topResourcesView) filter() string { return v.search }

func (v *topResourcesView) setFilter(filter string) {
	v.search = filter
	v.list = topList{}
}

func (v *topResourcesView) fetch(m *Model) tea.Cmd {
	id, realm := v.id, v.target.realm
	return func() tea.Msg {
		var info common.MachineInfo
		if err := m.remote.Call(realm, common.ProcedureMachineInfo, nil, &info); err != nil {
			return topResourcesMsg{id: id, err: err}
		}
		var procs []topProcess
		err := m.remote.Call(realm, common.ProcedureProcessList, nil, &procs)
		return topResourcesMsg{id: id, info: &info, procs: procs, err: err}
	}
}

func (v *topResourcesView) handle(m *Model, msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case topResourcesMsg:
		if msg.id != v.id {
			return nil, false
		}
		v.err = msg.err
		if msg.info != nil {
			v.info, v.fetched = msg.info, time.Now()
			v.cpuHistory = pushHistory(v.cpuHistory, averageCPU(msg.info.CPUUsages))
			v.netHistory = pushHistory(v.netHistory, networkRate(msg.info))
		}
		if msg.err == nil {
			v.procs = msg.procs
		} else if strings.Contains(strings.ToLower(msg.err.Error()), "auth") {
			// A wrong password: asking again beats retrying it.
			m.closeView(v)
			return m.failed(v.target, "Resources of "+v.target.name, msg.err), true
		}
		id := v.id
		return tea.Tick(v.interval, func(time.Time) tea.Msg { return topResourcesTickMsg{id: id} }), true
	case topResourcesTickMsg:
		if msg.id != v.id {
			return nil, false
		}
		return v.fetch(m), true
	case topSignalMsg:
		if msg.id != v.id {
			return nil, false
		}
		return v.signaled(m, msg), true
	}
	return nil, false
}

func pushHistory(history []float64, value float64) []float64 {
	history = append(history, value)
	if len(history) > topHistory {
		history = history[len(history)-topHistory:]
	}
	return history
}

func averageCPU(usages []float64) float64 {
	if len(usages) == 0 {
		return 0
	}
	total := 0.0
	for _, usage := range usages {
		total += usage
	}
	return total / float64(len(usages))
}

// networkRate is the bytes a second the machine sends and receives, over all its interfaces.
func networkRate(info *common.MachineInfo) float64 {
	total := 0.0
	for _, iface := range info.NetworkInterfaces {
		total += iface.BytesSentPS + iface.BytesRecvPS
	}
	return total
}

func (v *topResourcesView) title(m *Model) string {
	return m.titled("Resources("+v.target.name+")", v.search, len(v.processes()))
}

func (v *topResourcesView) hints(*Model) []topHint {
	return []topHint{
		{"C", "Sort CPU"}, {"M", "Sort memory"}, {"N", "Sort name"}, {"I", "Sort PID"},
		{"R", "Reverse sort"}, {"t", "Terminate"}, {"ctrl-k", "Kill"}, {"+/-", "Interval"},
		{"s", "Shell"}, {"l", "Logs"}, {"/", hintFilter},
	}
}

func (v *topResourcesView) key(m *Model, key string) (tea.Cmd, bool) {
	procs := v.processes()
	if v.list.move(key, len(procs), v.height) {
		return nil, true
	}
	sorts := map[string]string{"C": topSortCPU, "M": topSortMem, "N": topSortName, "I": topSortPID}
	if sortBy, ok := sorts[key]; ok {
		if v.sortBy == sortBy {
			v.reverse = !v.reverse
		} else {
			v.sortBy, v.reverse = sortBy, false
		}
		return nil, true
	}
	switch key {
	case "R":
		v.reverse = !v.reverse
	case "+", "=":
		v.interval = min(topMaxInterval, v.interval+time.Second)
		return m.setFlash(fmt.Sprintf("Updating every %s.", v.interval), false), true
	case "-":
		v.interval = max(topMinInterval, v.interval-time.Second)
		return m.setFlash(fmt.Sprintf("Updating every %s.", v.interval), false), true
	case keyEsc:
		if v.search == "" {
			return nil, false
		}
		v.setFilter("")
	case "s":
		return m.shell(v.target), true
	case "l":
		return m.push(newTopLogsView(m, v.target)), true
	case "t", "ctrl+k":
		if len(procs) == 0 {
			return nil, true
		}
		v.list.clamp(len(procs))
		v.confirmSignal(m, procs[v.list.cursor], key == "ctrl+k")
	default:
		return nil, false
	}
	return nil, true
}

// confirmSignal asks whether to terminate proc, or kill it.
func (v *topResourcesView) confirmSignal(m *Model, proc topProcess, kill bool) {
	signal, verb := "term", "Terminate"
	if kill {
		signal, verb = "kill", "Kill"
	}
	id, realm := v.id, v.target.realm
	m.dialog = &topConfirm{
		title:   verb,
		message: fmt.Sprintf("%s %s (%d) on %s?", verb, proc.Name, proc.PID, v.target.name),
		yes: func(m *Model) tea.Cmd {
			return func() tea.Msg {
				var results []topSignalResult
				args := map[string]any{"pids": []int32{proc.PID}, "signal": signal}
				err := m.remote.Call(realm, common.ProcedureProcessSignal, args, &results)
				return topSignalMsg{id: id, name: proc.Name, signal: signal, results: results, err: err}
			}
		},
	}
}

func (v *topResourcesView) signaled(m *Model, msg topSignalMsg) tea.Cmd {
	signal := strings.ToUpper(msg.signal)
	if msg.err != nil {
		return m.flashErrorf("%s %s: %v", signal, msg.name, msg.err)
	}
	for _, result := range msg.results {
		if !result.OK {
			return m.flashErrorf("%s %s (%d): %s", signal, msg.name, result.PID, result.Error)
		}
	}
	return tea.Batch(m.setFlash(fmt.Sprintf("Sent %s to %s.", signal, msg.name), false), v.fetch(m))
}

// processes are the processes matching the filter, sorted.
func (v *topResourcesView) processes() []topProcess {
	search := strings.ToLower(v.search)
	var procs []topProcess
	for _, proc := range v.procs {
		if search == "" || strings.Contains(strings.ToLower(proc.Name), search) ||
			strings.Contains(strings.ToLower(proc.User), search) ||
			strings.Contains(strings.ToLower(proc.Cmdline), search) || fmt.Sprint(proc.PID) == search {
			procs = append(procs, proc)
		}
	}
	compare := func(a, b topProcess) int {
		switch v.sortBy {
		case topSortMem:
			return cmp.Compare(b.MemRSS, a.MemRSS)
		case topSortPID:
			return cmp.Compare(a.PID, b.PID)
		case topSortName:
			return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
		default:
			return cmp.Compare(b.CPUPercent, a.CPUPercent)
		}
	}
	slices.SortStableFunc(procs, func(a, b topProcess) int {
		if c := compare(a, b); c != 0 {
			if v.reverse {
				return -c
			}
			return c
		}
		return cmp.Compare(a.PID, b.PID)
	})
	return procs
}

func (v *topResourcesView) body(m *Model, width, height int) []string {
	if v.info == nil {
		if v.err != nil {
			return []string{
				m.styles.bad.Render(fmt.Sprintf("Couldn't get the resources of %s: %v", v.target.name, v.err)),
				"",
				m.styles.dim.Render(fmt.Sprintf("Trying again every %s. Press esc to go back.", v.interval)),
			}
		}
		return []string{m.styles.dim.Render("Connecting to " + v.target.name + "…")}
	}

	lines := v.meterLines(m, width)
	if v.err != nil {
		lines = append(lines, m.styles.bad.Render(fit(fmt.Sprintf("Lost %s: %v. Retrying…", v.target.name, v.err), width)))
	} else {
		lines = append(lines, "")
	}

	columns := []topColumn{
		{title: v.sortTitle("PID", topSortPID), right: true}, {title: "USER", max: 12},
		{title: v.sortTitle("CPU%", topSortCPU), right: true}, {title: "MEM%", right: true},
		{title: v.sortTitle("RES", topSortMem), right: true}, {title: v.sortTitle("NAME", topSortName), max: 24},
		{title: "COMMAND", flex: true},
	}
	procs := v.processes()
	rows := make([]topRow, 0, len(procs))
	for _, proc := range procs {
		style := m.styles.text
		switch {
		case proc.CPUPercent >= 50:
			style = m.styles.bad
		case proc.CPUPercent >= 10:
			style = m.styles.warn
		}
		command := proc.Cmdline
		if command == "" {
			command = proc.Name
		}
		rows = append(rows, topRow{style: style, cells: []string{
			fmt.Sprint(proc.PID), proc.User, fmt.Sprintf("%.1f", proc.CPUPercent),
			fmt.Sprintf("%.1f", proc.MemPercent), formatSize(proc.MemRSS), proc.Name, command,
		}})
	}
	tableHeight := max(2, height-len(lines))
	v.height = tableHeight - 1
	return append(lines, m.table(columns, rows, &v.list, width, tableHeight)...)
}

// sortTitle marks the column the processes are sorted by.
func (v *topResourcesView) sortTitle(title, sortBy string) string {
	if v.sortBy != sortBy {
		return title
	}
	descending := sortBy != topSortName && sortBy != topSortPID
	if v.reverse {
		descending = !descending
	}
	if descending {
		return title + "↓"
	}
	return title + "↑"
}

// meterLines are htop's meters: a bar per CPU in columns, then memory, swap and disk beside
// the totals.
func (v *topResourcesView) meterLines(m *Model, width int) []string {
	info := v.info
	cores := len(info.CPUUsages)
	columns := 2
	if cores > 16 && width >= 160 {
		columns = 4
	}
	if cores <= 1 {
		columns = 1
	}
	rowsPerColumn := (cores + columns - 1) / columns
	columnWidth := (width - (columns - 1)) / columns
	labelWidth := len(fmt.Sprint(max(0, cores-1)))

	var lines []string
	for row := 0; row < rowsPerColumn; row++ {
		var parts []string
		for column := 0; column < columns; column++ {
			i := column*rowsPerColumn + row
			if i >= cores {
				parts = append(parts, strings.Repeat(" ", columnWidth))
				continue
			}
			label := fmt.Sprintf("%*d", labelWidth, i)
			usage := info.CPUUsages[i]
			parts = append(parts, m.meter(label, usage/100, fmt.Sprintf("%.1f%%", usage), columnWidth))
		}
		lines = append(lines, strings.Join(parts, " "))
	}

	half := (width - 1) / 2
	memUsed := info.RAMTotal - min(info.RAMTotal, info.RAMAvailable)
	left := []string{
		m.meter("Mem", ratio(memUsed, info.RAMTotal),
			formatSize(memUsed)+"/"+formatSize(info.RAMTotal), half),
		m.meter("Swp", ratio(info.SwapUsed, info.SwapTotal),
			formatSize(info.SwapUsed)+"/"+formatSize(info.SwapTotal), half),
		m.meter("Dsk", ratio(info.DiskUsed, info.DiskTotal),
			formatSize(info.DiskUsed)+"/"+formatSize(info.DiskTotal), half),
	}

	label := m.styles.infoLabel.Render
	value := m.styles.infoValue.Render
	running := 0
	for _, proc := range v.procs {
		if proc.Status == "R" || strings.HasPrefix(strings.ToLower(proc.Status), "running") {
			running++
		}
	}
	sparkWidth := max(0, min(topHistory, half-30))
	right := []string{
		label("Tasks: ") + value(fmt.Sprint(len(v.procs))) + m.styles.dim.Render(fmt.Sprintf(", %d running", running)),
		label("CPU:   ") + value(fmt.Sprintf("%5.1f%%", averageCPU(info.CPUUsages))) + " " +
			m.styles.good.Render(sparkline(v.cpuHistory, 100, sparkWidth)),
		label("Net:   ") + value(fmt.Sprintf("%6s/s", formatSize(uint64(networkRate(info))))) + " " +
			m.styles.accent.Render(sparkline(v.netHistory, 0, sparkWidth)),
	}
	if b := info.Battery; b != nil {
		battery := fmt.Sprintf("%d%% %s", b.Percentage, b.Status)
		if b.TimeRemainingMins > 0 {
			battery += fmt.Sprintf(", %dh%02dm left", b.TimeRemainingMins/60, b.TimeRemainingMins%60)
		}
		right = append(right, label("Power: ")+value(battery))
	}
	if info.CPUModel != "" {
		right = append(right, label("Model: ")+m.styles.dim.Render(fmt.Sprintf("%s (%d cores, %d threads)",
			info.CPUModel, info.CPUPhysical, info.CPULogical)))
	}
	for i := 0; i < max(len(left), len(right)); i++ {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		lines = append(lines, fit(l, half)+"  "+r)
	}
	return lines
}

func ratio(part, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total)
}

// meter is an htop meter in width cells: label[||||||     text], the bar coloured by how
// full it is.
func (m *Model) meter(label string, fraction float64, text string, width int) string {
	inner := max(0, width-lipgloss.Width(label)-2)
	fraction = math.Max(0, math.Min(1, fraction))
	filled := int(math.Round(fraction * float64(inner)))
	text = truncate(text, inner)
	textStart := inner - len([]rune(text))

	style := m.styles.good
	switch {
	case fraction >= 0.8:
		style = m.styles.bad
	case fraction >= 0.5:
		style = m.styles.warn
	}
	bar := strings.Repeat("|", min(filled, textStart))
	space := strings.Repeat(" ", max(0, textStart-filled))
	// The bar runs on under the text, which takes its colour where it does.
	textRunes := []rune(text)
	covered := max(0, min(len(textRunes), filled-textStart))
	return m.styles.bold.Render(label) + m.styles.bold.Render("[") + style.Render(bar) + space +
		style.Render(string(textRunes[:covered])) + m.styles.dim.Render(string(textRunes[covered:])) +
		m.styles.bold.Render("]")
}

// sparkline draws the last width values in blocks, scaled to top, or to the largest value
// if top is 0.
func sparkline(values []float64, top float64, width int) string {
	if width <= 0 || len(values) == 0 {
		return ""
	}
	values = values[max(0, len(values)-width):]
	if top == 0 {
		for _, value := range values {
			top = math.Max(top, value)
		}
	}
	blocks := []rune("▁▂▃▄▅▆▇█")
	var b strings.Builder
	for _, value := range values {
		level := 0
		if top > 0 {
			level = int(math.Round(value / top * float64(len(blocks)-1)))
		}
		b.WriteRune(blocks[max(0, min(len(blocks)-1, level))])
	}
	return b.String()
}

// formatSize is a size the short way htop has it: 512K, 1.5M, 15.5G.
func formatSize(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	value, suffix := float64(b)/unit, 0
	for value >= unit && suffix < 4 {
		value /= unit
		suffix++
	}
	if value >= 100 {
		return fmt.Sprintf("%.0f%c", value, "KMGTP"[suffix])
	}
	return fmt.Sprintf("%.1f%c", value, "KMGTP"[suffix])
}

func truncate(s string, width int) string {
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	if width <= 0 {
		return ""
	}
	return string(runes[:width-1]) + "…"
}
