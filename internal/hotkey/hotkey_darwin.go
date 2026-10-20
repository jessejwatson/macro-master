//go:build darwin && cgo

package hotkey

/*
#cgo LDFLAGS: -framework Cocoa -framework Carbon
void mm_run(void);
int mm_register(unsigned int id, unsigned int code, unsigned int mods, void **ref);
void mm_unregister(void *ref);
*/
import "C"

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"unsafe"
)

// Supported reports whether global hotkeys work on this system.
const Supported = true

// DaemonArg is the command-line argument that runs the hotkey daemon. That
// process keeps its main goroutine on the main thread, which macOS's event
// loop must run on.
const DaemonArg = "__hotkeyd"

func init() {
	if len(os.Args) > 1 && os.Args[1] == DaemonArg {
		runtime.LockOSThread()
	}
}

// Carbon modifier masks.
const (
	carbonCmd   = 1 << 8
	carbonShift = 1 << 9
	carbonAlt   = 1 << 11
	carbonCtrl  = 1 << 12
)

var (
	mu      sync.Mutex
	refs    = map[uint32]unsafe.Pointer{}
	pressed = make(chan uint32, 16)
)

//export mmHotkeyPressed
func mmHotkeyPressed(id C.uint) {
	select {
	case pressed <- uint32(id):
	default: // Presses pile up only if the daemon is stuck; drop them.
	}
}

// Run starts f on a new goroutine and then runs the event loop that
// hotkeys arrive on. It must be called from the main goroutine of the
// daemon process (see DaemonArg), and never returns.
func Run(f func()) {
	go f()
	C.mm_run()
	panic("hotkey event loop stopped")
}

// Pressed delivers the id of each hotkey pressed.
func Pressed() <-chan uint32 { return pressed }

// Register makes c a global hotkey that sends id on Pressed. It must not
// be called from the main goroutine, and waits for Run to have started.
func Register(id uint32, c Combo) error {
	var mods C.uint
	if c.Mods&Ctrl != 0 {
		mods |= carbonCtrl
	}
	if c.Mods&Alt != 0 {
		mods |= carbonAlt
	}
	if c.Mods&Shift != 0 {
		mods |= carbonShift
	}
	if c.Mods&Cmd != 0 {
		mods |= carbonCmd
	}
	var ref unsafe.Pointer
	status := C.mm_register(C.uint(id), C.uint(c.Code()), mods, &ref)
	switch status {
	case 0:
	case -9878: // eventHotKeyExistsErr
		return fmt.Errorf("%s is already taken by another app", c.Symbols())
	case -9879: // eventHotKeyInvalidErr
		return fmt.Errorf("macOS won't accept %s as a hotkey", c.Symbols())
	default:
		return fmt.Errorf("macOS refused %s (error %d)", c.Symbols(), int(status))
	}
	mu.Lock()
	refs[id] = ref
	mu.Unlock()
	return nil
}

// Unregister removes the hotkey registered as id.
func Unregister(id uint32) {
	mu.Lock()
	ref, ok := refs[id]
	delete(refs, id)
	mu.Unlock()
	if ok {
		C.mm_unregister(ref)
	}
}
