// Package evdev provides raw Linux evdev device access without CGo.
package evdev

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// InputEvent matches struct input_event from linux/input.h (x86-64: 24 bytes).
type InputEvent struct {
	TimeSec  int64
	TimeUsec int64
	Type     uint16
	Code     uint16
	Value    int32
}

// InputID matches struct input_id from linux/input.h.
type InputID struct {
	BusType uint16
	Vendor  uint16
	Product uint16
	Version uint16
}

const EvKey = 1

// ioctl request codes for 64-bit Linux.
// EVIOCGNAME(256), EVIOCGPHYS(256), EVIOCGUNIQ(256), EVIOCGID, EVIOCGRAB, EVIOCGBIT(0,4).
const (
	ioctlGNAME256 = uintptr(0x81004506)
	ioctlGPHYS256 = uintptr(0x81004507)
	ioctlGUNIQ256 = uintptr(0x81004508)
	ioctlGID      = uintptr(0x80084502)
	ioctlGRAB     = uintptr(0x40044590)
	ioctlGBIT04   = uintptr(0x80044520) // EVIOCGBIT(EV_SYN=0, 4 bytes) — reads EV type bitmap
	ioctlGKEY04   = uintptr(0x80044521) // EVIOCGBIT(EV_KEY=1, 4 bytes) — reads key codes 0-31
)

// Device wraps an open evdev device file.
type Device struct {
	f    *os.File
	Path string
	Name string
	Phys string
	Uniq string
	ID   InputID
}

// Open opens an evdev device and reads its metadata.
func Open(path string) (*Device, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	d := &Device{f: f, Path: path}
	fd := f.Fd()
	buf := make([]byte, 256)
	d.Name = ioctlStr(fd, ioctlGNAME256, buf)
	d.Phys = ioctlStr(fd, ioctlGPHYS256, buf)
	d.Uniq = ioctlStr(fd, ioctlGUNIQ256, buf)
	syscall.Syscall(syscall.SYS_IOCTL, fd, ioctlGID, uintptr(unsafe.Pointer(&d.ID)))
	return d, nil
}

func ioctlStr(fd uintptr, req uintptr, buf []byte) string {
	for i := range buf {
		buf[i] = 0
	}
	syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(unsafe.Pointer(&buf[0])))
	if n := strings.IndexByte(string(buf), 0); n >= 0 {
		return string(buf[:n])
	}
	return string(buf)
}

// HasEvKey returns true if the device declares EV_KEY capability.
func (d *Device) HasEvKey() bool {
	var bits [4]byte
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, d.f.Fd(), ioctlGBIT04, uintptr(unsafe.Pointer(&bits[0])))
	return errno == 0 && bits[0]&(1<<EvKey) != 0
}

// HasKeyboardKeys returns true if the device has KEY_A (code 30), which is
// present on real keyboards but absent on mice, power buttons, and media devices.
func (d *Device) HasKeyboardKeys() bool {
	var bits [4]byte
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, d.f.Fd(), ioctlGKEY04, uintptr(unsafe.Pointer(&bits[0])))
	// KEY_A = 30 → byte index 3 (30/8), bit index 6 (30%8)
	return errno == 0 && bits[3]&(1<<6) != 0
}

// Grab acquires (on=true) or releases exclusive access to the device.
func (d *Device) Grab(on bool) error {
	v := uintptr(0)
	if on {
		v = 1
	}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, d.f.Fd(), ioctlGRAB, v)
	if errno != 0 {
		return fmt.Errorf("EVIOCGRAB: %w", errno)
	}
	return nil
}

// Read reads one input event, blocking until one is available.
func (d *Device) Read() (InputEvent, error) {
	var ev InputEvent
	err := binary.Read(d.f, binary.LittleEndian, &ev)
	return ev, err
}

// Close closes the device file.
func (d *Device) Close() { d.f.Close() }
