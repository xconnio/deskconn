package info

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	ole "github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
)

// startMenuDirs are where Windows keeps .lnk shortcuts for installed apps - the closest
// equivalent to Linux's .desktop files, and the same source Start Menu search itself reads.
func startMenuDirs() []string {
	var dirs []string
	if pd := os.Getenv("ProgramData"); pd != "" {
		dirs = append(dirs, filepath.Join(pd, "Microsoft", "Windows", "Start Menu", "Programs"))
	}
	if ad := os.Getenv("AppData"); ad != "" {
		dirs = append(dirs, filepath.Join(ad, "Microsoft", "Windows", "Start Menu", "Programs"))
	}
	return dirs
}

// scanDesktopFiles walks the Start Menu for .lnk shortcuts and resolves each to its target exe
// via the WScript.Shell COM object's CreateShortcut, the standard way to read .lnk files without
// hand-parsing the binary shell-link format.
func scanDesktopFiles() map[string]desktopEntry {
	result := make(map[string]desktopEntry)

	// COM apartment state is per-OS-thread; this goroutine must stay pinned to one thread
	// for the whole CoInitialize/.../CoUninitialize span, or the calls below can fail or
	// corrupt state if the Go runtime moves the goroutine mid-call.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := ole.CoInitialize(0); err != nil {
		return result
	}
	defer ole.CoUninitialize()

	unknown, err := oleutil.CreateObject("WScript.Shell")
	if err != nil {
		return result
	}
	defer unknown.Release()
	wshell, err := unknown.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return result
	}
	defer wshell.Release()

	for _, dir := range startMenuDirs() {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.EqualFold(filepath.Ext(path), ".lnk") {
				return nil //nolint:nilerr
			}
			entry, ok := parseShortcut(wshell, path)
			if !ok {
				return nil
			}
			key := strings.ToLower(entry.ExecBase)
			if _, exists := result[key]; !exists {
				result[key] = entry
			}
			return nil
		})
	}

	return result
}

// parseShortcut resolves one .lnk file to a desktopEntry, keyed later by its target's exe
// basename same as buildApps does for .desktop files' Exec field.
func parseShortcut(wshell *ole.IDispatch, path string) (desktopEntry, bool) {
	shortcutVariant, err := oleutil.CallMethod(wshell, "CreateShortcut", path)
	if err != nil {
		return desktopEntry{}, false
	}
	shortcut := shortcutVariant.ToIDispatch()
	defer shortcut.Release()

	targetVariant, err := oleutil.GetProperty(shortcut, "TargetPath")
	if err != nil {
		return desktopEntry{}, false
	}
	target := targetVariant.ToString()
	if target == "" || !strings.EqualFold(filepath.Ext(target), ".exe") {
		return desktopEntry{}, false
	}

	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	// IconName holds a full path here rather than an icon-theme name (Windows has no icon
	// theme system); the target exe itself is what a native icon extractor would need.
	return desktopEntry{ID: name, Name: name, IconName: target, ExecBase: filepath.Base(target)}, true
}
