package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/alecthomas/kingpin/v2"

	"github.com/xconnio/deskconn/internal/deskconn"
)

type selfCommands struct {
	version *kingpin.CmdClause
	update  *kingpin.CmdClause
	remove  *kingpin.CmdClause

	removeYes *bool
}

func registerSelfCommands(app *kingpin.Application) *selfCommands {
	selfCmd := app.Command("self", "Manage the installed CLI")
	versionCmd := selfCmd.Command("version", "Show the installed desk version")
	updateCmd := selfCmd.Command("update", "Check for updates and install the latest release")
	removeCmd := selfCmd.Command("remove", "Remove desk from this machine")
	removeYes := removeCmd.Flag("yes", "Do not prompt for confirmation").Short('y').Bool()

	return &selfCommands{
		version:   versionCmd,
		update:    updateCmd,
		remove:    removeCmd,
		removeYes: removeYes,
	}
}

func dispatchSelfCommand(parsedCmd string, cmds *selfCommands, cfgDirectory string) bool {
	switch parsedCmd {
	case cmds.version.FullCommand():
		fmt.Printf("desk %s\n", version)
	case cmds.update.FullCommand():
		printErr(updateApp())
	case cmds.remove.FullCommand():
		if err := removeApp(cfgDirectory, *cmds.removeYes); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		return false
	}
	return true
}

// legacyCLIPaths lists files left behind by older installs: the pre-rename "deskconn" CLI and
// the removed "dsh"/"dcp" shortcut wrappers, along with their shell completions.
func legacyCLIPaths(binDir, bashCompDir, zshCompDir string) []string {
	var paths []string
	for _, name := range []string{binaryName, "dsh", "dcp"} {
		paths = append(paths,
			filepath.Join(binDir, name),
			filepath.Join(bashCompDir, name),
			filepath.Join(zshCompDir, "_"+name),
		)
	}
	return paths
}

// The installed layout: the deskconn binary lives in ~/.local/lib/exec, desk on the PATH
// is a symlink to it, and the deskconn-userd service runs it as "deskconn serve --mode user".
const (
	binaryName    = "deskconn"
	cliName       = "desk"
	legacyService = "deskconnd"
)

// launchdLabel is the launchd agent install.sh runs deskconn-userd as on macOS.
const launchdLabel = "com.deskconn." + userdService

// launchdTarget is the deskconn-userd agent in the current user's GUI session.
func launchdTarget() string {
	return fmt.Sprintf("gui/%d/%s", os.Getuid(), launchdLabel)
}

// launchdPlist is where install.sh writes the deskconn-userd agent's plist.
func launchdPlist(homeDir string) string {
	return filepath.Join(homeDir, "Library", "LaunchAgents", launchdLabel+".plist")
}

// userdExecStart is the command the deskconn-userd service runs in mode.
func userdExecStart(execDir, mode string) string {
	return filepath.Join(execDir, binaryName) + " serve --mode " + mode
}

// installDirs returns the PATH directory desk is linked into and the directory the
// deskconn binary lives in.
func installDirs(homeDir string) (binDir, execDir string) {
	return filepath.Join(homeDir, ".local", "bin"), filepath.Join(homeDir, ".local", "lib", "exec")
}

// legacyDaemonPaths lists what older installs ran the daemons from: the deskconn-userd and
// deskconn-rootd symlinks to deskconn, the separate deskconnd before that, and the vpnd
// symlink to desk.
func legacyDaemonPaths(binDir, execDir string) []string {
	var paths []string
	for _, name := range []string{userdService, deskconn.RootdName, legacyService} {
		paths = append(paths, filepath.Join(execDir, name))
	}
	return append(paths, filepath.Join(binDir, "vpnd"))
}

type appUpdateResponse struct {
	DownloadURL   string `json:"download_url"`
	LatestVersion string `json:"latest_version"`
}

func updateApp() error {
	fmt.Printf("Checking for updates for %s on %s-%s...\n", version, runtime.GOOS, runtime.GOARCH)

	updateResp, err := latestAppUpdate()
	if err != nil {
		return err
	}

	if updateResp.DownloadURL == "" {
		fmt.Printf("You're already on version %s of desk (the latest version).\n", version)
		return nil
	}

	fmt.Printf("Found update at %s.\n", updateResp.DownloadURL)

	if err := downloadAndInstallUpdate(updateResp.DownloadURL); err != nil {
		return err
	}

	if err := removeLegacyXlinkService(); err != nil {
		return err
	}
	if err := migrateUserdService(); err != nil {
		return err
	}

	fmt.Printf("Restarting %s service...\n", userdService)
	cmd := exec.Command("systemctl", "--user", "restart", userdService) // nolint: gosec
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("launchctl", "kickstart", "-k", launchdTarget()) // nolint: gosec
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to restart %s: %w", userdService, err)
	}

	fmt.Printf("Updated desk from version %s to %s.\n", version, updateResp.LatestVersion)
	return nil
}

// removeLegacyXlinkService stops and removes the separate xlink service and binary that
// installs before deskconnd embedded xlink had; left running, it would compete with
// deskconnd for deskconn.sock.
func removeLegacyXlinkService() error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get user home dir: %w", err)
	}

	serviceFile := filepath.Join(homeDir, ".config", "systemd", "user", "xlink.service")
	if _, err := os.Stat(serviceFile); err == nil {
		fmt.Println("Removing the old xlink service (now part of deskconn-userd)...")
		_ = exec.Command("systemctl", "--user", "stop", "xlink").Run()    // nolint: gosec
		_ = exec.Command("systemctl", "--user", "disable", "xlink").Run() // nolint: gosec
		if err := os.Remove(serviceFile); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove xlink service file: %w", err)
		}
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run() // nolint: gosec
	}

	xlinkBin := filepath.Join(homeDir, ".local", "lib", "exec", "xlink")
	if err := os.Remove(xlinkBin); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove %s: %w", xlinkBin, err)
	}
	return nil
}

// migrateUserdService points the deskconn-userd service of older installs, and the
// deskconnd.service of installs before that, at "deskconn serve" (see migrateExecStart),
// keeping the environment the installer captured in it (DISPLAY, WAYLAND_DISPLAY) and the
// flags added to it in drop-ins.
func migrateUserdService() error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get user home dir: %w", err)
	}
	_, execDir := installDirs(homeDir)
	systemdDir := filepath.Join(homeDir, ".config", "systemd", "user")
	userdFile := filepath.Join(systemdDir, userdService+".service")

	legacyFile := filepath.Join(systemdDir, legacyService+".service")
	data, err := os.ReadFile(legacyFile)
	fromLegacy := err == nil
	if fromLegacy {
		fmt.Printf("Replacing the %s service with %s...\n", legacyService, userdService)
		_ = exec.Command("systemctl", "--user", "stop", legacyService).Run()    // nolint: gosec
		_ = exec.Command("systemctl", "--user", "disable", legacyService).Run() // nolint: gosec
	} else if os.IsNotExist(err) {
		data, err = os.ReadFile(userdFile)
	}
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read %s service: %w", userdService, err)
	}

	unit := migrateExecStart(string(data), execDir)
	unit = strings.Replace(unit, "Description=deskconnd daemon", "Description=deskconn user daemon", 1)
	if fromLegacy || unit != string(data) {
		if err := os.WriteFile(userdFile, []byte(unit), 0644); err != nil { // nolint: gosec
			return fmt.Errorf("failed to write %s service: %w", userdService, err)
		}
	}
	if fromLegacy {
		if err := os.Remove(legacyFile); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove %s: %w", legacyFile, err)
		}
	}

	dropIns, _ := filepath.Glob(filepath.Join(systemdDir, userdService+".service.d", "*.conf"))
	for _, dropIn := range dropIns {
		data, err := os.ReadFile(dropIn)
		if err != nil {
			return fmt.Errorf("failed to read %s: %w", dropIn, err)
		}
		if updated := migrateExecStart(string(data), execDir); updated != string(data) {
			if err := os.WriteFile(dropIn, []byte(updated), 0644); err != nil { // nolint: gosec
				return fmt.Errorf("failed to write %s: %w", dropIn, err)
			}
		}
	}

	_ = exec.Command("systemctl", "--user", "daemon-reload").Run() // nolint: gosec
	if fromLegacy {
		if err := exec.Command("systemctl", "--user", "enable", userdService).Run(); err != nil { // nolint: gosec
			return fmt.Errorf("failed to enable %s: %w", userdService, err)
		}
	}
	return nil
}

// migrateExecStart rewrites the ExecStart lines of a deskconn-userd unit (or drop-in) that
// run the daemon from execDir, by its old names, to run "deskconn serve" instead, keeping any
// flags after it: "--mode standalone" for those with the old --standalone flag, otherwise
// "--mode user".
func migrateExecStart(unit, execDir string) string {
	lines := strings.Split(unit, "\n")
	for i, line := range lines {
		for _, name := range []string{userdService, legacyService} {
			oldStart := "ExecStart=" + filepath.Join(execDir, name)
			if line != oldStart && !strings.HasPrefix(line, oldStart+" ") {
				continue
			}
			args := strings.TrimPrefix(line, oldStart)
			mode := serveModeUser
			if strings.Contains(args+" ", " --standalone ") {
				args = strings.Replace(args+" ", " --standalone ", " ", 1)
				args = strings.TrimSuffix(args, " ")
				mode = serveModeStandalone
			}
			lines[i] = "ExecStart=" + userdExecStart(execDir, mode) + args
		}
	}
	return strings.Join(lines, "\n")
}

func latestAppUpdate() (appUpdateResponse, error) {
	const releaseURL = "https://api.github.com/repos/xconnio/deskconn/releases/latest"

	client := &http.Client{
		Timeout: 30 * time.Second,
	}
	req, err := http.NewRequest(http.MethodGet, releaseURL, nil)
	if err != nil {
		return appUpdateResponse{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "desk-self-update")

	resp, err := client.Do(req)
	if err != nil {
		return appUpdateResponse{}, fmt.Errorf("failed to check latest release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return appUpdateResponse{}, fmt.Errorf("failed to check latest release: unexpected status %s", resp.Status)
	}

	var release struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return appUpdateResponse{}, fmt.Errorf("failed to parse latest release: %w", err)
	}

	if release.TagName == "" {
		return appUpdateResponse{}, fmt.Errorf("latest release response missing tag_name")
	}
	if strings.TrimPrefix(version, "v") == strings.TrimPrefix(release.TagName, "v") {
		return appUpdateResponse{LatestVersion: release.TagName}, nil
	}

	assetName := fmt.Sprintf("deskconn_%s_%s_%s.tar.gz", strings.TrimPrefix(release.TagName, "v"),
		runtime.GOOS, runtime.GOARCH)
	for _, asset := range release.Assets {
		if asset.Name == assetName {
			if asset.BrowserDownloadURL == "" {
				return appUpdateResponse{}, fmt.Errorf("latest release asset %s missing browser_download_url", assetName)
			}
			return appUpdateResponse{
				DownloadURL:   asset.BrowserDownloadURL,
				LatestVersion: release.TagName,
			}, nil
		}
	}

	return appUpdateResponse{}, fmt.Errorf("latest release %s missing asset %s", release.TagName, assetName)
}

const pathInstallerBlock = "\n# Added by deskconn installer\nexport PATH=\"$HOME/.local/bin:$PATH\"\n"

func removeApp(cfgDirectory string, skipConfirm bool) error {
	if !skipConfirm {
		confirmed, err := confirmPrompt("Are you sure you want to remove desk and all configuration, "+
			"credentials and machine list? (y/N): ", false)
		if err != nil {
			return err
		}
		if !confirmed {
			fmt.Println("remove cancelled")
			return nil
		}
	}

	if err := detachIfAttached(cfgDirectory); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to detach this machine from your account: %v\n", err)
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get user home dir: %w", err)
	}

	if err := removeLegacyXlinkService(); err != nil {
		return err
	}

	for _, serviceName := range []string{userdService, legacyService} {
		serviceFile := filepath.Join(homeDir, ".config", "systemd", "user", serviceName+".service")
		if _, err := os.Stat(serviceFile); err != nil {
			continue
		}
		fmt.Printf("Stopping %s service...\n", serviceName)
		_ = exec.Command("systemctl", "--user", "stop", serviceName).Run()    // nolint: gosec
		_ = exec.Command("systemctl", "--user", "disable", serviceName).Run() // nolint: gosec
		if err := os.Remove(serviceFile); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove service file: %w", err)
		}
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run() // nolint: gosec
	}

	if runtime.GOOS == "darwin" {
		plist := launchdPlist(homeDir)
		if _, err := os.Stat(plist); err == nil {
			fmt.Printf("Stopping %s service...\n", userdService)
			_ = exec.Command("launchctl", "bootout", launchdTarget()).Run() // nolint: gosec
			if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("failed to remove launchd plist: %w", err)
			}
		}
	}

	binDir, execDir := installDirs(homeDir)
	bashCompDir := filepath.Join(homeDir, ".local", "share", "bash-completion", "completions")
	zshCompDir := filepath.Join(homeDir, ".local", "share", "zsh", "site-functions")

	paths := []string{
		filepath.Join(binDir, cliName),
		filepath.Join(execDir, binaryName),
		filepath.Join(bashCompDir, cliName),
		filepath.Join(zshCompDir, "_"+cliName),
	}
	paths = append(paths, legacyDaemonPaths(binDir, execDir)...)
	paths = append(paths, legacyCLIPaths(binDir, bashCompDir, zshCompDir)...)

	fmt.Println("Removing desk binaries and shell completions...")
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove %s: %w", p, err)
		}
	}

	for _, rcFile := range []string{".bashrc", ".bash_profile", ".zshrc", ".profile"} {
		if err := removePathInstallerBlock(filepath.Join(homeDir, rcFile)); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to clean up %s: %v\n", rcFile, err)
		}
	}

	fmt.Println("Removing desk configuration, credentials and machine list...")
	if err := os.RemoveAll(cfgDirectory); err != nil {
		return fmt.Errorf("failed to remove config directory: %w", err)
	}

	fmt.Println("desk has been removed.")
	return nil
}

// removePathInstallerBlock strips the exact PATH block install.sh appends to shell rc files.
// It is a no-op if the file doesn't exist or doesn't contain the block.
func removePathInstallerBlock(rcFile string) error {
	data, err := os.ReadFile(rcFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	if !strings.Contains(string(data), pathInstallerBlock) {
		return nil
	}

	updated := strings.Replace(string(data), pathInstallerBlock, "", 1)
	info, err := os.Stat(rcFile)
	if err != nil {
		return err
	}
	return os.WriteFile(rcFile, []byte(updated), info.Mode()) // nolint: gosec
}

func downloadAndInstallUpdate(downloadURL string) error {
	client := &http.Client{
		Timeout: 5 * time.Minute,
	}

	fmt.Printf("Downloading update from %s...\n", downloadURL)
	resp, err := client.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("failed to download update: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to download update: unexpected status %s", resp.Status)
	}

	gzipReader, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to open update archive: %w", err)
	}
	defer gzipReader.Close()

	tarReader := tar.NewReader(gzipReader)
	fmt.Println("Extracting update archive...")

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get user home dir: %w", err)
	}

	binDir, execDir := installDirs(homeDir)
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return fmt.Errorf("failed to create bin dir: %w", err)
	}
	if err := os.MkdirAll(execDir, 0755); err != nil {
		return fmt.Errorf("failed to create exec dir: %w", err)
	}

	found := false
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read update archive: %w", err)
		}

		if header.Typeflag != tar.TypeReg || filepath.Base(header.Name) != binaryName {
			continue
		}
		fmt.Printf("Installing %s...\n", binaryName)
		if err := installBinaryFromReader(tarReader, filepath.Join(execDir, binaryName), 0755); err != nil {
			return err
		}
		found = true
	}
	if !found {
		return fmt.Errorf("update archive missing the %s binary", binaryName)
	}

	if err := linkCLI(binDir, execDir); err != nil {
		return err
	}

	if err := installCompletions(filepath.Join(binDir, cliName)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to update shell completions: %v\n", err)
	}

	bashCompDir := filepath.Join(homeDir, ".local", "share", "bash-completion", "completions")
	zshCompDir := filepath.Join(homeDir, ".local", "share", "zsh", "site-functions")
	for _, p := range append(legacyDaemonPaths(binDir, execDir), legacyCLIPaths(binDir, bashCompDir, zshCompDir)...) {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "warning: failed to remove %s: %v\n", p, err)
		}
	}

	return nil
}

// linkCLI points desk on the PATH at the deskconn binary in execDir.
func linkCLI(binDir, execDir string) error {
	link := filepath.Join(binDir, cliName)
	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to replace %s: %w", link, err)
	}
	if err := os.Symlink(filepath.Join(execDir, binaryName), link); err != nil {
		return fmt.Errorf("failed to link %s: %w", link, err)
	}
	return nil
}

func installCompletions(deskBin string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home dir: %w", err)
	}

	bashDir := filepath.Join(homeDir, ".local", "share", "bash-completion", "completions")
	zshDir := filepath.Join(homeDir, ".local", "share", "zsh", "site-functions")

	for _, dir := range []string{bashDir, zshDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}

	bashOut, err := exec.Command(deskBin, "--completion-script-bash").Output() // nolint: gosec
	if err != nil {
		return fmt.Errorf("generating bash completion: %w", err)
	}
	bashScript := fixBashCompletionSpacing(fixBashCompletionWordBreaks(string(bashOut)))
	if err := os.WriteFile(filepath.Join(bashDir, "desk"), []byte(bashScript), 0644); err != nil { // nolint: gosec
		return fmt.Errorf("writing bash completion: %w", err)
	}

	zshOut, err := exec.Command(deskBin, "--completion-script-zsh").Output() // nolint: gosec
	if err != nil {
		return fmt.Errorf("generating zsh completion: %w", err)
	}
	if err := os.WriteFile(filepath.Join(zshDir, "_desk"), zshOut, 0644); err != nil { // nolint: gosec
		return fmt.Errorf("writing zsh completion: %w", err)
	}

	return nil
}

// fixBashCompletionWordBreaks clears ':' from COMP_WORDBREAKS so bash
// completes "machine:path" as one word instead of splitting at the colon.
func fixBashCompletionWordBreaks(script string) string {
	const marker = "_desk_bash_autocomplete() {\n"
	return strings.Replace(script, marker, marker+"    COMP_WORDBREAKS=${COMP_WORDBREAKS//:}\n", 1)
}

// fixBashCompletionSpacing makes path completions behave like "cd": the menu shows only the last
// path segment (-o filenames), and no trailing space is added after directories/"machine:" prefixes.
// Leaf files are left to bash's default space-append.
func fixBashCompletionSpacing(script string) string {
	const marker = `    COMPREPLY=( $(compgen -W "${opts}" -- ${cur}) )` + "\n"
	const replacement = marker +
		"    compopt -o filenames 2>/dev/null || true\n" +
		`    if [ "${#COMPREPLY[@]}" -eq 1 ] && [[ "${COMPREPLY[0]}" == */ || "${COMPREPLY[0]}" == *: ]]; then` + "\n" +
		"        compopt -o nospace 2>/dev/null || true\n" +
		"    fi\n"
	return strings.Replace(script, marker, replacement, 1)
}

func installBinaryFromReader(src io.Reader, dst string, mode os.FileMode) error {
	tmpFile, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file for %s: %w", dst, err)
	}

	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		_ = tmpFile.Close()
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := io.Copy(tmpFile, src); err != nil {
		return fmt.Errorf("failed to write %s: %w", dst, err)
	}

	if err := tmpFile.Chmod(mode); err != nil {
		return fmt.Errorf("failed to chmod %s: %w", dst, err)
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close %s: %w", dst, err)
	}

	if err := os.Rename(tmpPath, dst); err != nil {
		return fmt.Errorf("failed to install %s: %w", dst, err)
	}

	cleanup = false
	return nil
}
