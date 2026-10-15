// Command hotfixd is Hotfix Server: a headless service for Linux, Windows and
// macOS that watches every process's CPU use and kills (or, in monitor mode,
// reports) processes that stay above a threshold.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const usage = `Hotfix Server — kills runaway processes before they take a server down.

Usage: hotfixd <command>

Service:
  install              Install and start the hotfixd service (root/administrator)
  uninstall            Stop and remove the service (keeps the config)
  start | stop | restart
  status               Show what the service is doing

License:
  license status       Show the license (trial days left, or licensed)
  license activate <key>
                       Use a seat of your license (HFXS-… key) on this server
  license deactivate   Free this server's seat
  license refresh      Check the license with the server now
  license reset-identity
                       Give a cloned machine its own identity

Settings:
  config show          Print the settings
  config set <key> <value>
                       Change a setting, e.g. "config set mode enforce"
                       (lists take comma-separated values)
  config path          Print the config file's location

Tools:
  check                Measure every process now and show what Hotfix would do
  run                  Run in the foreground (what the service runs)
  version              Print the version

Settings keys: mode (monitor|enforce), cpu_threshold, kill_duration,
term_grace_seconds, whitelist, protect_users, only_users, skip_containers,
webhook_url, https_proxy, license_key_file.
`

func main() {
	if err := runCLI(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "hotfixd:", err)
		os.Exit(1)
	}
}

var errNeedElevation = errors.New("this command needs root (sudo) or an administrator prompt")

func needElevation() error {
	if !isElevated() {
		return errNeedElevation
	}
	return nil
}

func runCLI(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stdout, usage)
		return nil
	}
	switch cmd, rest := args[0], args[1:]; cmd {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	case "version", "--version", "-v":
		if licenseEnforced() {
			fmt.Fprintln(stdout, "hotfixd", version)
		} else {
			fmt.Fprintln(stdout, "hotfixd", version, "(unlicensed development build)")
		}
		return nil
	case "license":
		return licenseCommand(rest, stdout)
	case "run":
		return runForeground()
	case "check":
		cfg, err := readConfig(configPath())
		if err != nil {
			return err
		}
		return runCheck(stdout, cfg, 2*time.Second, 20)
	case "status":
		return printStatus(stdout)
	case "config":
		return configCommand(rest, stdout)
	case "install", "uninstall", "start", "stop", "restart":
		return serviceCommand(cmd)
	default:
		return fmt.Errorf("unknown command %q (run `hotfixd help`)", cmd)
	}
}

// runForeground runs the monitor until interrupted. Under a service manager
// the service layer calls runService instead (see service.go).
func runForeground() error {
	if isServiceManaged() {
		return runService()
	}
	if err := initLog(logFilePath()); err != nil && isElevated() {
		fmt.Fprintln(os.Stderr, "hotfixd: log file:", err)
	}
	defer closeLog()
	e, err := newEngine(configPath())
	if err != nil {
		return err
	}
	go e.Run()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, append([]os.Signal{os.Interrupt, syscall.SIGTERM}, reloadSignals()...)...)
	for s := range sigs {
		if isReloadSignal(s) {
			e.Reload()
			continue
		}
		break
	}
	e.Stop()
	return nil
}

func logFilePath() string { return logDir() + string(os.PathSeparator) + "hotfixd.log" }

func configCommand(args []string, stdout io.Writer) error {
	sub := "show"
	if len(args) > 0 {
		sub = args[0]
	}
	path := configPath()
	switch sub {
	case "path":
		fmt.Fprintln(stdout, path)
		return nil
	case "show":
		cfg, err := readConfig(path)
		if err != nil {
			return err
		}
		data, _ := json.MarshalIndent(cfg, "", "  ")
		fmt.Fprintln(stdout, string(data))
		return nil
	case "set":
		if len(args) != 3 {
			return errors.New(`usage: hotfixd config set <key> <value>`)
		}
		if err := needElevation(); err != nil {
			return err
		}
		cfg, err := readConfig(path)
		if err != nil {
			return err
		}
		key := strings.ToLower(args[1])
		if err := setConfigValue(&cfg, key, args[2]); err != nil {
			return err
		}
		if err := writeConfig(path, cfg); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Saved %s. The service picks it up within a few seconds.\n", key)
		if key == "mode" && cfg.Mode == ModeEnforce {
			fmt.Fprintln(stdout, "Enforce mode: processes over the threshold will now be killed.")
		}
		return nil
	default:
		return fmt.Errorf("unknown config command %q (show, set, path)", sub)
	}
}

func printStatus(w io.Writer) error {
	svc := serviceState()
	fmt.Fprintf(w, "Service:   %s\n", svc)
	st, err := readStatus()
	if err != nil {
		fmt.Fprintln(w, "No status yet (the service has not run, or has not finished its first sample).")
		return nil
	}
	age := time.Since(st.Updated).Round(time.Second)
	fmt.Fprintf(w, "Version:   %s (PID %d, up since %s)\n", st.Version, st.PID, st.Started.Format(time.RFC1123))
	fmt.Fprintf(w, "Mode:      %s: over %.0f%% of one core for %.0fs is %s\n", st.Mode, st.CPUThreshold, st.KillDuration,
		map[bool]string{true: "killed", false: "reported only"}[st.Mode == ModeEnforce])
	if st.ConfiguredMode == ModeEnforce && st.Mode != ModeEnforce {
		fmt.Fprintln(w, "           (set to enforce, but not licensed: only reporting)")
	}
	fmt.Fprintf(w, "License:   %s\n", describeSummary(st.License))
	fmt.Fprintf(w, "Updated:   %s ago, %d processes measured\n", age, st.Processes)
	if age > 30*time.Second {
		fmt.Fprintln(w, "           (stale: is the service running?)")
	}
	if st.Warning != "" {
		fmt.Fprintf(w, "Warning:   %s\n", st.Warning)
	}
	if len(st.Hot) == 0 {
		fmt.Fprintln(w, "Hot now:   none")
	} else {
		fmt.Fprintln(w, "Hot now:")
		for _, h := range st.Hot {
			fmt.Fprintf(w, "  PID %-7d %5.1f%%  %-30s for %.0fs\n", h.PID, h.CPU, h.Name, h.HotFor)
		}
	}
	if len(st.Events) > 0 {
		fmt.Fprintln(w, "Recent:")
		for i, ev := range st.Events {
			if i == 10 {
				break
			}
			fmt.Fprintf(w, "  %s  %-11s %s (PID %d) %.0f%% CPU\n", ev.Time.Local().Format("Jan 02 15:04:05"), ev.Action, ev.Name, ev.PID, ev.CPU)
		}
	}
	return nil
}
