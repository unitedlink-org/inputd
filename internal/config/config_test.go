package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"inputd/internal/config"
)

func TestSaveLoad_roundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	cfg := config.Default()
	cfg.Roles["primary_input"] = config.RoleConfig{
		StablePath: "/dev/input/test_keyboard",
		DeviceID:   "test-kbd",
		Grab:       true,
	}

	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	rc := got.Roles["primary_input"]
	if rc.StablePath != "/dev/input/test_keyboard" {
		t.Errorf("StablePath = %q, want /dev/input/test_keyboard", rc.StablePath)
	}
	if rc.DeviceID != "test-kbd" {
		t.Errorf("DeviceID = %q, want test-kbd", rc.DeviceID)
	}
	if !rc.Grab {
		t.Error("Grab should be true after roundtrip")
	}
	if got.Transport.EventSocket != cfg.Transport.EventSocket {
		t.Errorf("EventSocket = %q, want %q", got.Transport.EventSocket, cfg.Transport.EventSocket)
	}
}

func TestLoad_missingFile(t *testing.T) {
	_, err := config.Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected errors.Is(err, os.ErrNotExist), got: %v", err)
	}
}

func TestDefault_structure(t *testing.T) {
	cfg := config.Default()
	for _, role := range []string{"primary_input", "secondary_input"} {
		if _, ok := cfg.Roles[role]; !ok {
			t.Errorf("Default() missing role %q", role)
		}
	}
	if cfg.Transport.EventSocket == "" {
		t.Error("Default() EventSocket is empty")
	}
	if cfg.Transport.ControlSocket == "" {
		t.Error("Default() ControlSocket is empty")
	}
}

func TestSave_noTempFileLeft(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := config.Save(path, config.Default()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("leftover tmp file after Save: %s", e.Name())
		}
	}
}
