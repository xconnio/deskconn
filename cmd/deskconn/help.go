package main

import (
	"text/template"

	"github.com/alecthomas/kingpin/v2"
)

// Keep overviews short; expanded help and man pages use Kingpin's full templates.
func configureHelp(app *kingpin.Application) {
	app.UsageFuncs(template.FuncMap{
		"CommandRows":   commandHelpRows,
		"GroupExamples": groupHelpExamples,
		"ShellExamples": shellHelpExamples,
		"CommandFlags":  commandHelpFlags,
		"GlobalFlags": func(flags []*kingpin.FlagModel, global []*kingpin.FlagModel) []*kingpin.FlagModel {
			return filterHelpFlags(flags, global, true)
		},
	}).UsageTemplate(`{{if .Context.SelectedCommand -}}
{{$command := .Context.SelectedCommand -}}
{{if $command.Commands -}}
Usage: {{.App.Name}} {{$command.FullCommand}} <command>

{{$command.Help}}

Commands:
{{CommandRows $command.Commands | FormatTwoColumns}}
{{with GroupExamples $command.Name -}}
Examples:
{{range .}}  {{.}}
{{end}}
{{end -}}
Run 'desk {{$command.FullCommand}} <command> --help' for details.
Run 'desk --help-long' for all commands and flags.
{{else -}}
Usage: {{FormatCommandUsage .App $command}}

{{$command.Help}}

{{with .Context.Args -}}
Args:
{{. | ArgsToTwoColumns | FormatTwoColumns}}
{{end -}}
{{with CommandFlags .Context.Flags .App.Flags -}}
Flags:
{{. | FlagsToTwoColumns | FormatTwoColumns}}
{{end -}}
{{if eq $command.FullCommand "shell" -}}
Examples:
{{range ShellExamples}}  {{.}}
{{end}}
{{end -}}
{{with GlobalFlags .Context.Flags .App.Flags -}}
Global flags:
{{. | FlagsToTwoColumns | FormatTwoColumns}}
{{end -}}
{{end -}}
{{else -}}
Usage: {{.App.Name}} [flags] <command>

{{.App.Help}}

Commands:
{{CommandRows .App.Commands "top" "ls" "shell" "file" "port" "vpn" "agent" | FormatTwoColumns}}
Management:
{{CommandRows .App.Commands "machine" "app" "account" "self" "serve" | FormatTwoColumns}}
Standalone:
{{CommandRows .App.Commands "profile" "keygen" | FormatTwoColumns}}
Flags:
  --help            Show help
  --profile NAME    Connect directly to a standalone machine saved as a profile

Run 'desk <command> --help' for details.
Run 'desk --help-long' for all commands and flags.

Examples:
  desk ls
  desk shell office
  desk file cp ./notes.txt office:/tmp/notes.txt
{{end -}}
`)
}

func shellHelpExamples() []string {
	if standaloneRequested() {
		return []string{"desk shell", "desk shell -- uname -a", "desk shell -A"}
	}
	return []string{"desk shell office", "desk shell office -- uname -a", "desk shell -A office"}
}

func commandHelpRows(commands []*kingpin.CmdModel, names ...string) [][2]string {
	if len(names) == 0 {
		for _, command := range commands {
			names = append(names, command.Name)
		}
	}
	rows := make([][2]string, 0, len(names))
	for _, name := range names {
		for _, command := range commands {
			if command.Name == name && !command.Hidden {
				rows = append(rows, [2]string{command.Name, command.Help})
				break
			}
		}
	}
	return rows
}

func commandHelpFlags(flags, global []*kingpin.FlagModel) []*kingpin.FlagModel {
	return filterHelpFlags(flags, global, false)
}

func filterHelpFlags(flags, global []*kingpin.FlagModel, wantGlobal bool) []*kingpin.FlagModel {
	globalNames := make(map[string]bool, len(global))
	for _, flag := range global {
		globalNames[flag.Name] = true
	}
	var result []*kingpin.FlagModel
	for _, flag := range flags {
		if !flag.Hidden && globalNames[flag.Name] == wantGlobal {
			result = append(result, flag)
		}
	}
	return result
}

func groupHelpExamples(group string) []string {
	switch group {
	case "file":
		if standaloneRequested() {
			return []string{"desk file ls :/tmp", "desk file cp ./notes.txt :/tmp/notes.txt", "desk file edit :/etc/hosts"}
		}
		return []string{
			"desk file ls office:/tmp",
			"desk file cp ./notes.txt office:/tmp/notes.txt",
			"desk file edit office:/etc/hosts",
		}
	case "machine":
		return []string{"desk machine ls", "desk machine info office", "desk machine alias office work"}
	case "account":
		return []string{"desk account login", "desk account whoami", "desk account logout"}
	case "app":
		if standaloneRequested() {
			return []string{"desk app ls", "desk app register ./remote-control", "desk app disable remote-control"}
		}
		return []string{"desk app ls office", "desk app register office ./remote-control",
			"desk app disable office remote-control"}
	case "port":
		if standaloneRequested() {
			return []string{"desk port forward 8080:80", "desk port reverse 9000:3000"}
		}
		return []string{"desk port forward office 8080:80", "desk port reverse office 9000:3000"}
	case "vpn":
		if standaloneRequested() {
			return []string{"desk vpn connect", "desk vpn serve", "desk vpn stop"}
		}
		return []string{"desk vpn connect office", "desk vpn serve", "desk vpn stop"}
	case "agent":
		if standaloneRequested() {
			return []string{"desk agent ls", "desk agent pull"}
		}
		return []string{"desk agent ls", "desk agent pull office", "desk agent resume <session-id>"}
	case "profile":
		return []string{
			"desk profile add office --url quic://host:18080 --cert-hash <fingerprint> --private-key <key>",
			"desk --profile office shell",
			"desk profile ls",
		}
	case "self":
		return []string{"desk self version", "desk self update"}
	case "serve":
		return []string{
			"desk serve",
			"desk serve --mode standalone --user alice:s3cret",
			"desk serve init",
			"desk serve --config deskconn.yaml",
		}
	default:
		return nil
	}
}
