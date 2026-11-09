package cgroup

import "fmt"

// Mode says whether the server needs cgroups.
type Mode string

const (
	// Off never uses cgroups: limits are setrlimit plus the container's cap.
	Off Mode = "off"
	// Auto uses cgroups when the probe passes, and otherwise runs as Off and says
	// why in one warning at start.
	Auto Mode = "auto"
	// Required refuses to start unless the probe passes. It never falls back.
	Required Mode = "required"
)

// ParseMode parses CGROUP_MODE; "" is Off.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case "", Off:
		return Off, nil
	case Auto, Required:
		return Mode(s), nil
	}
	return "", fmt.Errorf("CGROUP_MODE must be off, auto or required, got %q", s)
}
