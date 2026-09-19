package protocol

const (
	// DAPSessionEventName identifies the adapter event that lets clients bind
	// richer transports to the exact managed session created by DAP.
	DAPSessionEventName    = "bingo/session/v1"
	DAPSessionEventVersion = 1

	// DAPSourceLaunchVersion distinguishes servers that build local Go packages
	// from older adapters that silently ignore the launch mode argument.
	DAPSourceLaunchVersion = 1
)
