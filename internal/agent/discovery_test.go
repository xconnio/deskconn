package agent_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/agent"
)

const sessionABC = "abc123"

func TestSessionsWithIDPrefix(t *testing.T) {
	sessions := []agent.AgentSessionFile{{ID: sessionABC}, {ID: "abd456"}, {ID: "xyz789"}}
	ids := func(matches []agent.AgentSessionFile) []string {
		var out []string
		for _, s := range matches {
			out = append(out, s.ID)
		}
		return out
	}
	require.Equal(t, []string{sessionABC}, ids(agent.SessionsWithIDPrefix(sessions, "abc")))
	require.Equal(t, []string{sessionABC, "abd456"}, ids(agent.SessionsWithIDPrefix(sessions, "ab")), "both, ambiguous")
	require.Empty(t, agent.SessionsWithIDPrefix(sessions, "nope"))
	require.Len(t, agent.SessionsWithIDPrefix(sessions, ""), 3, "an empty prefix matches all")
}
