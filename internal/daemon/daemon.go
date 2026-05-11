package daemon

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"inputd/internal/config"
)

// Daemon orchestrates device readers, event broadcast, and the control API.
type Daemon struct {
	cfgPath string
	webAddr string

	mu          sync.Mutex
	cfgVal      *config.Config
	readers     map[string]*roleReader // role → static reader
	autoReaders map[string]*roleReader // devPath → dynamic reader (udev hotplug)

	eventCh chan []byte
	bcast   *broadcaster

	learnMu sync.Mutex
	learn   *learnSession

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func New(cfg *config.Config, cfgPath, webAddr string) *Daemon {
	ctx, cancel := context.WithCancel(context.Background())
	return &Daemon{
		cfgPath:     cfgPath,
		webAddr:     webAddr,
		cfgVal:      cfg,
		readers:     make(map[string]*roleReader),
		autoReaders: make(map[string]*roleReader),
		eventCh:     make(chan []byte, 128),
		bcast:       newBroadcaster(),
		ctx:         ctx,
		cancel:      cancel,
	}
}

// cfg returns a snapshot of the current config (caller must not modify).
func (d *Daemon) cfg() *config.Config {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cfgVal
}

// Start launches all background goroutines.
func (d *Daemon) Start() error {
	if err := os.MkdirAll("/run/inputd", 0755); err != nil {
		return err
	}

	d.wg.Add(1)
	go d.runBroadcaster()

	d.wg.Add(1)
	go d.runEventSocket()

	d.wg.Add(1)
	go d.runControlAPI()

	d.mu.Lock()
	cfg := d.cfgVal
	d.mu.Unlock()

	hasAutoDiscover := false
	for role, rc := range cfg.Roles {
		if rc.StablePath != "" {
			d.startReader(role, rc)
		}
		if rc.AutoDiscover {
			hasAutoDiscover = true
		}
	}

	if hasAutoDiscover {
		d.wg.Add(1)
		go d.runUdevWatcher()
	}

	d.wg.Add(1)
	go d.runHealth()

	if d.webAddr != "" {
		d.wg.Add(1)
		go d.runWebUI()
	}

	slog.Info("inputd started", "config", d.cfgPath)
	sdNotify("READY=1")
	return nil
}

// Stop gracefully shuts down the daemon.
func (d *Daemon) Stop() {
	slog.Info("inputd stopping")
	d.StopLearn()

	// Close all open devices before cancelling ctx so blocking Read() calls
	// unblock immediately instead of waiting for the next key press.
	d.mu.Lock()
	roles := make([]string, 0, len(d.readers))
	for role := range d.readers {
		roles = append(roles, role)
	}
	autoPaths := make([]string, 0, len(d.autoReaders))
	for p := range d.autoReaders {
		autoPaths = append(autoPaths, p)
	}
	d.mu.Unlock()

	for _, role := range roles {
		d.stopReader(role)
	}
	for _, p := range autoPaths {
		d.handleUdevRemove(p)
	}

	d.cancel()
	d.wg.Wait()
}

// Reload re-reads the config file and applies it.
func (d *Daemon) Reload() error {
	newCfg, err := config.Load(d.cfgPath)
	if err != nil {
		return err
	}
	return d.applyConfig(newCfg)
}

// applyConfig diffs newCfg against the current config, stops/starts affected
// readers, then atomically updates d.cfgVal.
// Callers must NOT have modified d.cfgVal before calling this.
func (d *Daemon) applyConfig(newCfg *config.Config) error {
	d.mu.Lock()
	old := d.cfgVal
	d.mu.Unlock()

	// stop readers whose role changed or was removed
	for role, oldRC := range old.Roles {
		newRC, exists := newCfg.Roles[role]
		if !exists || newRC.StablePath != oldRC.StablePath {
			d.stopReader(role)
		}
	}

	d.mu.Lock()
	d.cfgVal = newCfg
	d.mu.Unlock()

	// start readers for new or changed roles
	for role, newRC := range newCfg.Roles {
		if newRC.StablePath == "" {
			continue
		}
		oldRC, existed := old.Roles[role]
		if !existed || oldRC.StablePath != newRC.StablePath {
			d.startReader(role, newRC)
		}
	}

	slog.Info("config.loaded", "path", d.cfgPath)
	return nil
}

// cloneConfig returns a deep copy of cfg with an independent Roles map.
func cloneConfig(src *config.Config) *config.Config {
	dst := *src
	dst.Roles = make(map[string]config.RoleConfig, len(src.Roles))
	for k, v := range src.Roles {
		dst.Roles[k] = v
	}
	return &dst
}

// AssignRole binds deviceID to role (enforces uniqueness), saves and applies.
func (d *Daemon) AssignRole(role, deviceID string) (removedFrom string, err error) {
	d.mu.Lock()
	newCfg := cloneConfig(d.cfgVal)
	d.mu.Unlock()

	// resolve stable path: existing role binding
	stablePath := ""
	for _, rc := range newCfg.Roles {
		if rc.DeviceID == deviceID && rc.StablePath != "" {
			stablePath = rc.StablePath
			break
		}
	}
	if stablePath == "" {
		stablePath = deviceID
	}

	// enforce uniqueness
	for r, rc := range newCfg.Roles {
		if rc.DeviceID == deviceID && r != role {
			removedFrom = r
			rc.DeviceID = ""
			rc.StablePath = ""
			newCfg.Roles[r] = rc
		}
	}

	rc := newCfg.Roles[role]
	rc.DeviceID = deviceID
	rc.StablePath = stablePath
	newCfg.Roles[role] = rc

	if err = config.Save(d.cfgPath, newCfg); err != nil {
		return
	}
	err = d.applyConfig(newCfg)
	return
}

// ClearRole removes a role binding, saves and applies.
func (d *Daemon) ClearRole(role string) error {
	d.mu.Lock()
	newCfg := cloneConfig(d.cfgVal)
	d.mu.Unlock()

	rc := newCfg.Roles[role]
	rc.StablePath = ""
	rc.DeviceID = ""
	newCfg.Roles[role] = rc

	if err := config.Save(d.cfgPath, newCfg); err != nil {
		return err
	}
	return d.applyConfig(newCfg)
}

// startReader starts a role reader goroutine.
func (d *Daemon) startReader(role string, rc config.RoleConfig) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.readers[role]; exists {
		return
	}
	r := newRoleReader(role, rc, d.eventCh, d.ctx, d)
	d.readers[role] = r
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		r.run()
	}()
}

// stopReader stops the role reader goroutine and removes it from the map.
func (d *Daemon) stopReader(role string) {
	d.mu.Lock()
	r, exists := d.readers[role]
	delete(d.readers, role)
	d.mu.Unlock()
	if exists {
		r.stop()
	}
}

func (d *Daemon) runHealth() {
	defer d.wg.Done()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-t.C:
			d.eventCh <- encode(WireEvent{
				Kind: "health",
				TS:   time.Now().UnixMilli(),
			})
			sdNotify("WATCHDOG=1")
		}
	}
}

// readerOnline reports whether the role has at least one device open (static or auto-discovered).
func (d *Daemon) readerOnline(role string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r, ok := d.readers[role]; ok && r.isOnline() {
		return true
	}
	for _, r := range d.autoReaders {
		if r.role == role && r.isOnline() {
			return true
		}
	}
	return false
}

// ScanDevices returns info about all keyboard devices currently visible.
func (d *Daemon) ScanDevices() []DeviceInfo {
	d.mu.Lock()
	cfg := d.cfgVal
	d.mu.Unlock()

	seen := make(map[string]bool)
	var result []DeviceInfo
	for _, rc := range cfg.Roles {
		if rc.StablePath == "" || seen[rc.StablePath] {
			continue
		}
		seen[rc.StablePath] = true
		result = append(result, probeDevice(rc.StablePath, rc.DeviceID))
	}
	return result
}

// runControlAPI starts the HTTP control API over a Unix socket.
func (d *Daemon) runControlAPI() {
	defer d.wg.Done()
	path := d.cfg().Transport.ControlSocket
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		slog.Error("control socket listen failed", "path", path, "err", err)
		return
	}
	defer ln.Close()
	if err := os.Chmod(path, 0666); err != nil {
		slog.Warn("control socket chmod failed", "path", path, "err", err)
	}
	slog.Info("control socket listening", "path", path)

	go func() {
		<-d.ctx.Done()
		ln.Close()
	}()

	mux := http.NewServeMux()
	d.registerRoutes(mux)
	srv := &http.Server{Handler: mux}
	if err := srv.Serve(ln); err != nil && d.ctx.Err() == nil {
		slog.Error("control api error", "err", err)
	}
}

// sdNotify sends a systemd sd_notify message if NOTIFY_SOCKET is set.
func sdNotify(msg string) {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return
	}
	if strings.HasPrefix(sock, "@") {
		sock = "\x00" + sock[1:] // abstract socket
	}
	conn, err := net.Dial("unixgram", sock)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.Write([]byte(msg)) //nolint:errcheck
}
