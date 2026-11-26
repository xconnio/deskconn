package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/olekukonko/tablewriter"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/connectivity"
	"github.com/xconnio/deskconn/internal/deskconn"
)

type machineCommands struct {
	ls         *kingpin.CmdClause
	lsShortcut *kingpin.CmdClause
	attach     *kingpin.CmdClause
	add        *kingpin.CmdClause
	remove     *kingpin.CmdClause
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

	addName     *string
	addURL      *string
	addCertHash *string
	addKey      *string
	addUsername *string

	removeName *string

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

	cmds.ls = machineCmd.Command("ls", "List managed and standalone machines")
	cmds.ls.Flag("refresh", "Refresh managed machines from your account").BoolVar(&cmds.lsRefresh)
	cmds.ls.Flag("detailed", "Show detailed output").BoolVar(&cmds.lsDetailed)

	cmds.attach = machineCmd.Command("attach", "Register this machine with your account")
	cmds.attachName = cmds.attach.Flag("name", "Machine name").Short('n').String()
	cmds.attachUsername = cmds.attach.Flag("username", "Username").Short('u').String()

	cmds.add = machineCmd.Command("add", "Add a standalone machine (served with `desk serve --mode standalone`) "+
		"to connect to directly")
	cmds.addName = cmds.add.Arg("name", "Name to refer to the machine by").Required().String()
	cmds.addURL = cmds.add.Flag("url", "The machine's URL: quic://host:port, or https://host:port "+
		"for WebTransport").Required().String()
	cmds.addUsername = cmds.add.Flag("username", "Username to log in as (default: current user)").String()
	cmds.addCertHash = cmds.add.Flag("cert-hash", "SHA-256 fingerprint of the machine's certificate, "+
		"as `desk serve` prints it, to trust it even if self-signed (default: verify against trusted CAs)").String()
	cmds.addKey = cmds.add.Flag("private-key", "Private key (hex) to log in with; see `desk keygen`. "+
		"Without it, the password is asked for and saved").String()

	cmds.remove = machineCmd.Command("remove", "Remove a standalone machine from this client")
	cmds.removeName = cmds.remove.Arg("name", "Name of the standalone machine").Required().
		HintAction(standaloneCompletions(cfgDirectory)).String()

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
	c.lsShortcut.Flag("refresh", "Refresh managed machines from your account").BoolVar(&c.lsRefresh)
	c.lsShortcut.Flag("detailed", "Show detailed output").BoolVar(&c.lsDetailed)
}

func dispatchMachineCommand(parsedCmd string, cmds *machineCommands, cfgDirectory string) bool {
	switch parsedCmd {
	case cmds.ls.FullCommand(), cmds.lsShortcut.FullCommand():
		exitOnErr(listMachines(cfgDirectory, cmds.lsRefresh, cmds.lsDetailed))
	case cmds.attach.FullCommand():
		exitOnErr(attach(*cmds.attachUsername, *cmds.attachName))
	case cmds.add.FullCommand():
		exitOnErr(addStandaloneMachine(cfgDirectory, common.StandaloneMachine{Name: *cmds.addName, URL: *cmds.addURL,
			CertHash: *cmds.addCertHash, PrivateKey: *cmds.addKey, Username: *cmds.addUsername}))
	case cmds.remove.FullCommand():
		exitOnErr(removeStandaloneMachine(cfgDirectory, *cmds.removeName))
	case cmds.detach.FullCommand():
		exitOnErr(detach(cfgDirectory))
	case cmds.alias.FullCommand():
		if *cmds.aliasUnset == (*cmds.aliasName != "") {
			exitOnErr(&usageError{msg: "specify either an alias name or --unset"})
		} else {
			exitOnErr(deskconn.UpdateMachineAlias(cfgDirectory, *cmds.aliasMachine, *cmds.aliasName))
		}
	case cmds.ping.FullCommand():
		exitOnErr(pingMachine(cfgDirectory, *cmds.pingMachine, *cmds.pingCount))
	case cmds.connect.FullCommand():
		exitOnErr(connectMachine(cfgDirectory, *cmds.connectMachine))
	case cmds.disconnect.FullCommand():
		exitOnErr(disconnectMachine(cfgDirectory, *cmds.disconnectMachine, *cmds.disconnectAll))
	case cmds.info.FullCommand():
		exitOnErr(showMachineInfo(cfgDirectory, *cmds.infoMachine, *cmds.infoMode))
	case cmds.logs.FullCommand():
		exitOnErr(streamMachineLogs(cfgDirectory, *cmds.logsMachine, *cmds.logsSource, *cmds.logsFollow,
			*cmds.logsTail, *cmds.logsSince, *cmds.logsMode))
	default:
		return false
	}
	return true
}

// listMachines prints the managed machines of your account, with when each got connected,
// and the standalone machines saved on this client. Managed machines come from the cached
// list unless refresh is set or there isn't one.
func listMachines(cfgDirectory string, refresh, detailed bool) error {
	standalone, err := deskconn.StandaloneMachines(cfgDirectory)
	// An unreadable config is replaced by the refresh, as it was before standalone machines.
	if err != nil && !refresh {
		return err
	}
	managed, fetched, err := managedMachinesToList(cfgDirectory, refresh, len(standalone) > 0)
	if err != nil {
		return err
	}
	if len(managed) == 0 && len(standalone) == 0 && !loggedIn(cfgDirectory) {
		fmt.Println("No machines. Sign in with `desk auth login`, or add a standalone machine with " +
			"`desk machine add <name> --url <url>`.")
		return nil
	}

	// The daemon's connections, to managed machines and standalone ones; standalone machines
	// work without the daemon too, so listing only them doesn't need it running.
	connectedMachines, err := daemonConnectedMachines(cfgDirectory)
	if err != nil && len(managed) > 0 {
		return err
	}

	tableHeader := []string{"ID", "NAME", "MODE", "ALIAS", "CONNECTED SINCE"}
	if detailed {
		tableHeader = append(tableHeader, "ORGANIZATION", "REALM", "URL")
	}
	table := tablewriter.NewWriter(os.Stdout)
	table.Header(tableHeader)
	for _, d := range managed {
		row := []any{deskconn.ShortID(d.Authid), d.Name, modeManaged, d.Alias,
			deskconn.ConnectedSince(connectedMachines, d.Realm)}
		if detailed {
			row = append(row, d.Organization.Name, d.Realm, "")
		}
		_ = table.Append(row)
	}
	for _, m := range standalone {
		row := []any{"", m.Name, modeStandalone, "",
			deskconn.ConnectedSince(connectedMachines, common.StandaloneMachineRealm(m.Name))}
		if detailed {
			row = append(row, "", "", m.URL)
		}
		_ = table.Append(row)
	}
	if err := table.Render(); err != nil {
		return err
	}
	for _, m := range standalone {
		if machine, ok := deskconn.ManagedMachineConflict(managed, m.Name); ok {
			fmt.Fprintf(os.Stderr, "warning: %v\n", ambiguousMachineError(m.Name, machine))
		}
	}
	if fetched {
		return deskconn.CacheMachines(cfgDirectory, managed)
	}
	return nil
}

// daemonConnectedMachines returns when the local daemon connected to each machine it's
// connected to, by realm.
func daemonConnectedMachines(cfgDirectory string) (map[string]any, error) {
	session, err := connectivity.ConnectDaemon(context.Background(), cfgDirectory)
	if err != nil {
		return nil, err
	}
	defer func() { _ = session.Leave() }()
	resp := session.Call(common.ProcedureConnectedMachines).Execute()
	if resp.Err != nil {
		return nil, resp.Err
	}
	connected, ok := resp.Args[0].(map[string]any)
	if !ok {
		return nil, errors.New("expected a map of strings")
	}
	return connected, nil
}

// managedMachinesToList returns the account's machines desk ls shows, and whether they were
// fetched (for the caller to cache): the cached ones, unless refresh is set or there are none
// yet. Signed out, only the cached ones. A failed fetch that wasn't asked for doesn't stop
// listing a config's standalone machines (haveStandalone), or an empty cached list.
func managedMachinesToList(cfgDirectory string, refresh, haveStandalone bool) ([]common.Machine, bool, error) {
	cached, err := common.MachinesFromCfg(cfgDirectory)
	noConfig := errors.Is(err, os.ErrNotExist)
	if err != nil && !noConfig {
		if !refresh {
			return nil, false, err
		}
		fmt.Println("your config is invalid, refreshing from cloud")
	}
	var machines []common.Machine
	for _, d := range cached {
		if d.Name != "" || d.Authid != "" || d.Alias != "" {
			machines = append(machines, d)
		}
	}
	if !refresh && (len(machines) > 0 || !loggedIn(cfgDirectory)) {
		return machines, false, nil
	}

	refreshed, err := deskconn.RefreshMachines(cfgDirectory, cached)
	if err != nil {
		if refresh || (noConfig && !haveStandalone) {
			return nil, false, err
		}
		fmt.Fprintf(os.Stderr, "warning: could not fetch managed machines: %v\n", err)
		return machines, false, nil
	}
	return refreshed, true, nil
}

// pingMachine pings machine count times (0 = until interrupted) a second apart, then prints
// statistics like ping(8).
func pingMachine(cfgDirectory, machine string, count int) error {
	realm, err := machineRealm(machine, cfgDirectory)
	if err != nil {
		return err
	}
	ping, closePing, err := machinePinger(cfgDirectory, realm)
	if err != nil {
		return err
	}
	defer closePing()

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

		ms, err := ping()
		sent++
		if err != nil {
			fmt.Fprintf(os.Stderr, "seq=%d error: %v\n", seq, err)
		} else {
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
	// Like ping(8), no reply at all is a failure; the statistics above say so already.
	if sent > 0 && received == 0 {
		return errAlreadyReported
	}
	return nil
}

// machinePinger returns a func that pings realm's machine once and returns the round trip
// in milliseconds: through the daemon (see throughDaemon), otherwise over a connection of its
// own, which the returned close func closes.
func machinePinger(cfgDirectory, realm string) (func() (int64, error), func(), error) {
	if !deskconn.ThroughDaemon(cfgDirectory, realm) {
		conn, err := connectivity.ConnectMachineRealmQUIC(context.Background(), realm, cfgDirectory)
		if err != nil {
			return nil, nil, err
		}
		return func() (int64, error) {
			start := time.Now()
			if err := conn.Call(common.ProcedurePing).Execute().Err; err != nil {
				return 0, err
			}
			return time.Since(start).Milliseconds(), nil
		}, func() { _ = conn.Connection().Close() }, nil
	}

	localSession, err := connectivity.ConnectDaemon(context.Background(), cfgDirectory)
	if err != nil {
		return nil, nil, err
	}
	return func() (int64, error) {
		callResp := localSession.Call(common.ProcedureProxyPing).Args(realm).Execute()
		if callResp.Err != nil {
			return 0, callResp.Err
		}
		ms, _ := callResp.ArgInt64(0)
		return ms, nil
	}, func() { _ = localSession.Leave() }, nil
}

func connectMachine(cfgDirectory, machine string) error {
	realm, err := machineRealm(machine, cfgDirectory)
	if err != nil {
		return err
	}
	session, err := connectivity.ConnectDaemon(context.Background(), cfgDirectory)
	if err != nil {
		return err
	}
	if err := session.Call(common.ProcedureConnect).Args(realm).Execute().Err; err != nil {
		return err
	}
	fmt.Printf("connected to %s\n", machine)
	return nil
}

func disconnectMachine(cfgDirectory, machine string, all bool) error {
	session, err := connectivity.ConnectDaemon(context.Background(), cfgDirectory)
	if err != nil {
		return err
	}
	if all {
		kwargs := map[string]any{common.DisconnectKwargAll: true}
		if err := session.Call(common.ProcedureDisconnect).Kwargs(kwargs).Execute().Err; err != nil {
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
	kwargs := map[string]any{common.DisconnectKwargRealm: realm}
	if err := session.Call(common.ProcedureDisconnect).Kwargs(kwargs).Execute().Err; err != nil {
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
	rawData, err := fetchMachineInfo(cfgDirectory, realm, deskconn.DefaultMode(cfgDirectory, realm, mode))
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
	session, err := deskconn.ConnectMachineSession(context.Background(), cfgDirectory, realm, mode)
	if err != nil {
		return nil, err
	}
	if session.Proxied {
		callResp := session.Call(common.ProcedureProxyMachineInfo).Args(realm).Execute()
		if callResp.Err != nil {
			return nil, callResp.Err
		}
		return callResp.ArgBytes(0)
	}
	defer session.Close()
	return deskconn.CallFileOp(session.Session, common.ProcedureMachineInfo, nil)
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
		return deskconn.RunLogs(ctx, deskconn.DefaultMode(cfgDirectory, realm, mode), realm, cfgDirectory, source, follow,
			tail, since, os.Stdout)
	})
}

// machineRealm returns the realm the CLI names machineName's machine by: a standalone
// machine saved on this client (see useStandaloneMachine), otherwise a managed machine of the
// account, by its name, alias, ID or the start of its ID.
func machineRealm(machineName, cfgDirectory string) (string, error) {
	if machineName == "" {
		return "", errors.New("no machine given; see desk machine ls")
	}
	standalone, err := deskconn.FindStandaloneMachine(cfgDirectory, machineName)
	switch {
	case err == nil:
		// Only the cached managed machines: a standalone machine works offline and signed out.
		managed, _ := common.MachinesFromCfg(cfgDirectory)
		if machine, ok := deskconn.ManagedMachineConflict(managed, machineName); ok {
			return "", ambiguousMachineError(machineName, machine)
		}
		return useStandaloneMachine(standalone), nil
	case !errors.Is(err, deskconn.ErrStandaloneMachineNotFound):
		return "", err
	}
	return managedMachineRealm(machineName, cfgDirectory)
}

// managedMachineRealm returns the realm of the account's machine machineName refers to,
// fetching the account's machines if none are cached yet.
func managedMachineRealm(machineName, cfgDirectory string) (string, error) {
	machines, err := common.MachinesFromCfg(cfgDirectory)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	// No config.yml yet, or one with only standalone machines: fetch the account's machines,
	// which says so when signed out.
	if len(machines) == 0 && (errors.Is(err, os.ErrNotExist) || loggedIn(cfgDirectory)) {
		machines, err = deskconn.FetchMachinesFromCloud(cfgDirectory)
		if err != nil {
			return "", err
		}
		if err := deskconn.CacheMachines(cfgDirectory, machines); err != nil {
			return "", err
		}
	}

	if d, ok := deskconn.ManagedMachineNamed(machines, machineName); ok {
		return d.Realm, nil
	}
	for _, d := range machines {
		if strings.HasPrefix(d.Authid, machineName) {
			return d.Realm, nil
		}
	}

	return "", fmt.Errorf("machine not found: %s; see desk machine ls", machineName)
}

// machineNames are the names of the managed machines cached and the standalone machines
// saved, for completing a machine argument.
func machineNames(cfgDirectory string) []string {
	var names []string
	if machines, err := common.MachinesFromCfg(cfgDirectory); err == nil {
		for _, d := range machines {
			if d.Name != "" {
				names = append(names, d.Name)
			}
		}
	}
	return append(names, standaloneNames(cfgDirectory)...)
}

func standaloneNames(cfgDirectory string) []string {
	machines, _ := deskconn.StandaloneMachines(cfgDirectory)
	names := make([]string, 0, len(machines))
	for _, m := range machines {
		names = append(names, m.Name)
	}
	return names
}

func machineCompletions(cfgDirectory string) func() []string {
	return func() []string { return machineNames(cfgDirectory) }
}

func standaloneCompletions(cfgDirectory string) func() []string {
	return func() []string { return standaloneNames(cfgDirectory) }
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
