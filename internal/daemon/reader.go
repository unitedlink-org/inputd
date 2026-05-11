package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"inputd/internal/config"
	"inputd/internal/evdev"
)

// roleReader manages the evdev read loop for one role.
// It reconnects automatically when the device disappears.
type roleReader struct {
	role    string
	cfg     config.RoleConfig
	eventCh chan<- []byte
	ctx     context.Context
	cancel  context.CancelFunc

	// guards the currently open device so stop() can close it to unblock Read()
	devMu sync.Mutex
	dev   *evdev.Device

	daemon *Daemon
}

func newRoleReader(role string, cfg config.RoleConfig, eventCh chan<- []byte, parentCtx context.Context, d *Daemon) *roleReader {
	ctx, cancel := context.WithCancel(parentCtx)
	return &roleReader{
		role:    role,
		cfg:     cfg,
		eventCh: eventCh,
		ctx:     ctx,
		cancel:  cancel,
		daemon:  d,
	}
}

// isOnline reports whether the reader currently has a device open and reading.
func (r *roleReader) isOnline() bool {
	r.devMu.Lock()
	defer r.devMu.Unlock()
	return r.dev != nil
}

// stop cancels the reader and closes the open device to unblock any blocking Read().
func (r *roleReader) stop() {
	r.cancel()
	r.devMu.Lock()
	if r.dev != nil {
		r.dev.Close()
	}
	r.devMu.Unlock()
}

// run is the top-level goroutine; it loops until ctx is cancelled.
func (r *roleReader) run() {
	for {
		if err := r.loop(); err != nil {
			slog.Warn("reader.error", "role", r.role, "path", r.cfg.StablePath, "err", err)
			r.emit(statusEvent("device_disconnected", r.role, r.cfg.DeviceID, r.cfg.StablePath, ""))
		}
		select {
		case <-r.ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// loop opens the device, grabs if configured, reads events until error or cancel.
func (r *roleReader) loop() error {
	dev, err := evdev.Open(r.cfg.StablePath)
	if err != nil {
		return err
	}

	r.devMu.Lock()
	r.dev = dev
	r.devMu.Unlock()
	defer func() {
		r.devMu.Lock()
		if r.dev == dev {
			r.dev = nil
		}
		r.devMu.Unlock()
		dev.Close()
	}()

	// cancelled while opening
	select {
	case <-r.ctx.Done():
		return nil
	default:
	}

	if r.cfg.Grab {
		if err := dev.Grab(true); err != nil {
			slog.Warn("grab failed", "role", r.role, "err", err)
		} else {
			defer dev.Grab(false) //nolint:errcheck
		}
	}

	slog.Info("device.connected", "role", r.role, "path", r.cfg.StablePath, "name", dev.Name)
	r.emit(statusEvent("device_connected", r.role, r.cfg.DeviceID, r.cfg.StablePath, dev.Name))

	for {
		ev, err := dev.Read()
		if err != nil {
			return err
		}
		select {
		case <-r.ctx.Done():
			return nil
		default:
		}
		if ev.Type != evdev.EvKey {
			continue
		}

		// notify learn mode on any key-down
		if ev.Value == 1 {
			r.daemon.notifyLearn(r.cfg.StablePath)
		}

		we := WireEvent{
			Kind:       "key",
			TS:         int64(ev.TimeSec)*1000 + int64(ev.TimeUsec)/1000,
			Role:       r.role,
			DeviceID:   r.cfg.DeviceID,
			DevicePath: r.cfg.StablePath,
			DeviceName: dev.Name,
			EventType:  "EV_KEY",
			Code:       ev.Code,
			CodeName:   evdev.KeyCodeName(ev.Code),
			Value:      ev.Value,
		}
		r.emit(encode(we))
	}
}

func (r *roleReader) emit(data []byte) {
	select {
	case r.eventCh <- data:
	default:
		slog.Warn("event channel full, dropping event", "role", r.role)
	}
}

// statusEvent builds a JSON line for device_connected / device_disconnected.
func statusEvent(kind, role, deviceID, path, name string) []byte {
	we := WireEvent{
		Kind:       kind,
		TS:         time.Now().UnixMilli(),
		Role:       role,
		DeviceID:   deviceID,
		DevicePath: path,
		DeviceName: name,
	}
	return encode(we)
}

func encode(v any) []byte {
	b, _ := json.Marshal(v)
	return append(b, '\n')
}
