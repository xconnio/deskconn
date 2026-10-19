package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/olekukonko/tablewriter"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
	"github.com/xconnio/xconn-go"
)

type machineCommands struct {
	ls         *kingpin.CmdClause
	lsShortcut *kingpin.CmdClause
	attach     *kingpin.CmdClause
	detach     *kingpin.CmdClause
	alias      *kingpin.CmdClause
	ping       *kingpin.CmdClause
	connect    *kingpin.CmdClause
	disconnect *kingpin.CmdClause
	info       *kingpin.CmdClause
	logs       *kingpin.CmdClause

	// Shared by ls and its lsShortcut.
	lsRefresh  bool
	lsDetailed bool

	attachName     *string
	attachUsername *string

	aliasMachine *string
	aliasName    *string
	aliasUnset   *bool

	pingMachine *string
	pingCount   *int

	connectMachine *string

	disconnectMachine *string
	disconnectAll     *bool

	infoMachine *string
	infoMode    *string

	logsMachine *string
	logsSource  *string
	logsFollow  *bool
	logsTail    *int64
	logsSince   *string
	logsMode    *string
}

func registerMachineCommands(app *kingpin.Application, cfgDirectory string) *machineCommands {
	const modeHelp = "Connection mode: 'routed' goes via the router over QUIC, 'direct' connects over WebRTC"
	cmds := &machineCommands{}

	machineCmd := app.Command("machine", "Manage machines and connections")

	cmds.ls = machineCmd.Command("ls", "List machines")
	cmds.ls.Flag("refresh", "Refresh machine list from cloud").BoolVar(&cmds.lsRefresh)
	cmds.ls.Flag("detailed", "Show detailed output").BoolVar(&cmds.lsDetailed)

	cmds.attach = machineCmd.Command("attach", "Register this machine with your account")
	cmds.attachName = cmds.attach.Flag("name", "Machine name").Short('n').String()
	cmds.attachUsername = cmds.attach.Flag("username", "Username").Short('u').String()

	cmds.detach = machineCmd.Command("detach", "Remove this machine from your account (asks for confirmation)")

	cmds.alias = machineCmd.Command("alias", "Set or unset a machine's alias")
	cmds.aliasMachine = cmds.alias.Arg("machine", "ID, name or alias of machine").Required().
		HintAction(machineCompletions(cfgDirectory)).String()
	cmds.aliasName = cmds.alias.Arg("name", "Alias to set").String()
	cmds.aliasUnset = cmds.alias.Flag("unset", "Remove the machine's alias").Bool()

	cmds.ping = machineCmd.Command("ping", "Ping a machine and measure round-trip time")
	cmds.pingMachine = machineArg(cmds.ping, "ID, name or alias of machine", cfgDirectory)
	cmds.pingCount = cmds.ping.Flag("count", "Number of pings to send (0 = infinite)").Short('c').Default("0").Int()

	cmds.connect = machineCmd.Command("connect", "Establish a persistent connection to a machine")
	cmds.connectMachine = machineArg(cmds.connect, "ID, name or alias of machine", cfgDirectory)

	cmds.disconnect = machineCmd.Command("disconnect", "Disconnect a persistent connection")
	cmds.disconnectMachine = cmds.disconnect.Arg("machine", "ID, name or alias of machine").
		HintAction(machineCompletions(cfgDirectory)).String()
	cmds.disconnectAll = cmds.disconnect.Flag("all", "Disconnect all machines").Bool()

	cmds.info = machineCmd.Command("info", "Show machine resource usage")
	cmds.infoMachine = machineArg(cmds.info, "ID, name or alias of machine", cfgDirectory)
	cmds.infoMode = modeFlag(cmds.info, modeHelp)

	cmds.logs = machineCmd.Command("logs", "Stream logs from a machine")
	cmds.logsMachine = machineArg(cmds.logs, "ID, name or alias of machine", cfgDirectory)
	cmds.logsSource = cmds.logs.Arg("source",
		"Service name (e.g. nginx) or file path (e.g. /var/log/syslog); omit for system journal").String()
	cmds.logsFollow = cmds.logs.Flag("follow", "Follow log output").Short('f').Bool()
	cmds.logsTail = cmds.logs.Flag("tail", "Number of lines to show from the end (-1 = default)").Short('n').
		Default("-1").Int64()
	cmds.logsSince = cmds.logs.Flag("since", "Show entries since duration ago (e.g. 1h, 30m)").String()
	cmds.logsMode = modeFlag(cmds.logs, modeHelp)

	return cmds
}

// registerLsShortcut adds the top-level "ls" shortcut for "machine ls". It is registered
// separately so it keeps its place after "vpn" in --help-long and shell completions.
func (c *machineCommands) registerLsShortcut(app *kingpin.Application) {
	c.lsShortcut = app.Command("ls", "List your machines (shortcut for 'machine ls')")
	c.lsShortcut.Flag("refresh", "Refresh machine list from cloud").BoolVar(&c.lsRefresh)
	c.lsShortcut.Flag("detailed", "Show detailed output").BoolVar(&c.lsDetailed)
}

// accountOnly reports whether parsedCmd works on the machines of your account, so it isn't
// available with --profile.
func (c *machineCommands) accountOnly(parsedCmd string) bool {
	switch parsedCmd {
	case c.ping.FullCommand(), c.connect.FullCommand(), c.disconnect.FullCommand(), c.lsShortcut.FullCommand(),
		c.ls.FullCommand():
		return true
	}
	return false
}

func dispatchMachineCommand(parsedCmd string, cmds *machineCommands, cfgDirectory string) bool {
	switch parsedCmd {
	case cmds.ls.FullCommand(), cmds.lsShortcut.FullCommand():
		printErr(listMachines(cfgDirectory, cmds.lsRefresh, cmds.lsDetailed))
	case cmds.attach.FullCommand():
		printErr(attach(*cmds.attachUsername, *cmds.attachName))
	case cmds.detach.FullCommand():
		printErr(detach(cfgDirectory))
	case cmds.alias.FullCommand():
		if *cmds.aliasUnset == (*cmds.aliasName != "") {
			printErr(errors.New("specify either an alias name or --unset"))
		} else {
			printErr(updateMachineAlias(cfgDirectory, *cmds.aliasMachine, *cmds.aliasName))
		}
	case cmds.ping.FullCommand():
		printErr(pingMachine(cfgDirectory, *cmds.pingMachine, *cmds.pingCount))
	case cmds.connect.FullCommand():
		printErr(connectMachine(cfgDirectory, *cmds.connectMachine))
	case cmds.disconnect.FullCommand():
		printErr(disconnectMachine(cfgDirectory, *cmds.disconnectMachine, *cmds.disconnectAll))
	case cmds.info.FullCommand():
		printErr(showMachineInfo(cfgDirectory, *cmds.infoMachine, *cmds.infoMode))
	case cmds.logs.FullCommand():
		printErr(streamMachineLogs(cfgDirectory, *cmds.logsMachine, *cmds.logsSource, *cmds.logsFollow,
			*cmds.logsTail, *cmds.logsSince, *cmds.logsMode))
	default:
		return false
	}
	return true
}

// listMachines prints the machines of your account and when each got connected, from the
// cached list unless refresh is set or there isn't one.
func listMachines(cfgDirectory string, refresh, detailed bool) error {
	session, err := xconn.ConnectAnonymous(context.Background(), daemonURI(cfgDirectory), common.LocalRealm)
	if err != nil {
		log.Fatal(err)
	}

	machinesCallResp := session.Call(common.ProcedureConnectedMachines).Do()
	if machinesCallResp.Err != nil {
		return machinesCallResp.Err
	}
	connectedMachines, ok := machinesCallResp.Args()[0].(map[string]any)
	if !ok {
		return errors.New("expected a map of strings")
	}
	fileExists := true
	machinesFromCfg, err := common.MachinesFromCfg(cfgDirectory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fileExists = false
		} else {
			if !refresh {
				return err
			}
			fmt.Println("your config is invalid, refreshing from cloud")
		}
	}

	tableHeader := []string{"ID", "NAME", "ALIAS", "CONNECTED SINCE"}
	if detailed {
		tableHeader = append(tableHeader, "ORGANIZATION", "REALM")
	}
	table := tablewriter.NewWriter(os.Stdout)
	table.Header(tableHeader)
	appendRow := func(d common.Machine) {
		since := connectedSince(connectedMachines, d.Realm)
		if detailed {
			_ = table.Append([]any{shortID(d.Authid), d.Name, d.Alias, since, d.Organization.Name, d.Realm})
		} else {
			_ = table.Append([]any{shortID(d.Authid), d.Name, d.Alias, since})
		}
	}

	if !refresh && fileExists {
		for _, d := range machinesFromCfg {
			if d.Name == "" && d.Authid == "" && d.Alias == "" {
				continue
			}
			appendRow(d)
		}
		return table.Render()
	}

	machines, err := deskconn.FetchMachinesFromCloud(cfgDirectory)
	if err != nil {
		return err
	}

	cfgMap := make(map[string]common.Machine, len(machinesFromCfg))
	for _, d := range machinesFromCfg {
		cfgMap[d.Authid] = d
	}

	nameCount := make(map[string]int)
	for i, d := range machines {
		count := nameCount[d.Name]
		if count > 0 {
			newName := fmt.Sprintf("%s-%s", d.Name, d.Authid[:6])
			machines[i].Name = newName
		}
		if localMachine, ok := cfgMap[d.Authid]; ok {
			machines[i].Alias = localMachine.Alias
		}
		nameCount[machines[i].Name]++
		appendRow(machines[i])
	}

	if err := table.Render(); err != nil {
		return err
	}
	return deskconn.CacheMachines(cfgDirectory, machines)
}

// pingMachine pings machine count times (0 = until interrupted) a second apart, then prints
// statistics like ping(8).
func pingMachine(cfgDirectory, machine string, count int) error {
	realm, err := machineRealm(machine, cfgDirectory)
	if err != nil {
		return err
	}
	localSession, err := xconn.ConnectAnonymous(context.Background(), daemonURI(cfgDirectory), common.LocalRealm)
	if err != nil {
		return err
	}

	fmt.Printf("PING %s\n", machine)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	var sent, received int
	var totalMs int64
	var minMs, maxMs int64 = 1 << 62, 0

pingLoop:
	for seq := 1; count == 0 || seq <= count; seq++ {
		select {
		case <-sigCh:
			break pingLoop
		default:
		}

		callResp := localSession.Call(common.ProcedureProxyPing).Args(realm).Do()
		sent++
		if callResp.Err != nil {
			fmt.Fprintf(os.Stderr, "seq=%d error: %v\n", seq, callResp.Err)
		} else {
			ms, _ := callResp.ArgInt64(0)
			received++
			totalMs += ms
			if ms < minMs {
				minMs = ms
			}
			if ms > maxMs {
				maxMs = ms
			}
			fmt.Printf("seq=%d time=%dms\n", seq, ms)
		}

		if count == 0 || seq < count {
			select {
			case <-sigCh:
				break pingLoop
			case <-time.After(time.Second):
			}
		}
	}

	loss := 0
	if sent > 0 {
		loss = (sent - received) * 100 / sent
	}
	fmt.Printf("\n--- %s ping statistics ---\n", machine)
	fmt.Printf("%d packets transmitted, %d received, %d%% packet loss\n", sent, received, loss)
	if received > 0 {
		fmt.Printf("rtt min/avg/max = %d/%d/%d ms\n", minMs, totalMs/int64(received), maxMs)
	}
	return nil
}

func connectMachine(cfgDirectory, machine string) error {
	realm, err := machineRealm(machine, cfgDirectory)
	if err != nil {
		return err
	}
	session, err := xconn.ConnectAnonymous(context.Background(), daemonURI(cfgDirectory), common.LocalRealm)
	if err != nil {
		return err
	}
	if err := session.Call(common.ProcedureConnect).Args(realm).Do().Err; err != nil {
		return err
	}
	fmt.Printf("connected to %s\n", machine)
	return nil
}

func disconnectMachine(cfgDirectory, machine string, all bool) error {
	session, err := xconn.ConnectAnonymous(context.Background(), daemonURI(cfgDirectory), common.LocalRealm)
	if err != nil {
		return err
	}
	if all {
		if err := session.Call(common.ProcedureDisconnectAll).Do().Err; err != nil {
			return err
		}
		fmt.Println("disconnected all machines")
		return nil
	}
	if machine == "" {
		return errors.New("specify a machine or use --all")
	}
	realm, err := machineRealm(machine, cfgDirectory)
	if err != nil {
		return err
	}
	if err := session.Call(common.ProcedureDisconnect).Args(realm).Do().Err; err != nil {
		return err
	}
	fmt.Printf("disconnected %s\n", machine)
	return nil
}

func showMachineInfo(cfgDirectory, machine, mode string) error {
	realm, err := machineRealm(machine, cfgDirectory)
	if err != nil {
		return err
	}
	rawData, err := fetchMachineInfo(cfgDirectory, realm, mode)
	if err != nil {
		return err
	}
	var info common.MachineInfo
	if err := json.Unmarshal(rawData, &info); err != nil {
		return err
	}
	printMachineInfo(info)
	return nil
}

func fetchMachineInfo(cfgDirectory, realm, mode string) ([]byte, error) {
	switch mode {
	case ModeRouted:
		quicSess, err := common.ConnectMachineRealmQUIC(context.Background(), realm, cfgDirectory)
		if err != nil {
			return nil, err
		}
		defer quicSess.Connection().Close()
		return deskconn.CallFileOp(quicSess.Session, common.ProcedureMachineInfo, nil)
	case ModeDirect:
		p2pSess, err := deskconn.ConnectMachineRealmP2P(context.Background(), realm, cfgDirectory)
		if err != nil {
			return nil, err
		}
		defer func() { _ = p2pSess.Leave() }()
		return deskconn.CallFileOp(p2pSess, common.ProcedureMachineInfo, nil)
	default:
		localSession, err := xconn.ConnectAnonymous(context.Background(), daemonURI(cfgDirectory), common.LocalRealm)
		if err != nil {
			return nil, err
		}
		callResp := localSession.Call(common.ProcedureProxyMachineInfo).Args(realm).Do()
		if callResp.Err != nil {
			return nil, callResp.Err
		}
		return callResp.ArgBytes(0)
	}
}

// printMachineInfo prints info laid out like top(1).
func printMachineInfo(info common.MachineInfo) {
	t := info.CPUTimes
	fmt.Printf("CPU Model:  %s\n", info.CPUModel)
	fmt.Printf("CPU Cores:  %d physical, %d logical\n", info.CPUPhysical, info.CPULogical)
	const cols = 4
	for i, usage := range info.CPUUsages {
		fmt.Printf("  CPU%-3d %5.1f%%", i, usage)
		if (i+1)%cols == 0 || i == len(info.CPUUsages)-1 {
			fmt.Println()
		}
	}
	fmt.Printf("%%Cpu(s):    %5.1f us, %5.1f sy, %5.1f ni, %5.1f id, %5.1f wa, %5.1f hi, %5.1f si, %5.1f st\n",
		t.User, t.System, t.Nice, t.Idle, t.IOWait, t.IRQ, t.SoftIRQ, t.Steal)
	fmt.Printf("MiB Mem:   %8.1f total, %8.1f free, %8.1f used, %8.1f buff/cache, %8.1f avail\n",
		toMiB(info.RAMTotal), toMiB(info.RAMFree), toMiB(info.RAMUsed),
		toMiB(info.RAMBuffCache), toMiB(info.RAMAvailable))
	fmt.Printf("MiB Swap:  %8.1f total, %8.1f free, %8.1f used\n",
		toMiB(info.SwapTotal), toMiB(info.SwapFree), toMiB(info.SwapUsed))
	fmt.Printf("Disk (/):   %s used, %s free / %s total\n",
		formatBytes(info.DiskUsed), formatBytes(info.DiskFree), formatBytes(info.DiskTotal))
	if b := info.Battery; b != nil {
		fmt.Printf("Battery:    %s (%d%%)", b.Status, b.Percentage)
		if b.TimeRemainingMins > 0 {
			fmt.Printf(", %dh%02dm remaining", b.TimeRemainingMins/60, b.TimeRemainingMins%60)
		}
		fmt.Println()
	}
}

func streamMachineLogs(cfgDirectory, machine, source string, follow bool, tail int64, since, mode string) error {
	realm, err := machineRealm(machine, cfgDirectory)
	if err != nil {
		return err
	}
	return runUntilInterrupted("Stopping log stream...", func(ctx context.Context) error {
		return deskconn.RunLogs(ctx, mode, realm, cfgDirectory, source, follow, tail, since)
	})
}

func machineRealm(machineName, cfgDirectory string) (string, error) {
	if standaloneRequested() {
		return common.StandaloneRealm, nil
	}
	machines, err := common.MachinesFromCfg(cfgDirectory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			machines, err := deskconn.FetchMachinesFromCloud(cfgDirectory)
			if err != nil {
				return "", err
			}

			if err := deskconn.CacheMachines(cfgDirectory, machines); err != nil {
				return "", err
			}

			return machineRealm(machineName, cfgDirectory)
		}
		return "", err
	}

	for _, d := range machines {
		if d.Name == machineName {
			return d.Realm, nil
		}
		if d.Authid == machineName {
			return d.Realm, nil
		}
		if d.Alias == machineName {
			return d.Realm, nil
		}
	}

	for _, d := range machines {
		if strings.HasPrefix(d.Authid, machineName) {
			return d.Realm, nil
		}
	}

	return "", fmt.Errorf("machine not found: %s", machineName)
}

func machineCompletions(cfgDirectory string) func() []string {
	return func() []string {
		machines, err := common.MachinesFromCfg(cfgDirectory)
		if err != nil {
			return nil
		}
		var names []string
		for _, d := range machines {
			if d.Name != "" {
				names = append(names, d.Name)
			}
		}
		return names
	}
}

func shortID(id string) string {
	if len(id) <= 6 {
		return id
	}
	return id[:6]
}

func connectedSince(connectedMachines map[string]any, realm string) string {
	v, ok := connectedMachines[realm]
	if !ok {
		return ""
	}
	var ts int64
	switch t := v.(type) {
	case int64:
		ts = t
	case uint64:
		ts = int64(t) // nolint: gosec
	case float64:
		ts = int64(t)
	default:
		return "connected"
	}
	return formatSince(time.Since(time.Unix(ts, 0)))
}

func formatSince(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

func toMiB(b uint64) float64 {
	return float64(b) / (1024 * 1024)
}

// formatBytes uses binary (1024-based) units, matching toMiB above and what Windows Explorer/
// macOS Finder/Linux's "df -h" show - a decimal (1000-based) unit here would print a visibly
// different, larger number for the same disk than what's shown everywhere else on the machine.
func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func updateMachineAlias(cfgDirectory, machineKey, alias string) error {
	configFile := filepath.Join(cfgDirectory, "config.yml")

	data, err := os.ReadFile(configFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no config found. User not logged in")
		}
		return fmt.Errorf("failed to read config: %w", err)
	}

	var config common.Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	machines := config.Machines
	for _, d := range machines {
		if (d.Alias == alias && d.Alias != "") || d.Authid == alias || d.Name == alias {
			return fmt.Errorf("alias '%s' already in use", alias)
		}
	}

	updated := false
	for i, d := range machines {
		if d.Authid == machineKey || d.Name == machineKey || d.Alias == machineKey {
			machines[i].Alias = alias
			updated = true
			break
		}
	}

	if !updated {
		return fmt.Errorf("machine %s not found", machineKey)
	}

	out, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	return os.WriteFile(configFile, out, 0600)
}
