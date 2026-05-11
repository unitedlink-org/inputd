//go:build !linux

package daemon

import "log/slog"

func (d *Daemon) runUdevWatcher() {
	defer d.wg.Done()
	slog.Warn("udev auto-discovery is only supported on Linux")
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
	}
}
