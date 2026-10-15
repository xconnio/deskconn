package deskconn

// RootdName is the name the deskconn binary runs as its root helper under: main
// dispatches on it either as argv[0] (the installed symlink) or as the first argument
// (how LaunchVPNHelper invokes it). Needed on every platform, unlike the rest of the root
// helper machinery (see launch.go), since main's multicall dispatch checks it unconditionally.
const RootdName = "deskconn-rootd"
