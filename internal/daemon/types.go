package daemon

// WireEvent is the JSON structure emitted on the event socket and used for
// status notifications. kind values: "key", "device_connected",
// "device_disconnected", "role_changed", "health".
type WireEvent struct {
	Kind       string  `json:"kind"`
	TS         int64   `json:"ts"`
	Role       string  `json:"role,omitempty"`
	DeviceID   string  `json:"device_id,omitempty"`
	DevicePath string  `json:"device_path,omitempty"`
	DeviceName string  `json:"device_name,omitempty"`
	EventType  string  `json:"event_type,omitempty"`
	Code       uint16  `json:"code,omitempty"`
	CodeName   string  `json:"code_name,omitempty"`
	Value      int32   `json:"value,omitempty"`
}

// DeviceInfo is returned by the /v1/devices API.
type DeviceInfo struct {
	DeviceID  string `json:"device_id"`
	Path      string `json:"path"`
	Name      string `json:"name"`
	Phys      string `json:"phys"`
	Uniq      string `json:"uniq"`
	VendorID  string `json:"vendor_id"`
	ProductID string `json:"product_id"`
	Online    bool   `json:"online"`
}
