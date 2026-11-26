package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"golang.org/x/term"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/connectivity"
	"github.com/xconnio/deskconn/internal/deskconn"
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
	cpProgress  *bool

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
	cmds.cpMode = modeFlag(cmds.cp, modeHelp+" (default: use the daemon's connection)")
	cmds.cpStreams = cmds.cp.Flag("streams", "Number of parallel streams to use for the transfer (default 4)").
		Short('s').Int()
	cmds.cpProgress = cmds.cp.Flag("progress",
		"Show transfer progress on stderr when it's a terminal (--no-progress to never show it)").
		Default("true").Bool()

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
		exitOnErr(listFiles(cfgDirectory, *cmds.lsTarget, *cmds.lsMode))
	case cmds.mv.FullCommand():
		exitOnErr(moveFile(cfgDirectory, *cmds.mvSrc, *cmds.mvDst, *cmds.mvMode))
	case cmds.cp.FullCommand():
		exitOnErr(copyFiles(cfgDirectory, *cmds.cpSrc, *cmds.cpDst, *cmds.cpRecursive, *cmds.cpStreams, *cmds.cpMode,
			transferProgressOut(*cmds.cpProgress)))
	case cmds.rm.FullCommand():
		exitOnErr(removeFile(cfgDirectory, *cmds.rmTarget, *cmds.rmMode))
	case cmds.cat.FullCommand():
		exitOnErr(catFile(cfgDirectory, *cmds.catTarget, *cmds.catMode))
	case cmds.edit.FullCommand():
		exitOnErr(editFile(cfgDirectory, *cmds.editTarget, *cmds.editMode))
	case cmds.mount.FullCommand():
		exitOnErr(mountFiles(cfgDirectory, *cmds.mountTarget, *cmds.mountPoint, *cmds.mountMode,
			*cmds.mountReadOnly, *cmds.mountDebug))
	default:
		return false
	}
	return true
}

func listFiles(cfgDirectory, target, mode string) error {
	machine, path := deskconn.ParseMachinePath(target)
	realm, err := deskconn.MachineRealm(machine, cfgDirectory)
	if err != nil {
		return err
	}
	args := []string{"ls"}
	if path != "" {
		args = append(args, path)
	}
	return deskconn.RunExec(context.Background(), deskconn.DefaultMode(cfgDirectory, realm, mode), realm, cfgDirectory,
		"", args)
}

func moveFile(cfgDirectory, src, dst, mode string) error {
	srcMachine, srcPath := deskconn.ParseMachinePath(src)
	dstMachine, dstPath := deskconn.ParseMachinePath(dst)
	if srcMachine != dstMachine {
		return errors.New("cross-machine move not supported")
	}
	realm, err := deskconn.MachineRealm(srcMachine, cfgDirectory)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"old_path": srcPath, "new_path": dstPath})
	return deskconn.FileOp(context.Background(), realm, cfgDirectory, common.ProcedureFileRename, payload, mode)
}

// transferProgressOut is where a transfer prints its progress: stderr, if show is set and
// stderr is a terminal, since the progress line redraws itself. Otherwise nil, for none.
func transferProgressOut(show bool) io.Writer {
	if !show || !term.IsTerminal(int(os.Stderr.Fd())) { // #nosec
		return nil
	}
	return os.Stderr
}

func copyFiles(cfgDirectory, src, dst string, recursive bool, streams int, mode string, progress io.Writer) error {
	srcRemote := deskconn.IsRemotePath(src)
	dstRemote := deskconn.IsRemotePath(dst)
	switch {
	case srcRemote && dstRemote:
		srcMachine, srcPath := deskconn.ParseMachinePath(src)
		dstMachine, dstPath := deskconn.ParseMachinePath(dst)
		if srcMachine != dstMachine {
			return errors.New("cross-machine copy not supported")
		}
		realm, err := deskconn.MachineRealm(srcMachine, cfgDirectory)
		if err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]string{"src": srcPath, "dst": dstPath})
		return deskconn.FileOp(context.Background(), realm, cfgDirectory, common.ProcedureFileCopy, payload, mode)

	case !srcRemote && dstRemote:
		dstMachine, dstPath := deskconn.ParseMachinePath(dst)
		realm, err := deskconn.MachineRealm(dstMachine, cfgDirectory)
		if err != nil {
			return err
		}
		if deskconn.ThroughDaemon(cfgDirectory, realm) && !recursive {
			info, err := os.Lstat(src)
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				return deskconn.UploadViaDaemon(cfgDirectory, realm, src, dstPath, mode, streams, progress)
			}
		}
		return deskconn.TransferFiles(cfgDirectory, realm, mode, func(session common.StreamSession) error {
			return deskconn.UploadFiles(session, src, dstPath, recursive, streams, progress)
		})

	case srcRemote && !dstRemote:
		srcMachine, srcPath := deskconn.ParseMachinePath(src)
		realm, err := deskconn.MachineRealm(srcMachine, cfgDirectory)
		if err != nil {
			return err
		}
		return deskconn.TransferFiles(cfgDirectory, realm, mode, func(session common.StreamSession) error {
			return deskconn.DownloadFiles(session, srcPath, dst, recursive, streams, progress)
		})

	default:
		return errors.New("at least one of src or dst must be a remote path (machine:path)")
	}
}

func removeFile(cfgDirectory, target, mode string) error {
	machine, path := deskconn.ParseMachinePath(target)
	realm, err := deskconn.MachineRealm(machine, cfgDirectory)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{jsonFieldPath: path})
	return deskconn.FileOp(context.Background(), realm, cfgDirectory, common.ProcedureFileDelete, payload, mode)
}

func catFile(cfgDirectory, target, mode string) error {
	machine, path := deskconn.ParseMachinePath(target)
	if path == "" {
		return errors.New("path is required")
	}
	realm, err := deskconn.MachineRealm(machine, cfgDirectory)
	if err != nil {
		return err
	}
	session, err := deskconn.ConnectMachineSession(context.Background(), cfgDirectory, realm,
		deskconn.DefaultMode(cfgDirectory, realm, mode))
	if err != nil {
		return err
	}
	if session.Proxied {
		return deskconn.CatFileViaProxy(session.Session, realm, path)
	}
	defer session.Close()
	return deskconn.CatFile(session.Session, path)
}

// editFile opens a copy of a remote text file in $EDITOR, then sends the machine only a
// patch of what changed.
func editFile(cfgDirectory, target, mode string) error {
	machine, path := deskconn.ParseMachinePath(target)
	if path == "" {
		return errors.New("path is required")
	}
	if !deskconn.IsEditableExtension(path) {
		return fmt.Errorf("%s: not a text file, editing is not supported", path)
	}
	realm, err := deskconn.MachineRealm(machine, cfgDirectory)
	if err != nil {
		return err
	}

	original, err := deskconn.ReadRemoteFile(cfgDirectory, realm, path, mode)
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
	err = deskconn.FileOp(context.Background(), realm, cfgDirectory, common.ProcedureFileEdit, payload, mode)
	if err != nil {
		return err
	}
	fmt.Println("file updated")
	return nil
}

func machinePathCompletions(cfgDirectory string) func() []string {
	return func() []string {
		names := machineNames(cfgDirectory)
		for i, name := range names {
			names[i] = name + ":"
		}
		return names
	}
}

// remotePathCompletions falls back to local filename completion when no
// "machine:" prefix is typed, and browses the remote directory once one is.
func remotePathCompletions(cfgDirectory string) func() []string {
	return func() []string {
		current := currentCompletionArg()
		if !deskconn.IsRemotePath(current) {
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
	machine, path := deskconn.ParseMachinePath(current)
	realm, err := deskconn.MachineRealm(machine, cfgDirectory)
	// Remote paths complete through the daemon's connection, so not for a standalone machine
	// while it isn't running.
	if err != nil || !deskconn.ThroughDaemon(cfgDirectory, realm) {
		return nil
	}

	dir, prefix := "", path
	if idx := strings.LastIndexByte(path, '/'); idx >= 0 {
		dir, prefix = path[:idx+1], path[idx+1:]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	localSession, err := connectivity.ConnectDaemon(ctx, cfgDirectory)
	if err != nil {
		return nil
	}
	defer func() { _ = localSession.Leave() }()

	connected, err := deskconn.ConnectedMachines(localSession)
	if _, ok := connected[realm]; err != nil || !ok {
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
