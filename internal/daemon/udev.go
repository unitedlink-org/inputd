//go:build linux

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"inputd/internal/config"
	"inputd/internal/evdev"
)

type udevEvent struct {
	action  string // "add" or "remove"
	devPath string // "/dev/input/eventX"
}

// runUdevWatcher monitors kernel uevents and auto-assigns new keyboards to
// roles with auto_discover: true.
func (d *Daemon) runUdevWatcher() {
	defer d.wg.Done()

	ch, err := watchKernelUevents(d.ctx)
	if err != nil {
		slog.Error("udev watcher failed to start", "err", err)
		return
	}

	// Handle devices that were already present before we started listening.
	d.autoDiscoverExisting()

	for {
		select {
		case <-d.ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			switch ev.action {
			case "add":
				d.wg.Add(1)
				go func(p string) {
					defer d.wg.Done()
					d.tryAutoDiscover(p, true)
				}(ev.devPath)
			case "remove":
				d.handleUdevRemove(ev.devPath)
			}
		}
	}
}

func (d *Daemon) autoDiscoverExisting() {
	matches, _ := filepath.Glob("/dev/input/event*")
	for _, devPath := range matches {
		d.tryAutoDiscover(devPath, false)
	}
}

// tryAutoDiscover adds devPath to the auto-discover role if it is an unclaimed keyboard.
// withDelay adds a brief pause so udev symlink rules can settle before we check claims.
func (d *Daemon) tryAutoDiscover(devPath string, withDelay bool) {
	if withDelay {
		time.Sleep(200 * time.Millisecond)
	}

	// Check keyboard capability before taking the lock.
	dev, err := evdev.Open(devPath)
	if err != nil {
		return
	}
	isKeyboard := dev.HasKeyboardKeys()
	devName := dev.Name
	devPhys := dev.Phys
	dev.Close()

	if !isKeyboard {
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// Already tracked as an auto-reader.
	if _, exists := d.autoReaders[devPath]; exists {
		return
	}

	cfg := d.cfgVal

	// Skip if this device (or another HID node on the same USB device) is
	// already claimed by any configured role — including the auto-discover role
	// itself so sub-devices of the already-bound secondary keyboard are excluded.
	for _, rc := range cfg.Roles {
		if rc.StablePath == "" {
			continue
		}
		if symlinkResolves(rc.StablePath, devPath) {
			return
		}
		// Same physical USB device (e.g. mouse/consumer-control sub-interface).
		if devPhys != "" {
			if sp := devicePhysFromSysfs(rc.StablePath); sp != "" && physPrefix(sp) == physPrefix(devPhys) {
				return
			}
		}
	}

	// Find the auto-discover role.
	var autoRole string
	var autoRC config.RoleConfig
	for role, rc := range cfg.Roles {
		if rc.AutoDiscover {
			autoRole = role
			autoRC = rc
			break
		}
	}
	if autoRole == "" {
		return
	}

	rc := config.RoleConfig{
		StablePath: devPath,
		DeviceID:   devName, // friendly name for events and UI
		Grab:       autoRC.Grab,
	}
	r := newRoleReader(autoRole, rc, d.eventCh, d.ctx, d)
	d.autoReaders[devPath] = r

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		r.run()
	}()

	slog.Info("keyboard auto-discovered", "path", devPath, "name", devName, "role", autoRole)
}

// devicePhysFromSysfs reads the physical location string from sysfs for devPath
// (which may be a udev symlink like /dev/input/primary_keypad).
func devicePhysFromSysfs(devPath string) string {
	resolved, err := filepath.EvalSymlinks(devPath)
	if err != nil {
		return ""
	}
	data, err := os.ReadFile("/sys/class/input/" + filepath.Base(resolved) + "/device/phys")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// physPrefix returns the USB device prefix from a phys string.
// "usb-0000:00:14.0-2/input1" → "usb-0000:00:14.0-2"
func physPrefix(phys string) string {
	if idx := strings.Index(phys, "/"); idx >= 0 {
		return phys[:idx]
	}
	return phys
}

func (d *Daemon) handleUdevRemove(devPath string) {
	d.mu.Lock()
	r, exists := d.autoReaders[devPath]
	if exists {
		delete(d.autoReaders, devPath)
	}
	d.mu.Unlock()

	if exists {
		r.stop()
		slog.Info("auto-reader removed", "path", devPath)
	}
}

// symlinkResolves reports whether stablePath (a possible symlink) resolves to devPath.
func symlinkResolves(stablePath, devPath string) bool {
	resolved, err := filepath.EvalSymlinks(stablePath)
	if err != nil {
		return false
	}
	devResolved, err := filepath.EvalSymlinks(devPath)
	if err != nil {
		return false
	}
	return resolved == devResolved
}

// watchKernelUevents opens a NETLINK_KOBJECT_UEVENT socket and returns a channel
// of input/eventX add and remove events. The channel is closed when ctx is done.
func watchKernelUevents(ctx context.Context) (<-chan udevEvent, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_KOBJECT_UEVENT)
	if err != nil {
		return nil, fmt.Errorf("netlink socket: %w", err)
	}

	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{
		Family: syscall.AF_NETLINK,
		Groups: 1, // kernel uevents
	}); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("netlink bind: %w", err)
	}

	ch := make(chan udevEvent, 32)

	var closeOnce sync.Once
	closeFd := func() { closeOnce.Do(func() { syscall.Close(fd) }) }

	// Close fd when ctx is done to unblock the blocking Read below.
	go func() {
		<-ctx.Done()
		closeFd()
	}()

	go func() {
		defer close(ch)
		defer closeFd()
		buf := make([]byte, 8192)
		for {
			n, err := syscall.Read(fd, buf)
			if err != nil || n == 0 {
				return
			}
			ev, ok := parseUevent(buf[:n])
			if !ok {
				continue
			}
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch, nil
}

// parseUevent parses a raw kernel uevent message.
// Format: "action@devpath\x00KEY=VALUE\x00..."
func parseUevent(data []byte) (udevEvent, bool) {
	parts := strings.SplitN(string(data), "\x00", -1)
	if len(parts) < 2 {
		return udevEvent{}, false
	}

	var action, subsystem, devName string
	for _, kv := range parts[1:] {
		if v, ok := strings.CutPrefix(kv, "ACTION="); ok {
			action = v
		} else if v, ok := strings.CutPrefix(kv, "SUBSYSTEM="); ok {
			subsystem = v
		} else if v, ok := strings.CutPrefix(kv, "DEVNAME="); ok {
			devName = v
		}
	}

	if subsystem != "input" || !strings.HasPrefix(devName, "input/event") {
		return udevEvent{}, false
	}
	if action != "add" && action != "remove" {
		return udevEvent{}, false
	}

	return udevEvent{action: action, devPath: "/dev/" + devName}, true
}
