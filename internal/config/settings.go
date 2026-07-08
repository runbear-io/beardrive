package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultServer is used wherever a server is needed and none is configured:
// bare `bdrive login`, and `bdrive init` on a never-logged-in device.
const DefaultServer = "https://beardrive.ai"

// Settings are device-wide defaults, stored at $BDRIVE_HOME/settings.json.
type Settings struct {
	// Server is the default bdrive web server for `bdrive init`.
	Server string `json:"server,omitempty"`
	// Token authenticates this device to Server (minted by `bdrive login`).
	// Sent as a Bearer header.
	Token string `json:"token,omitempty"`
	// Email and Name identify the signed-in account; journal ops carry them
	// so history shows who changed what.
	Email string `json:"email,omitempty"`
	Name  string `json:"name,omitempty"`
}

func settingsPath() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "settings.json"), nil
}

// LoadSettings reads the device settings; a missing file is zero settings.
func LoadSettings() (Settings, error) {
	var s Settings
	path, err := settingsPath()
	if err != nil {
		return s, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("parse %s: %w", path, err)
	}
	return s, nil
}

// SaveSettings persists the device settings.
func SaveSettings(s Settings) error {
	path, err := settingsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeJSON(path, s)
}
