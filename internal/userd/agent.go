package userd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn-core/bindings/go"
)

// agentInstalled reports whether the agent tool (claude, codex) is installed: whether the
// user's own shell finds it, as it does when a terminal runs it (see userCommand). Checked
// once, when userd starts.
// ponytail: installing or removing an agent takes a userd restart to show.
var agentInstalled = userHasCommand //nolint:gochecknoglobals // tests replace it

// agentApps are the apps of the agents installed on this machine: each lists and pulls its
// sessions for the web app and `desk agent`, and resumes them in a terminal.
func (d *Deskconn) agentApps() []*app {
	agents := []struct{ tool, name, list, pull, projects string }{
		{common.ClaudeTool, "Claude", common.ProcedureClaudeSessionList, common.ProcedureClaudeSessionPull,
			common.ProcedureClaudeProjectList},
		{common.CodexTool, "Codex", common.ProcedureCodexSessionList, common.ProcedureCodexSessionPull,
			common.ProcedureCodexProjectList},
	}
	var apps []*app
	for _, a := range agents {
		if !agentInstalled(a.tool) {
			continue
		}
		apps = append(apps, &app{ID: a.tool, Name: a.name, Requires: []string{terminalAppID},
			Procedures: map[string]xconn.InvocationHandler{
				a.list:     d.handleAgentSessionList(a.tool),
				a.pull:     d.handleAgentSessionPull(a.tool),
				a.projects: d.handleAgentProjectList(a.tool),
			}})
	}
	return apps
}

func (d *Deskconn) handleAgentSessionList(tool string) xconn.InvocationHandler {
	return func(_ context.Context, inv *xconn.Invocation) *xconn.Result {
		return d.agentSessionList(tool, inv)
	}
}

func (d *Deskconn) agentSessionList(tool string, inv *xconn.Invocation) *xconn.Result {
	enc, errResult := d.callerKeys(inv)
	if errResult != nil {
		return errResult
	}

	var args common.AgentSessionListArgs
	if errResult = decryptArgs(enc, inv, &args); errResult != nil {
		return errResult
	}
	if args.Offset < 0 || args.Limit < 0 {
		return xconn.NewResultError(common.ErrInvalidArgument, "offset and limit can't be negative")
	}

	sessions, err := agentLocalSessions(tool, args.Path)
	if err != nil {
		return xconn.NewResultError(common.ErrOperationFailed, err.Error())
	}

	// Only the page's sessions are summarized: that reads each one's file.
	sessions = sessions[min(args.Offset, len(sessions)):]
	if args.Limit > 0 {
		sessions = sessions[:min(args.Limit, len(sessions))]
	}
	return encryptResult(enc, common.SummarizeAgentSessions(sessions))
}

func (d *Deskconn) handleAgentSessionPull(tool string) xconn.InvocationHandler {
	return func(_ context.Context, inv *xconn.Invocation) *xconn.Result {
		return d.agentSessionPull(tool, inv)
	}
}

func (d *Deskconn) agentSessionPull(tool string, inv *xconn.Invocation) *xconn.Result {
	enc, errResult := d.callerKeys(inv)
	if errResult != nil {
		return errResult
	}

	var args common.AgentSessionPullArgs
	if errResult = decryptArgs(enc, inv, &args); errResult != nil {
		return errResult
	}

	sessions, err := agentLocalSessions(tool, args.Path)
	if err != nil {
		return xconn.NewResultError(common.ErrOperationFailed, err.Error())
	}

	if args.SessionID != "" {
		var matches []common.AgentSessionFile
		for _, s := range sessions {
			if strings.HasPrefix(s.ID, args.SessionID) {
				matches = append(matches, s)
			}
		}
		switch {
		case len(matches) == 0:
			return xconn.NewResultError(common.ErrOperationFailed, fmt.Sprintf("no session matching %q", args.SessionID))
		case len(matches) > 1:
			return xconn.NewResultError(common.ErrOperationFailed,
				fmt.Sprintf("%q matches more than one session; use a longer prefix", args.SessionID))
		}
		sessions = matches
	}

	byTool := make(map[string][]common.AgentSessionFile)
	for _, s := range sessions {
		if args.Tool != "" && s.Tool != args.Tool {
			continue
		}
		byTool[s.Tool] = append(byTool[s.Tool], s)
	}
	if len(byTool) == 0 {
		return xconn.NewResultError(common.ErrOperationFailed, "no local sessions found on this machine for this project")
	}

	var bundles []common.AgentSessionBundle
	for tool, files := range byTool {
		tarball, err := BuildAgentTarball(files)
		if err != nil {
			return xconn.NewResultError(common.ErrOperationFailed, err.Error())
		}
		bundles = append(bundles, common.AgentSessionBundle{Tool: tool, Tarball: tarball})
	}

	return encryptResult(enc, bundles)
}

func (d *Deskconn) handleAgentProjectList(tool string) xconn.InvocationHandler {
	return func(_ context.Context, inv *xconn.Invocation) *xconn.Result {
		return d.agentProjectList(tool, inv)
	}
}

func (d *Deskconn) agentProjectList(tool string, inv *xconn.Invocation) *xconn.Result {
	enc, errResult := d.callerKeys(inv)
	if errResult != nil {
		return errResult
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return xconn.NewResultError(common.ErrOperationFailed, err.Error())
	}
	projects, err := common.DiscoverAgentProjects(tool, homeDir)
	if err != nil {
		return xconn.NewResultError(common.ErrOperationFailed, err.Error())
	}
	return encryptResult(enc, projects)
}

// agentLocalSessions discovers this machine's current tool sessions for the project at path:
// relative to home, as `desk agent` sends it to match the project across machines, or
// absolute, as the web app sends a directory from the agent's project list.
func agentLocalSessions(tool, path string) ([]common.AgentSessionFile, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get home directory: %w", err)
	}
	return common.DiscoverAgentSessions(tool, homeDir, path)
}
