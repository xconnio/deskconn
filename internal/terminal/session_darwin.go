package terminal

// killSession is left to the process group on macOS, which has no /proc to find the rest of
// the session from: jobs an interactive shell put in groups of their own get the hangup the
// shell passes on, but not the kill.
func killSession(int) {}
