//go:build windows

package main

import "golang.org/x/sys/windows"

func detachConsole() {
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("FreeConsole")
	_, _, _ = proc.Call()
}
