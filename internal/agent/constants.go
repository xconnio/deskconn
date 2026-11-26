package agent

const (
	// AgentTitleScanLines caps how many leading lines of a session file are inspected for a title.
	AgentTitleScanLines = 50

	// AgentMaxTitleLen truncates a session title for display.
	AgentMaxTitleLen = 70

	// AgentSessionTailBytes is how much of a session file's end is read for its recent
	// messages, title and cwd.
	AgentSessionTailBytes = 256 * 1024
	// AgentRecentMessages is how many of a session's last chat messages are listed.
	AgentRecentMessages = 6
	AgentMaxMessageLen  = 500

	// AgentRoleUser is both a session record's type and its message's role for a user's message.
	AgentRoleUser      = "user"
	agentRoleAssistant = "assistant"
)
