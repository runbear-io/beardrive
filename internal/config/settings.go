package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Settings are device-wide defaults, stored at $BDRIVE_HOME/settings.json.
type Settings struct {
	// Server is the default bdrive web server for `bdrive init`.
	Server string `json:"server,omitempty"`
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
