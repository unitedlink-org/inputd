package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"inputd/internal/config"
	"inputd/internal/evdev"
)

const learnTimeout = 15 * time.Second

type learnSession struct {
	role   string
	result chan string // receives the winning stable path
	cancel context.CancelFunc
}

// notifyLearn is called by role readers and learn scanners on key-down.
func (d *Daemon) notifyLearn(devicePath string) {
	d.learnMu.Lock()
	ls := d.learn
	d.learnMu.Unlock()
	if ls == nil {
		return
	}
	select {
	case ls.result <- devicePath:
	default:
	}
}

// StartLearn activates learn mode for the given role.
func (d *Daemon) StartLearn(role string) error {
	d.learnMu.Lock()
	defer d.learnMu.Unlock()
	if d.learn != nil {
		return fmt.Errorf("learn mode already active for role %q", d.learn.role)
	}
	ctx, cancel := context.WithTimeout(d.ctx, learnTimeout)
	ls := &learnSession{
		role:   role,
		result: make(chan string, 1),
		cancel: cancel,
	}
	d.learn = ls
	go d.runLearn(ctx, ls)
	slog.Info("learn.started", "role", role, "timeout_sec", int(learnTimeout.Seconds()))
	return nil
}

// StopLearn cancels any active learn session.
func (d *Daemon) StopLearn() {
	d.learnMu.Lock()
	ls := d.learn
	d.learn = nil
	d.learnMu.Unlock()
	if ls != nil {
		ls.cancel()
		slog.Info("learn.stopped")
	}
}

func (d *Daemon) runLearn(ctx context.Context, ls *learnSession) {
	defer func() {
		ls.cancel()
		d.learnMu.Lock()
		if d.learn == ls {
			d.learn = nil
		}
		d.learnMu.Unlock()
	}()

	// Open all keyboard devices not currently held by a role reader.
	alreadyOpen := d.openedDevicePaths()
	var tempDevices []*evdev.Device

	entries, _ := os.ReadDir("/dev/input")
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "event") {
			continue
		}
		path := "/dev/input/" + e.Name()
		real, _ := filepath.EvalSymlinks(path)
		if alreadyOpen[path] || alreadyOpen[real] {
			continue
		}
		dev, err := evdev.Open(path)
		if err != nil || !dev.HasEvKey() {
			if dev != nil {
				dev.Close()
			}
			continue
		}
		tempDevices = append(tempDevices, dev)
		go func(dev *evdev.Device) {
			defer dev.Close()
			for {
				ev, err := dev.Read()
				if err != nil {
					return
				}
				select {
				case <-ctx.Done():
					return
				default:
				}
				if ev.Type == evdev.EvKey && ev.Value == 1 {
					d.notifyLearn(resolveStablePath(dev.Path))
					return
				}
			}
		}(dev)
	}
	defer func() {
		for _, dev := range tempDevices {
			dev.Close()
		}
	}()

	// Drain residual key events for 300 ms so we don't capture the UI click
	// or keyboard shortcut the operator used to start learn mode.
	drainTimer := time.NewTimer(300 * time.Millisecond)
	for draining := true; draining; {
		select {
		case <-ctx.Done():
			drainTimer.Stop()
			return
		case <-drainTimer.C:
			draining = false
		case <-ls.result:
			// discard residual
		}
	}

	select {
	case <-ctx.Done():
		slog.Warn("learn.timeout", "role", ls.role)
	case winner := <-ls.result:
		slog.Info("learn.completed", "role", ls.role, "path", winner)
		if err := d.bindLearnResult(ls.role, winner); err != nil {
			slog.Error("learn bind failed", "err", err)
		}
	}
}

// bindLearnResult assigns the winning stable path to the role, saves and applies.
func (d *Daemon) bindLearnResult(role, stablePath string) error {
	deviceID := filepath.Base(stablePath)

	d.mu.Lock()
	newCfg := cloneConfig(d.cfgVal)
	d.mu.Unlock()

	// clear this device from any other role
	for r, rc := range newCfg.Roles {
		if r != role && rc.StablePath == stablePath {
			rc.StablePath = ""
			rc.DeviceID = ""
			newCfg.Roles[r] = rc
			slog.Info("role.moved", "from", r, "to", role, "device_id", deviceID)
		}
	}

	rc := newCfg.Roles[role]
	rc.StablePath = stablePath
	rc.DeviceID = deviceID
	newCfg.Roles[role] = rc

	if err := config.Save(d.cfgPath, newCfg); err != nil {
		return err
	}
	if err := d.applyConfig(newCfg); err != nil {
		return err
	}

	d.eventCh <- encode(WireEvent{
		Kind:     "role_changed",
		TS:       time.Now().UnixMilli(),
		Role:     role,
		DeviceID: deviceID,
	})
	return nil
}

// openedDevicePaths returns real paths of devices currently open by any reader
// (static or auto-discovered). Learn mode skips these to avoid double-listening.
func (d *Daemon) openedDevicePaths() map[string]bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	result := make(map[string]bool, len(d.readers)+len(d.autoReaders))
	for _, r := range d.readers {
		addDevPath(result, r.cfg.StablePath)
	}
	for _, r := range d.autoReaders {
		addDevPath(result, r.cfg.StablePath)
	}
	return result
}

func addDevPath(m map[string]bool, path string) {
	if path == "" {
		return
	}
	m[path] = true
	if real, err := filepath.EvalSymlinks(path); err == nil {
		m[real] = true
	}
}

// resolveStablePath maps an eventX path to its best stable symlink.
func resolveStablePath(eventPath string) string {
	real, err := filepath.EvalSymlinks(eventPath)
	if err != nil {
		real = eventPath
	}

	// search /dev/input/ for named symlinks (primary_keypad, secondary_keypad…)
	if entries, err := os.ReadDir("/dev/input"); err == nil {
		for _, e := range entries {
			if e.Type()&os.ModeSymlink == 0 {
				continue
			}
			link := "/dev/input/" + e.Name()
			if t, err := filepath.EvalSymlinks(link); err == nil && t == real {
				return link
			}
		}
	}

	// fall back to by-id (prefer -event-kbd)
	best := ""
	if entries, err := os.ReadDir("/dev/input/by-id"); err == nil {
		for _, e := range entries {
			link := "/dev/input/by-id/" + e.Name()
			if t, err := filepath.EvalSymlinks(link); err == nil && t == real {
				if best == "" || strings.HasSuffix(e.Name(), "-event-kbd") {
					best = link
				}
			}
		}
	}
	if best != "" {
		return best
	}
	return real
}

