package daemon

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"inputd/internal/config"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// newTestDaemon creates a Daemon backed by a temp config file.
// Roles start with no StablePath so no real devices are opened.
// d.Stop() is registered as a cleanup.
func newTestDaemon(t *testing.T) *Daemon {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := &config.Config{
		Version: 1,
		Transport: config.TransportConfig{
			EventSocket:   filepath.Join(dir, "input.sock"),
			ControlSocket: filepath.Join(dir, "control.sock"),
		},
		Roles: map[string]config.RoleConfig{
			"primary_input":   {},
			"secondary_input": {},
		},
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	d := New(cfg, cfgPath, "")
	t.Cleanup(d.Stop)
	return d
}

func TestCloneConfig_independence(t *testing.T) {
	src := &config.Config{
		Roles: map[string]config.RoleConfig{
			"primary_input": {DeviceID: "kbd-a", StablePath: "/dev/input/a"},
		},
	}
	dst := cloneConfig(src)

	// Mutate the clone; original must be unchanged.
	dst.Roles["primary_input"] = config.RoleConfig{DeviceID: "kbd-b"}
	dst.Roles["secondary_input"] = config.RoleConfig{DeviceID: "kbd-c"}

	if src.Roles["primary_input"].DeviceID != "kbd-a" {
		t.Error("original Roles was modified by clone mutation")
	}
	if _, ok := src.Roles["secondary_input"]; ok {
		t.Error("original Roles gained a key from clone mutation")
	}
}

func TestAssignRole_uniqueness(t *testing.T) {
	d := newTestDaemon(t)

	if _, err := d.AssignRole("primary_input", "keyboard-1"); err != nil {
		t.Fatalf("first AssignRole: %v", err)
	}
	// Reassign same device to secondary_input — primary_input should be cleared.
	removed, err := d.AssignRole("secondary_input", "keyboard-1")
	if err != nil {
		t.Fatalf("second AssignRole: %v", err)
	}
	if removed != "primary_input" {
		t.Errorf("removed = %q, want primary_input", removed)
	}

	d.mu.Lock()
	gcID := d.cfgVal.Roles["primary_input"].DeviceID
	piID := d.cfgVal.Roles["secondary_input"].DeviceID
	d.mu.Unlock()

	if gcID != "" {
		t.Errorf("primary_input.DeviceID = %q after move, want empty", gcID)
	}
	if piID != "keyboard-1" {
		t.Errorf("secondary_input.DeviceID = %q, want keyboard-1", piID)
	}
}

func TestAssignRole_persistedToFile(t *testing.T) {
	d := newTestDaemon(t)

	if _, err := d.AssignRole("primary_input", "keyboard-1"); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}

	loaded, err := config.Load(d.cfgPath)
	if err != nil {
		t.Fatalf("Load after AssignRole: %v", err)
	}
	if loaded.Roles["primary_input"].DeviceID != "keyboard-1" {
		t.Errorf("persisted DeviceID = %q, want keyboard-1", loaded.Roles["primary_input"].DeviceID)
	}
}

func TestClearRole(t *testing.T) {
	d := newTestDaemon(t)

	if _, err := d.AssignRole("primary_input", "keyboard-1"); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	if err := d.ClearRole("primary_input"); err != nil {
		t.Fatalf("ClearRole: %v", err)
	}

	d.mu.Lock()
	rc := d.cfgVal.Roles["primary_input"]
	d.mu.Unlock()

	if rc.DeviceID != "" || rc.StablePath != "" {
		t.Errorf("after ClearRole: DeviceID=%q StablePath=%q, want both empty",
			rc.DeviceID, rc.StablePath)
	}
}
