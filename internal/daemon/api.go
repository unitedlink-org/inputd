package daemon

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"inputd/internal/evdev"
)

func (d *Daemon) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/status", d.handleStatus)
	mux.HandleFunc("GET /v1/health", d.handleHealth)
	mux.HandleFunc("GET /v1/devices", d.handleDevices)
	mux.HandleFunc("GET /v1/roles", d.handleRoles)
	mux.HandleFunc("POST /v1/roles/{role}/assign", d.handleAssign)
	mux.HandleFunc("DELETE /v1/roles/{role}", d.handleClearRole)
	mux.HandleFunc("POST /v1/learn/{role}/start", d.handleLearnStart)
	mux.HandleFunc("POST /v1/learn/stop", d.handleLearnStop)
	mux.HandleFunc("POST /v1/config/reload", d.handleReload)
	mux.HandleFunc("GET /v1/events", d.handleEvents)
	mux.HandleFunc("POST /v1/inject", d.handleInject)
	mux.HandleFunc("GET /v1/autodiscover", d.handleAutoDiscover)
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg}) //nolint:errcheck
}

func (d *Daemon) handleHealth(w http.ResponseWriter, _ *http.Request) {
	cfg := d.cfg()
	type roleHealth struct {
		Online bool `json:"online"`
	}
	roles := make(map[string]roleHealth, len(cfg.Roles))
	allOnline := true
	for role := range cfg.Roles {
		online := d.readerOnline(role)
		roles[role] = roleHealth{Online: online}
		if !online {
			allOnline = false
		}
	}
	if !allOnline {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	jsonOK(w, map[string]any{"ok": allOnline, "roles": roles})
}

func (d *Daemon) handleStatus(w http.ResponseWriter, _ *http.Request) {
	cfg := d.cfg()
	jsonOK(w, map[string]any{
		"ok":             true,
		"version":        "1",
		"event_socket":   cfg.Transport.EventSocket,
		"control_socket": cfg.Transport.ControlSocket,
	})
}

func (d *Daemon) handleDevices(w http.ResponseWriter, _ *http.Request) {
	jsonOK(w, map[string]any{"devices": d.ScanDevices()})
}

func (d *Daemon) handleRoles(w http.ResponseWriter, _ *http.Request) {
	cfg := d.cfg()
	type roleStatus struct {
		Role       string `json:"role"`
		DeviceID   string `json:"device_id"`
		StablePath string `json:"stable_path"`
		Grab       bool   `json:"grab"`
		Online     bool   `json:"online"`
	}
	var roles []roleStatus
	for role, rc := range cfg.Roles {
		roles = append(roles, roleStatus{
			Role:       role,
			DeviceID:   rc.DeviceID,
			StablePath: rc.StablePath,
			Grab:       rc.Grab,
			Online:     d.readerOnline(role),
		})
	}
	jsonOK(w, map[string]any{"roles": roles})
}

func (d *Daemon) handleAssign(w http.ResponseWriter, r *http.Request) {
	role := r.PathValue("role")
	var req struct {
		DeviceID string `json:"device_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DeviceID == "" {
		jsonErr(w, http.StatusBadRequest, "device_id required")
		return
	}
	removed, err := d.AssignRole(role, req.DeviceID)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	slog.Info("role.assigned", "role", role, "device_id", req.DeviceID, "removed_from", removed)
	jsonOK(w, map[string]any{
		"ok":           true,
		"role":         role,
		"device_id":    req.DeviceID,
		"removed_from": removed,
	})
}

func (d *Daemon) handleClearRole(w http.ResponseWriter, r *http.Request) {
	role := r.PathValue("role")
	if err := d.ClearRole(role); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	slog.Info("role.unassigned", "role", role)
	jsonOK(w, map[string]any{"ok": true, "role": role})
}

func (d *Daemon) handleLearnStart(w http.ResponseWriter, r *http.Request) {
	role := r.PathValue("role")
	if err := d.StartLearn(role); err != nil {
		jsonErr(w, http.StatusConflict, err.Error())
		return
	}
	jsonOK(w, map[string]any{
		"ok":          true,
		"role":        role,
		"timeout_sec": int(learnTimeout.Seconds()),
	})
}

func (d *Daemon) handleLearnStop(w http.ResponseWriter, _ *http.Request) {
	d.StopLearn()
	jsonOK(w, map[string]any{"ok": true})
}

func (d *Daemon) handleReload(w http.ResponseWriter, _ *http.Request) {
	if err := d.Reload(); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{"ok": true})
}

// probeDevice opens the device at path and returns its info.
func probeDevice(path, deviceID string) DeviceInfo {
	di := DeviceInfo{DeviceID: deviceID, Path: path}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		di.Path = real
	}
	dev, err := evdev.Open(path)
	if err != nil {
		return di
	}
	defer dev.Close()
	di.Online = true
	di.Name = dev.Name
	di.Phys = dev.Phys
	di.Uniq = dev.Uniq
	di.VendorID = hex4(dev.ID.Vendor)
	di.ProductID = hex4(dev.ID.Product)
	return di
}

func hex4(v uint16) string {
	const h = "0123456789abcdef"
	return string([]byte{h[v>>12], h[(v>>8)&0xf], h[(v>>4)&0xf], h[v&0xf]})
}

func (d *Daemon) handleInject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Role  string `json:"role"`
		Code  uint16 `json:"code"`
		Value int32  `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Role == "" {
		jsonErr(w, http.StatusBadRequest, "role required")
		return
	}
	we := WireEvent{
		Kind:      "key",
		TS:        time.Now().UnixMilli(),
		Role:      req.Role,
		DeviceID:  "remote",
		EventType: "EV_KEY",
		Code:      req.Code,
		CodeName:  evdev.KeyCodeName(req.Code),
		Value:     req.Value,
	}
	select {
	case d.eventCh <- encode(we):
	default:
		jsonErr(w, http.StatusServiceUnavailable, "event channel full")
		return
	}
	slog.Info("inject.key", "role", req.Role, "code", req.Code, "value", req.Value)
	jsonOK(w, map[string]any{"ok": true, "event": we})
}

func (d *Daemon) handleAutoDiscover(w http.ResponseWriter, _ *http.Request) {
	type autoDevice struct {
		Path   string `json:"path"`
		Name   string `json:"name"`
		Role   string `json:"role"`
		Online bool   `json:"online"`
	}
	d.mu.Lock()
	devices := make([]autoDevice, 0, len(d.autoReaders))
	for path, r := range d.autoReaders {
		devices = append(devices, autoDevice{
			Path:   path,
			Name:   r.cfg.DeviceID,
			Role:   r.role,
			Online: r.isOnline(),
		})
	}
	d.mu.Unlock()
	jsonOK(w, map[string]any{"auto_discovered": devices})
}

func (d *Daemon) handleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Disable the server's WriteTimeout for this long-lived streaming response.
	http.NewResponseController(w).SetWriteDeadline(time.Time{}) //nolint:errcheck

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	ch := d.bcast.subscribeSSE()
	defer d.bcast.unsubscribeSSE(ch)

	for {
		select {
		case <-r.Context().Done():
			return
		case data := <-ch:
			w.Write([]byte("data: "))
			w.Write(data) // data already contains a trailing newline
			w.Write([]byte("\n"))
			flusher.Flush()
		}
	}
}
