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
)

type selfCommands struct {
	version *kingpin.CmdClause
	update  *kingpin.CmdClause
	remove  *kingpin.CmdClause
	// installCompletions is run by install.sh, so it applies the same completion fixes as
	// "desk self update".
	installCompletions *kingpin.CmdClause

	removeYes *bool
}

func registerSelfCommands(app *kingpin.Application) *selfCommands {
	selfCmd := app.Command("self", "Manage the installed CLI")
	versionCmd := selfCmd.Command("version", "Show the installed desk version")
	updateCmd := selfCmd.Command("update", "Check for updates and install the latest release")
	removeCmd := selfCmd.Command("remove", "Remove desk from this machine")
	removeYes := removeCmd.Flag("yes", "Do not prompt for confirmation").Short('y').Bool()
	installCompletionsCmd := selfCmd.Command("install-completions",
		"Install shell completions (used by install.sh)").Hidden()

	return &selfCommands{
		version:            versionCmd,
		update:             updateCmd,
		remove:             removeCmd,
		installCompletions: installCompletionsCmd,
		removeYes:          removeYes,
	}
}

func dispatchSelfCommand(parsedCmd string, cmds *selfCommands, cfgDirectory string) bool {
	switch parsedCmd {
	case cmds.version.FullCommand():
		fmt.Printf("desk %s\n", version)
	case cmds.update.FullCommand():
		exitOnErr(updateApp())
	case cmds.remove.FullCommand():
		exitOnErr(removeApp(cfgDirectory, *cmds.removeYes))
	case cmds.installCompletions.FullCommand():
		exitOnErr(installCompletionsForSelf())
	default:
		return false
	}
	return true
}

// The installed layout: the deskconn binary lives in ~/.local/lib/exec, desk on the PATH
// is a symlink to it, and the deskconn service runs it as "deskconn serve --mode user".
const (
	binaryName = "deskconn"
	cliName    = "desk"
)

// launchdTarget is the label launchd agent in the current user's GUI session.
func launchdTarget(label string) string {
	return fmt.Sprintf("gui/%d/%s", os.Getuid(), label)
}

// launchdAgentLoaded reports whether the label launchd agent is loaded.
func launchdAgentLoaded(label string) bool {
	return exec.Command("launchctl", "print", launchdTarget(label)).Run() == nil // nolint: gosec
}

// launchdPlist is where install.sh writes the label agent's plist.
func launchdPlist(homeDir, label string) string {
	return filepath.Join(homeDir, "Library", "LaunchAgents", label+".plist")
}

// launchdLogDir is where install.sh has launchd write the deskconn agent's output.
func launchdLogDir(homeDir string) string {
	return filepath.Join(homeDir, "Library", "Logs", "deskconn")
}

// installDirs returns the PATH directory desk is linked into and the directory the
// deskconn binary lives in.
func installDirs(homeDir string) (binDir, execDir string) {
	return filepath.Join(homeDir, ".local", "bin"), filepath.Join(homeDir, ".local", "lib", "exec")
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

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get user home dir: %w", err)
	}
	// Moving the service from deskconn-userd starts it with the new binary already.
	migrated, err := migrateService(homeDir)
	if err != nil {
		return err
	}
	if !migrated {
		if err := restartUserd(); err != nil {
			return err
		}
	}

	fmt.Printf("Updated desk from version %s to %s.\n", version, updateResp.LatestVersion)
	return nil
}

// restartUserd restarts the deskconn service so it runs the updated binary. On macOS it's
// only restarted when it runs as the launchd agent install.sh sets up; started any other
// way, it's left for the user to restart. An agent still named as before the rename is
// restarted as it is: install.sh renames it.
func restartUserd() error {
	cmd := exec.Command("systemctl", "--user", "restart", serviceName) // nolint: gosec
	if runtime.GOOS == "darwin" {
		label := launchdLabel
		if !launchdAgentLoaded(label) && launchdAgentLoaded(legacyLaunchdLabel) {
			label = legacyLaunchdLabel
		}
		if !launchdAgentLoaded(label) {
			fmt.Printf("%s isn't running as the %s launchd agent, so it wasn't restarted.\n",
				serviceName, launchdLabel)
			fmt.Println("Restart it yourself to run the new version.")
			return nil
		}
		cmd = exec.Command("launchctl", "kickstart", "-k", launchdTarget(label)) // nolint: gosec
		forgetFullDiskAccess()
	}

	fmt.Printf("Restarting %s service...\n", serviceName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to restart %s: %w", serviceName, err)
	}
	if homeDir, err := os.UserHomeDir(); err == nil {
		reportFullDiskAccess(homeDir)
	}
	return nil
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

const (
	pathInstallerBlock = "\n# Added by deskconn installer\nexport PATH=\"$HOME/.local/bin:$PATH\"\n"

	zshCompletionBlock = "\n# Added by deskconn installer (deskconn completion)\n" +
		"if [ -d \"$HOME/.local/share/zsh/site-functions\" ]; then\n" +
		"    fpath=(\"$HOME/.local/share/zsh/site-functions\" $fpath)\n" +
		"    autoload -Uz _desk 2>/dev/null\n" +
		"    compdef _desk desk deskconn 2>/dev/null\n" +
		"fi\n"

	bashCompletionBlock = "\n# Added by deskconn installer (deskconn completion)\n" +
		"if [ -f \"$HOME/.local/share/bash-completion/completions/desk\" ]; then\n" +
		"    . \"$HOME/.local/share/bash-completion/completions/desk\" 2>/dev/null\n" +
		"fi\n"
)

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

	// The service may still be deskconn-userd, as before the rename, or be both midway through
	// it.
	for _, name := range []string{serviceName, legacyServiceName} {
		serviceFile := filepath.Join(homeDir, ".config", "systemd", "user", name+".service")
		if _, err := os.Stat(serviceFile); err != nil {
			continue
		}
		fmt.Printf("Stopping %s service...\n", name)
		_ = exec.Command("systemctl", "--user", "stop", name).Run()    // nolint: gosec
		_ = exec.Command("systemctl", "--user", "disable", name).Run() // nolint: gosec
		if err := os.Remove(serviceFile); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove service file: %w", err)
		}
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run() // nolint: gosec
	}

	if runtime.GOOS == "darwin" {
		for _, label := range []string{launchdLabel, legacyLaunchdLabel} {
			plist := launchdPlist(homeDir, label)
			if _, err := os.Stat(plist); err != nil {
				continue
			}
			fmt.Printf("Stopping %s service...\n", label)
			_ = exec.Command("launchctl", "bootout", launchdTarget(label)).Run() // nolint: gosec
			if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("failed to remove launchd plist: %w", err)
			}
		}
		if err := os.RemoveAll(launchdLogDir(homeDir)); err != nil {
			return fmt.Errorf("failed to remove logs: %w", err)
		}
	}

	binDir, execDir := installDirs(homeDir)
	bashCompDir := filepath.Join(homeDir, ".local", "share", "bash-completion", "completions")
	zshCompDir := filepath.Join(homeDir, ".local", "share", "zsh", "site-functions")

	paths := []string{
		filepath.Join(binDir, cliName),
		filepath.Join(execDir, binaryName),
		filepath.Join(bashCompDir, cliName),
		filepath.Join(bashCompDir, binaryName),
		filepath.Join(zshCompDir, "_"+cliName),
		filepath.Join(zshCompDir, "_"+binaryName),
	}

	fmt.Println("Removing desk binaries and shell completions...")
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove %s: %w", p, err)
		}
	}
	// Links the installer may have added to Homebrew/system completion directories.
	for _, p := range []string{
		"/opt/homebrew/share/zsh/site-functions/_" + cliName,
		"/opt/homebrew/share/zsh/site-functions/_" + binaryName,
		"/usr/local/share/zsh/site-functions/_" + cliName,
		"/usr/local/share/zsh/site-functions/_" + binaryName,
		"/opt/homebrew/etc/bash_completion.d/" + cliName,
		"/opt/homebrew/etc/bash_completion.d/" + binaryName,
		"/usr/local/etc/bash_completion.d/" + cliName,
		"/usr/local/etc/bash_completion.d/" + binaryName,
	} {
		removeSymlink(p)
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

// removePathInstallerBlock strips PATH and completion blocks install.sh appends to shell rc files.
// It is a no-op if the file doesn't exist or doesn't contain any block.
func removePathInstallerBlock(rcFile string) error {
	data, err := os.ReadFile(rcFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	content := string(data)
	changed := false
	for _, block := range []string{pathInstallerBlock, zshCompletionBlock, bashCompletionBlock} {
		if strings.Contains(content, block) {
			content = strings.Replace(content, block, "", 1)
			changed = true
		}
	}
	if !changed {
		return nil
	}

	info, err := os.Stat(rcFile)
	if err != nil {
		return err
	}
	return os.WriteFile(rcFile, []byte(content), info.Mode()) // nolint: gosec
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

// installCompletionsForSelf installs the completion scripts generated by the running binary.
func installCompletionsForSelf() error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to find the desk binary: %w", err)
	}
	return installCompletions(self)
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
	bashScript := fixBashCompletionCommands(fixBashCompletionSpacing(fixBashCompletionWordBreaks(string(bashOut))))
	if err := os.WriteFile(filepath.Join(bashDir, "desk"), []byte(bashScript), 0644); err != nil { // nolint: gosec
		return fmt.Errorf("writing bash completion: %w", err)
	}
	if err := os.WriteFile(filepath.Join(bashDir, "deskconn"), []byte(bashScript), 0644); err != nil { // nolint: gosec
		return fmt.Errorf("writing bash completion: %w", err)
	}

	zshOut, err := exec.Command(deskBin, "--completion-script-zsh").Output() // nolint: gosec
	if err != nil {
		return fmt.Errorf("generating zsh completion: %w", err)
	}
	zshScript := fixZshCompletion(string(zshOut))
	if err := os.WriteFile(filepath.Join(zshDir, "_desk"), []byte(zshScript), 0644); err != nil { // nolint: gosec
		return fmt.Errorf("writing zsh completion: %w", err)
	}
	// _desk covers both desk and deskconn; a second file with the same #compdef line could
	// win at compinit and never call _desk, so remove one left by an earlier install.
	if err := os.Remove(filepath.Join(zshDir, "_deskconn")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing stale zsh completion: %w", err)
	}

	linkOptionalCompletions(zshDir, bashDir)
	return nil
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
