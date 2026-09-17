package orchestrator

// GuestEgressDir is where the guest learns its egress policy as data: a
// readable directory outside ReservedGuestDir, because /fuse is mode 0700
// and holds credentials, while the proxy endpoint is not a secret and a
// non-root browser harness has to be able to read it. the api refuses a
// caller file under it for the same reason it refuses one under /fuse.
const GuestEgressDir = "/etc/fuse"
