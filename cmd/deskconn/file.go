package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/alecthomas/kingpin/v2"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
	"github.com/xconnio/xconn-core/bindings/go"
)

type fileCommands struct {
	ls    *kingpin.CmdClause
	mv    *kingpin.CmdClause
	cp    *kingpin.CmdClause
	rm    *kingpin.CmdClause
	cat   *kingpin.CmdClause
	edit  *kingpin.CmdClause
	mount *kingpin.CmdClause

	lsTarget *string
	lsMode   *string

	mvSrc  *string
	mvDst  *string
	mvMode *string

	cpSrc       *string
	cpDst       *string
	cpRecursive *bool
	cpMode      *string
	cpStreams   *int

	rmTarget *string
	rmMode   *string

	catTarget *string
	catMode   *string

	editTarget *string
	editMode   *string

	mountTarget   *string
	mountPoint    *string
	mountReadOnly *bool
	mountDebug    *bool
	mountMode     *string
}

func registerFileCommands(app *kingpin.Application, cfgDirectory string) *fileCommands {
	const modeHelp = "Connection mode: 'routed' goes via the router over QUIC, 'direct' connects over WebRTC"
	cmds := &fileCommands{}

	fileCmd := app.Command("file", "Browse, copy, and edit files")

	cmds.ls = fileCmd.Command("ls", "List files on a machine")
	cmds.lsTarget = cmds.ls.Arg("target", "Remote path as machine:path (e.g. m1:/tmp)").Required().
		HintAction(remotePathCompletions(cfgDirectory)).String()
	cmds.lsMode = modeFlag(cmds.ls, modeHelp)

	cmds.mv = fileCmd.Command("mv", "Move or rename a file or directory on a machine")
	cmds.mvSrc = cmds.mv.Arg("src", "Source path as machine:path (e.g. m1:/a.txt)").Required().
		HintAction(remotePathCompletions(cfgDirectory)).String()
	cmds.mvDst = cmds.mv.Arg("dst", "Destination path as machine:path (e.g. m1:/b.txt)").Required().
		HintAction(remotePathCompletions(cfgDirectory)).String()
	cmds.mvMode = modeFlag(cmds.mv, modeHelp)

	cmds.cp = fileCmd.Command("cp", "Copy files to/from/between machines")
	cmds.cpSrc = cmds.cp.Arg("src", "Source: machine:path for remote, /path for local").Required().
		HintAction(remotePathCompletions(cfgDirectory)).String()
	cmds.cpDst = cmds.cp.Arg("dst", "Destination: machine:path for remote, /path for local").Required().
		HintAction(remotePathCompletions(cfgDirectory)).String()
	cmds.cpRecursive = cmds.cp.Flag("recursive", "Copy directories recursively").Short('r').Bool()
	cmds.cpMode = modeFlag(cmds.cp, modeHelp+" (default: try direct, fall back to routed)")
	cmds.cpStreams = cmds.cp.Flag("streams", "Number of parallel streams to use for the transfer (default 4)").
		Short('s').Int()

	cmds.rm = fileCmd.Command("rm", "Remove a file or directory on a machine")
	cmds.rmTarget = cmds.rm.Arg("target", "Remote path as machine:path (e.g. m1:/tmp/a.txt)").Required().
		HintAction(remotePathCompletions(cfgDirectory)).String()
	cmds.rmMode = modeFlag(cmds.rm, modeHelp)

	cmds.cat = fileCmd.Command("cat", "Print the contents of a file on a machine")
	cmds.catTarget = cmds.cat.Arg("target", "Remote path as machine:path (e.g. m1:/etc/hosts)").Required().
		HintAction(remotePathCompletions(cfgDirectory)).String()
	cmds.catMode = modeFlag(cmds.cat, modeHelp)

	cmds.edit = fileCmd.Command("edit", "Edit a text file on a machine with $EDITOR and send only the diff")
	cmds.editTarget = cmds.edit.Arg("target", "Remote path as machine:path (e.g. m1:/etc/hosts)").Required().
		HintAction(remotePathCompletions(cfgDirectory)).String()
	cmds.editMode = modeFlag(cmds.edit, modeHelp)

	cmds.mount = fileCmd.Command("mount", "Mount a directory of a machine as a local filesystem (FUSE)")
	cmds.mountTarget = cmds.mount.Arg("target", "Remote directory as machine:path (e.g. m1:/home/user, m1: for home)").
		Required().HintAction(remotePathCompletions(cfgDirectory)).String()
	cmds.mountPoint = cmds.mount.Arg("mountpoint", "Local directory to mount it on").Required().ExistingDir()
	cmds.mountReadOnly = cmds.mount.Flag("read-only", "Mount read-only").Bool()
	cmds.mountDebug = cmds.mount.Flag("debug", "Log every FUSE request").Bool()
	cmds.mountMode = modeFlag(cmds.mount, modeHelp)

	return cmds
}

func dispatchFileCommand(parsedCmd string, cmds *fileCommands, cfgDirectory string) bool {
	switch parsedCmd {
	case cmds.ls.FullCommand():
		printErr(listFiles(cfgDirectory, *cmds.lsTarget, *cmds.lsMode))
	case cmds.mv.FullCommand():
		printErr(moveFile(cfgDirectory, *cmds.mvSrc, *cmds.mvDst, *cmds.mvMode))
	case cmds.cp.FullCommand():
		printErr(copyFiles(cfgDirectory, *cmds.cpSrc, *cmds.cpDst, *cmds.cpRecursive, *cmds.cpStreams, *cmds.cpMode))
	case cmds.rm.FullCommand():
		printErr(removeFile(cfgDirectory, *cmds.rmTarget, *cmds.rmMode))
	case cmds.cat.FullCommand():
		printErr(catFile(cfgDirectory, *cmds.catTarget, *cmds.catMode))
	case cmds.edit.FullCommand():
		printErr(editFile(cfgDirectory, *cmds.editTarget, *cmds.editMode))
	case cmds.mount.FullCommand():
		printErr(mountFiles(cfgDirectory, *cmds.mountTarget, *cmds.mountPoint, *cmds.mountMode,
			*cmds.mountReadOnly, *cmds.mountDebug))
	default:
		return false
	}
	return true
}

func listFiles(cfgDirectory, target, mode string) error {
	machine, path := parseMachinePath(target)
	realm, err := machineRealm(machine, cfgDirectory)
	if err != nil {
		return err
	}
	args := []string{"ls"}
	if path != "" {
		args = append(args, path)
	}
	return deskconn.RunExec(context.Background(), mode, realm, cfgDirectory, "", args)
}

func moveFile(cfgDirectory, src, dst, mode string) error {
	srcMachine, srcPath := parseMachinePath(src)
	dstMachine, dstPath := parseMachinePath(dst)
	if srcMachine != dstMachine {
		return errors.New("cross-machine move not supported")
	}
	realm, err := machineRealm(srcMachine, cfgDirectory)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"old_path": srcPath, "new_path": dstPath})
	return fileOp(context.Background(), realm, cfgDirectory, common.ProcedureFileRename, payload, mode)
}

func copyFiles(cfgDirectory, src, dst string, recursive bool, streams int, mode string) error {
	srcRemote := isRemotePath(src)
	dstRemote := isRemotePath(dst)
	switch {
	case srcRemote && dstRemote:
		srcMachine, srcPath := parseMachinePath(src)
		dstMachine, dstPath := parseMachinePath(dst)
		if srcMachine != dstMachine {
			return errors.New("cross-machine copy not supported")
		}
		realm, err := machineRealm(srcMachine, cfgDirectory)
		if err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]string{"src": srcPath, "dst": dstPath})
		return fileOp(context.Background(), realm, cfgDirectory, common.ProcedureFileCopy, payload, mode)

	case !srcRemote && dstRemote:
		dstMachine, dstPath := parseMachinePath(dst)
		realm, err := machineRealm(dstMachine, cfgDirectory)
		if err != nil {
			return err
		}
		return transferFiles(cfgDirectory, realm, mode, fileTransfer{
			daemon: func(d *deskconn.DaemonStreams) error {
				return deskconn.UploadFilesDaemon(d, src, dstPath, recursive, streams)
			},
			p2p: func(sess deskconn.ChannelOpener) error {
				return deskconn.UploadFilesP2P(sess, src, dstPath, recursive, streams)
			},
			quic: func(sess xconn.MultiplexedSession) error {
				return deskconn.UploadFilesQUIC(sess, realm, src, dstPath, recursive, streams)
			},
		})

	case srcRemote && !dstRemote:
		srcMachine, srcPath := parseMachinePath(src)
		realm, err := machineRealm(srcMachine, cfgDirectory)
		if err != nil {
			return err
		}
		return transferFiles(cfgDirectory, realm, mode, fileTransfer{
			daemon: func(d *deskconn.DaemonStreams) error {
				return deskconn.DownloadFilesDaemon(d, srcPath, dst, recursive, streams)
			},
			p2p: func(sess deskconn.ChannelOpener) error {
				return deskconn.DownloadFilesP2P(sess, srcPath, dst, recursive, streams)
			},
			quic: func(sess xconn.MultiplexedSession) error {
				return deskconn.DownloadFilesQUIC(sess, realm, srcPath, dst, recursive, streams)
			},
		})

	default:
		return errors.New("at least one of src or dst must be a remote path (machine:path)")
	}
}

// fileTransfer is one direction of a copy (upload or download) over each kind of connection.
type fileTransfer struct {
	daemon func(*deskconn.DaemonStreams) error
	p2p    func(deskconn.ChannelOpener) error
	quic   func(xconn.MultiplexedSession) error
}

// transferFiles runs transfer over mode's connection to realm. Without a mode it goes through
// the local daemon, or when the daemon isn't running, over p2p, falling back to quic.
func transferFiles(cfgDirectory, realm, mode string, transfer fileTransfer) error {
	ctx := context.Background()
	viaQUIC := func() error {
		quicSess, err := common.ConnectMachineRealmQUIC(ctx, realm, cfgDirectory)
		if err != nil {
			return err
		}
		defer quicSess.Close()
		return transfer.quic(quicSess)
	}

	switch mode {
	case ModeRouted:
		return viaQUIC()
	case ModeDirect:
		p2pSess, err := deskconn.ConnectMachineRealmP2PSession(ctx, realm, cfgDirectory)
		if err != nil {
			return err
		}
		defer func() { _ = p2pSess.Close() }()
		return transfer.p2p(deskconn.P2PChannels(p2pSess))
	}

	daemon, err := deskconn.DialDaemonStreams(ctx, realm, cfgDirectory)
	if !errors.Is(err, deskconn.ErrDaemonUnavailable) {
		if err != nil {
			return err
		}
		return transfer.daemon(daemon)
	}
	p2pSess, err := deskconn.ConnectMachineRealmP2PSession(ctx, realm, cfgDirectory)
	if err == nil {
		defer func() { _ = p2pSess.Close() }()
		return transfer.p2p(deskconn.P2PChannels(p2pSess))
	}
	fmt.Fprintln(os.Stderr, "direct connection unavailable, falling back to routed")
	return viaQUIC()
}

func removeFile(cfgDirectory, target, mode string) error {
	machine, path := parseMachinePath(target)
	realm, err := machineRealm(machine, cfgDirectory)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{jsonFieldPath: path})
	return fileOp(context.Background(), realm, cfgDirectory, common.ProcedureFileDelete, payload, mode)
}

func catFile(cfgDirectory, target, mode string) error {
	machine, path := parseMachinePath(target)
	if path == "" {
		return errors.New("path is required")
	}
	realm, err := machineRealm(machine, cfgDirectory)
	if err != nil {
		return err
	}
	switch mode {
	case ModeRouted:
		quicSess, err := common.ConnectMachineRealmQUIC(context.Background(), realm, cfgDirectory)
		if err != nil {
			return err
		}
		defer quicSess.Connection().Close()
		return deskconn.CatFile(quicSess.Session, path)
	case ModeDirect:
		p2pSess, err := deskconn.ConnectMachineRealmP2P(context.Background(), realm, cfgDirectory)
		if err != nil {
			return err
		}
		defer func() { _ = p2pSess.Leave() }()
		return deskconn.CatFile(p2pSess, path)
	default:
		localSession, err := xconn.ConnectAnonymous(context.Background(), common.DaemonURI(cfgDirectory), common.LocalRealm)
		if err != nil {
			return err
		}
		return deskconn.CatFileViaProxy(localSession, realm, path)
	}
}

// editFile opens a copy of a remote text file in $EDITOR, then sends the machine only a
// patch of what changed.
func editFile(cfgDirectory, target, mode string) error {
	machine, path := parseMachinePath(target)
	if path == "" {
		return errors.New("path is required")
	}
	if !deskconn.IsEditableExtension(path) {
		return fmt.Errorf("%s: not a text file, editing is not supported", path)
	}
	realm, err := machineRealm(machine, cfgDirectory)
	if err != nil {
		return err
	}

	original, err := readRemoteFile(cfgDirectory, realm, path, mode)
	if err != nil {
		return err
	}

	tmpFile, err := os.CreateTemp("", "deskconn-edit-*-"+filepath.Base(path))
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmpFile.Write(original); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}

	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}

	editCmd := exec.Command(editor, tmpPath) // nolint: gosec
	editCmd.Stdin = os.Stdin
	editCmd.Stdout = os.Stdout
	editCmd.Stderr = os.Stderr
	if err := editCmd.Run(); err != nil {
		return err
	}

	edited, err := os.ReadFile(tmpPath)
	if err != nil {
		return err
	}

	if bytes.Equal(original, edited) {
		fmt.Println("no changes made")
		return nil
	}

	patch := deskconn.BuildEditPatch(original, edited)
	payload, _ := json.Marshal(map[string]string{jsonFieldPath: path, "patch": patch})
	if err := fileOp(context.Background(), realm, cfgDirectory, common.ProcedureFileEdit, payload, mode); err != nil {
		return err
	}
	fmt.Println("file updated")
	return nil
}

func readRemoteFile(cfgDirectory, realm, path, mode string) ([]byte, error) {
	switch mode {
	case ModeRouted:
		quicSess, err := common.ConnectMachineRealmQUIC(context.Background(), realm, cfgDirectory)
		if err != nil {
			return nil, err
		}
		defer quicSess.Connection().Close()
		return deskconn.ReadFile(quicSess.Session, path)
	case ModeDirect:
		p2pSess, err := deskconn.ConnectMachineRealmP2P(context.Background(), realm, cfgDirectory)
		if err != nil {
			return nil, err
		}
		defer func() { _ = p2pSess.Leave() }()
		return deskconn.ReadFile(p2pSess, path)
	default:
		localSession, err := xconn.ConnectAnonymous(context.Background(), common.DaemonURI(cfgDirectory), common.LocalRealm)
		if err != nil {
			return nil, err
		}
		return deskconn.ReadFileViaProxy(localSession, realm, path)
	}
}

func fileOp(ctx context.Context, realm, cfgDirectory, procedure string, payload []byte, mode string) error {
	switch mode {
	case ModeRouted:
		quicSess, err := common.ConnectMachineRealmQUIC(ctx, realm, cfgDirectory)
		if err != nil {
			return err
		}
		defer quicSess.Connection().Close()
		_, err = deskconn.CallFileOp(quicSess.Session, procedure, payload)
		return err
	case ModeDirect:
		p2pSess, err := deskconn.ConnectMachineRealmP2P(ctx, realm, cfgDirectory)
		if err != nil {
			return err
		}
		defer func() { _ = p2pSess.Leave() }()
		_, err = deskconn.CallFileOp(p2pSess, procedure, payload)
		return err
	default:
		localSession, err := xconn.ConnectAnonymous(ctx, common.DaemonURI(cfgDirectory), common.LocalRealm)
		if err != nil {
			return err
		}
		resp := localSession.Call(common.ProcedureProxyFileOp).Args(realm, procedure, payload).Execute()
		return resp.Err
	}
}

func machinePathCompletions(cfgDirectory string) func() []string {
	return func() []string {
		machines, err := common.MachinesFromCfg(cfgDirectory)
		if err != nil {
			return nil
		}
		var names []string
		for _, d := range machines {
			if d.Name != "" {
				names = append(names, d.Name+":")
			}
		}
		return names
	}
}

// remotePathCompletions falls back to local filename completion when no
// "machine:" prefix is typed, and browses the remote directory once one is.
func remotePathCompletions(cfgDirectory string) func() []string {
	return func() []string {
		current := currentCompletionArg()
		if !isRemotePath(current) {
			return machinePathCompletions(cfgDirectory)()
		}
		return remoteMachinePathCompletions(cfgDirectory, current)
	}
}

// currentCompletionArg returns the partial word under the cursor, passed as
// the last arg when kingpin's generated script re-invokes the binary with
// "--completion-bash".
func currentCompletionArg() string {
	if len(os.Args) == 0 {
		return ""
	}
	return os.Args[len(os.Args)-1]
}

// remoteMachinePathCompletions lists entries of the directory being typed in current.
func remoteMachinePathCompletions(cfgDirectory, current string) []string {
	machine, path := parseMachinePath(current)
	realm, err := machineRealm(machine, cfgDirectory)
	if err != nil {
		return nil
	}

	dir, prefix := "", path
	if idx := strings.LastIndexByte(path, '/'); idx >= 0 {
		dir, prefix = path[:idx+1], path[idx+1:]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	localSession, err := xconn.ConnectAnonymous(ctx, common.DaemonURI(cfgDirectory), common.LocalRealm)
	if err != nil {
		return nil
	}
	defer func() { _ = localSession.Leave() }()

	if !machineHasPersistentSession(localSession, realm) {
		_ = localSession.Call(common.ProcedureConnect).Args(realm).Execute()
		return nil
	}

	browsePayload, err := json.Marshal(map[string]string{jsonFieldPath: dir})
	if err != nil {
		return nil
	}
	resp := localSession.Call(common.ProcedureProxyFileOp).Args(realm, common.ProcedureFileBrowse, browsePayload).Execute()
	if resp.Err != nil {
		return nil
	}
	result, err := resp.ArgBytes(0)
	if err != nil {
		return nil
	}

	var browsed common.FileBrowseResult
	if err := json.Unmarshal(result, &browsed); err != nil {
		return nil
	}

	var out []string
	for _, entry := range browsed.Entries {
		if !strings.HasPrefix(entry.Name, prefix) {
			continue
		}
		completion := machine + ":" + dir + entry.Name
		if entry.IsDir {
			completion += "/"
		}
		out = append(out, completion)
	}
	return out
}

func machineHasPersistentSession(localSession *xconn.Session, realm string) bool {
	resp := localSession.Call(common.ProcedureConnectedMachines).Execute()
	if resp.Err != nil || len(resp.Args) == 0 {
		return false
	}
	connected, ok := resp.Args[0].(map[string]any)
	if !ok {
		return false
	}
	_, ok = connected[realm]
	return ok
}

func isRemotePath(s string) bool {
	return strings.ContainsRune(s, ':')
}

func parseMachinePath(s string) (machine, path string) {
	idx := strings.IndexByte(s, ':')
	if idx < 0 {
		return s, ""
	}
	return s[:idx], s[idx+1:]
}
