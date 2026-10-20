package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"macro-master/internal/hotkey"
	"macro-master/internal/store"
)

const (
	// hotkeyPoll is how often the daemon checks config.json and its own
	// binary for changes.
	hotkeyPoll = time.Second
	// hotkeyDebounce drops a second press of the same keys this soon after
	// the first, so a held or bouncing key starts one job.
	hotkeyDebounce = 500 * time.Millisecond
	// hotkeyLogMax is when the daemon's log is emptied as it starts.
	hotkeyLogMax = 1 << 20
	// exitRestart asks launchd to start the daemon again, after mm was
	// upgraded under it.
	exitRestart = 75
)

// cmdHotkeyd is mm __hotkeyd, the background process that registers key
// bindings as global hotkeys and runs mm __bind <keys> when one is
// pressed. launchd starts it (see mm bind service); it follows changes to
// config.json by itself.
func (a *App) cmdHotkeyd() error {
	if !hotkey.Supported {
		return hotkey.ErrUnsupported
	}
	home, err := a.homeDir()
	if err != nil {
		return err
	}
	st := &store.Store{Home: home}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}
	if fi, err := os.Stat(st.HotkeyLogPath()); err == nil && fi.Size() > hotkeyLogMax {
		os.Truncate(st.HotkeyLogPath(), 0)
	}
	log.SetFlags(log.LstdFlags)
	log.SetPrefix("mm hotkeys: ")

	// One daemon per home: a second one exits quietly, and launchd leaves
	// it be.
	lock, err := os.OpenFile(filepath.Join(home, "hotkeys.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		log.Printf("another hotkey daemon is running for %s; exiting", home)
		return nil
	}

	exe := os.Args[0]
	if !filepath.IsAbs(exe) {
		if exe, err = os.Executable(); err != nil {
			return err
		}
	}
	d := &hotkeyDaemon{
		configPath: filepath.Join(home, "config.json"),
		statusPath: st.HotkeyStatusPath(),
		exe:        exe,
		register:   hotkey.Register,
		unregister: hotkey.Unregister,
		fire: func(keys string) {
			cmd := exec.Command(exe, "__bind", keys)
			cmd.Dir = os.Getenv("HOME")
			cmd.Env = childEnv()
			cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := cmd.Start(); err != nil {
				log.Printf("can't run %s: %v", keys, err)
				return
			}
			go cmd.Wait()
		},
	}
	hotkey.Run(func() { os.Exit(d.loop()) })
	return nil
}

// hotkeyDaemon keeps the registered hotkeys in step with config.json.
type hotkeyDaemon struct {
	configPath, statusPath, exe string

	register   func(uint32, hotkey.Combo) error
	unregister func(uint32)
	fire       func(keys string)

	ids      map[string]uint32 // registered keys and their hotkey ids
	keys     map[uint32]string
	status   map[string]string // each binding's keys: "" or why it failed
	nextID   uint32
	config   stamp
	lastFire map[string]time.Time
}

// stamp identifies a version of a file, to notice it changing.
type stamp struct {
	size  int64
	mtime time.Time
	ino   uint64
}

func stampOf(path string) stamp {
	fi, err := os.Stat(path)
	if err != nil {
		return stamp{}
	}
	s := stamp{size: fi.Size(), mtime: fi.ModTime()}
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		s.ino = uint64(sys.Ino)
	}
	return s
}

// loop runs the daemon until it's stopped or mm is replaced, and returns
// the exit code.
func (d *hotkeyDaemon) loop() int {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	exeStamp := stampOf(d.exe)
	log.Printf("started (pid %d, %s)", os.Getpid(), d.exe)
	d.reload()
	tick := time.NewTicker(hotkeyPoll)
	defer tick.Stop()
	for {
		select {
		case id := <-hotkey.Pressed():
			d.pressed(id, time.Now())
		case <-tick.C:
			if s := stampOf(d.exe); s != exeStamp {
				log.Printf("%s changed; restarting", d.exe)
				os.Remove(d.statusPath)
				return exitRestart
			}
			if stampOf(d.configPath) != d.config {
				d.reload()
			}
		case s := <-stop:
			log.Printf("stopping on %v", s)
			os.Remove(d.statusPath)
			return 0
		}
	}
}

// reload reads the bindings from config.json, registers new ones,
// unregisters removed ones and writes the status file. An unreadable
// config keeps the hotkeys as they are.
func (d *hotkeyDaemon) reload() {
	if d.ids == nil {
		d.ids, d.keys, d.status, d.lastFire = map[string]uint32{}, map[uint32]string{}, map[string]string{}, map[string]time.Time{}
	}
	d.config = stampOf(d.configPath)
	var bindings map[string]store.Binding
	data, err := os.ReadFile(d.configPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		log.Printf("can't read %s: %v", d.configPath, err)
		return
	default:
		c, err := store.ParseConfig(data)
		if err != nil {
			log.Printf("can't use %s, keeping the hotkeys as they were: %v", d.configPath, err)
			return
		}
		bindings = c.Bindings
	}

	for keys, id := range d.ids {
		if _, ok := bindings[keys]; !ok {
			d.unregister(id)
			delete(d.ids, keys)
			delete(d.keys, id)
			log.Printf("removed %s", keys)
		}
	}
	status := map[string]string{}
	for _, keys := range slices.Sorted(maps.Keys(bindings)) {
		if _, ok := d.ids[keys]; ok {
			status[keys] = ""
			continue
		}
		c, err := hotkey.Parse(keys)
		if err == nil && c.String() != keys {
			err = fmt.Errorf("write it as %s", c)
		}
		if err == nil {
			d.nextID++
			if err = d.register(d.nextID, c); err == nil {
				d.ids[keys], d.keys[d.nextID] = d.nextID, keys
				log.Printf("registered %s for %s", keys, bindings[keys].Macro)
			}
		}
		if err != nil {
			log.Printf("can't register %s: %v", keys, err)
			status[keys] = err.Error()
			continue
		}
		status[keys] = ""
	}
	d.status = status
	d.writeStatus()
}

func (d *hotkeyDaemon) writeStatus() {
	data, _ := json.MarshalIndent(hotkeyStatus{PID: os.Getpid(), Exe: d.exe, Keys: d.status}, "", "  ")
	if err := store.WriteFileAtomic(d.statusPath, append(data, '\n'), 0o644); err != nil {
		log.Printf("can't write %s: %v", d.statusPath, err)
	}
}

// pressed runs the binding for hotkey id, unless it just ran.
func (d *hotkeyDaemon) pressed(id uint32, now time.Time) {
	keys, ok := d.keys[id]
	if !ok {
		return
	}
	if last, ok := d.lastFire[keys]; ok && now.Sub(last) < hotkeyDebounce {
		return
	}
	d.lastFire[keys] = now
	log.Printf("%s pressed", keys)
	d.fire(keys)
}
