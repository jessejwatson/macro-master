//go:build !darwin || !cgo

package hotkey

// Supported reports whether global hotkeys work on this system.
const Supported = false

// DaemonArg is the command-line argument that runs the hotkey daemon.
const DaemonArg = "__hotkeyd"

// Run runs f; there's no event loop to run here.
func Run(f func()) {
	f()
	select {}
}

// Pressed never delivers anything here.
func Pressed() <-chan uint32 { return nil }

// Register always fails here.
func Register(uint32, Combo) error { return ErrUnsupported }

// Unregister does nothing here.
func Unregister(uint32) {}
