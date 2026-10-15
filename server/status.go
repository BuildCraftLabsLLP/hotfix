package main

// status.json: the running service's state, written every sample and readable
// by any user, so `hotfixd status` needs no elevation and no IPC.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

type statusHot struct {
	PID    int     `json:"pid"`
	Name   string  `json:"name"`
	CPU    float64 `json:"cpu"`
	HotFor float64 `json:"hot_for_seconds"`
}

type Status struct {
	Version        string         `json:"version"`
	PID            int            `json:"pid"`
	Started        time.Time      `json:"started"`
	Updated        time.Time      `json:"updated"`
	Mode           string         `json:"mode"`            // as applied (monitor if unlicensed)
	ConfiguredMode string         `json:"configured_mode"` // as set in config.json
	License        licenseSummary `json:"license"`
	CPUThreshold   float64        `json:"cpu_threshold"`
	KillDuration   float64        `json:"kill_duration"`
	Processes      int            `json:"processes_seen"`
	Hot            []statusHot    `json:"hot"`
	Events         []Event        `json:"recent_events"`
	Warning        string         `json:"warning,omitempty"`
}

func statusPath() string { return filepath.Join(stateDir(), "status.json") }

func writeStatus(st Status) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	path := statusPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readStatus() (Status, error) {
	var st Status
	data, err := os.ReadFile(statusPath())
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(data, &st)
}
