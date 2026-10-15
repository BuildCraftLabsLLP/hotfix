package main

// The system service, via github.com/kardianos/service: systemd on Linux, the
// Service Control Manager on Windows (LocalSystem), launchd on macOS (a
// LaunchDaemon, as root). The service runs `hotfixd run`.

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"

	"github.com/kardianos/service"
)

func serviceName() string {
	if runtime.GOOS == "darwin" {
		return "town.buildcraft.hotfixd" // launchd label
	}
	return "hotfixd"
}

func serviceConfig() *service.Config {
	return &service.Config{
		Name:        serviceName(),
		DisplayName: "Hotfix Server",
		Description: "Watches CPU use and kills processes that run away (hotfix.buildcraft.town).",
		Arguments:   []string{"run"},
		Option: service.KeyValue{
			// Linux: our hardened unit (packaging/hotfixd.service).
			"SystemdScript": systemdUnitTemplate(),
			// Windows: start after boot settles, restart on crashes.
			"DelayedAutoStart":       true,
			"OnFailure":              "restart",
			"OnFailureDelayDuration": "10s",
			"OnFailureResetPeriod":   86400,
			// macOS: start at boot, restart if it exits.
			"KeepAlive": true,
			"RunAtLoad": true,
		},
	}
}

func newService(p service.Interface) (service.Service, error) {
	return service.New(p, serviceConfig())
}

// isServiceManaged reports whether a service manager (not a terminal) started us.
func isServiceManaged() bool { return !service.Interactive() }

// program adapts the engine to the service manager.
type program struct {
	e   *engine
	log service.Logger
}

func (p *program) Start(s service.Service) error {
	if err := initLog(logFilePath()); err != nil && p.log != nil {
		p.log.Warningf("hotfixd: cannot open log file: %v", err)
	}
	logStdout = nil
	if runtime.GOOS == "linux" {
		logStdout = os.Stdout // journald
	}
	e, err := newEngine(configPath())
	if err != nil {
		if p.log != nil {
			p.log.Errorf("hotfixd: %v", err)
		}
		return err
	}
	p.e = e
	if sigs := reloadSignals(); len(sigs) > 0 {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, sigs...)
		go func() {
			for range ch {
				e.Reload()
			}
		}()
	}
	go e.Run()
	if p.log != nil {
		p.log.Infof("Hotfix Server %s started (mode %s)", version, e.cfg.Get().Mode)
	}
	return nil
}

func (p *program) Stop(s service.Service) error {
	if p.e != nil {
		p.e.Stop()
	}
	if p.log != nil {
		p.log.Info("Hotfix Server stopped")
	}
	closeLog()
	return nil
}

// runService hands control to the service manager.
func runService() error {
	p := &program{}
	s, err := newService(p)
	if err != nil {
		return err
	}
	if l, err := s.Logger(nil); err == nil {
		p.log = l // Windows: the Event Log
	}
	return s.Run()
}

// serviceCommand implements install / uninstall / start / stop / restart.
func serviceCommand(cmd string) error {
	if err := needElevation(); err != nil {
		return err
	}
	if !licenseEnforced() && cmd == "install" && os.Getenv("HOTFIXD_ALLOW_DEV_INSTALL") == "" {
		return errors.New("this is an unlicensed development build; install an official release from hotfix.buildcraft.town/server.html")
	}
	if handled, err := packagedServiceCommand(cmd); handled {
		return err
	}
	s, err := newService(&program{})
	if err != nil {
		return err
	}
	switch cmd {
	case "install":
		if err := prepareDataDirs(); err != nil {
			return err
		}
		if _, err := s.Status(); !errors.Is(err, service.ErrNotInstalled) {
			// Already installed: just make sure it runs.
			_ = service.Control(s, "start")
			fmt.Println("Hotfix Server is already installed; it's running. (`hotfixd status` shows what it's doing.)")
			return nil
		}
		if err := service.Control(s, "install"); err != nil {
			return err
		}
		if err := service.Control(s, "start"); err != nil {
			return fmt.Errorf("installed, but could not start: %w", err)
		}
		printInstalled()
		return nil
	case "uninstall":
		_ = service.Control(s, "stop")
		if err := service.Control(s, "uninstall"); err != nil {
			return err
		}
		fmt.Printf("Hotfix Server removed. Settings are kept in %s.\n", configDir())
		return nil
	default:
		return service.Control(s, cmd)
	}
}

// prepareDataDirs creates the config/state/log directories (locking them down
// on Windows) and a default config if there is none.
func prepareDataDirs() error {
	for _, d := range []string{configDir(), stateDir(), logDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if err := secureDataDir(configDir()); err != nil {
		return fmt.Errorf("securing %s: %w", configDir(), err)
	}
	if _, err := os.Stat(configPath()); errors.Is(err, os.ErrNotExist) {
		return writeConfig(configPath(), defaultConfig())
	}
	return nil
}

func printInstalled() {
	fmt.Printf(`Hotfix Server is installed and running.

It starts in MONITOR mode: it only logs what it would kill. Watch it for a
while, then switch to killing with:

    hotfixd config set mode enforce

See what it's doing:  hotfixd status
Settings file:        %s
Log:                  %s
`, configPath(), logFilePath())
}

// serviceState describes the service for `hotfixd status`.
func serviceState() string {
	if st, ok := packagedServiceState(); ok {
		return st
	}
	s, err := newService(&program{})
	if err != nil {
		return "unknown"
	}
	st, err := s.Status()
	switch {
	case errors.Is(err, service.ErrNotInstalled):
		return "not installed (run `hotfixd install`)"
	case err != nil:
		return "unknown (" + err.Error() + ")"
	case st == service.StatusRunning:
		return "running"
	case st == service.StatusStopped:
		return "stopped (run `hotfixd start`)"
	default:
		return "unknown"
	}
}
