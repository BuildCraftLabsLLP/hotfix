package main

import (
	"os"
	"sync"
	"time"
)

// version is stamped by CI: -ldflags "-X main.version=1.2.3".
var version = "dev"

const pollInterval = 5 * time.Second

// engine is the monitoring loop the service runs.
type engine struct {
	cfg     *configStore
	mon     *Monitor
	smp     *sampler
	started time.Time
	warning string

	reload   chan struct{}
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func newEngine(configFile string) (*engine, error) {
	store, err := newConfigStore(configFile)
	if err != nil {
		return nil, err
	}
	e := &engine{
		cfg:     store,
		mon:     newMonitor(os.Getpid(), selfUID(), isElevated()),
		smp:     newSampler(),
		started: time.Now(),
		reload:  make(chan struct{}, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	e.mon.onEvent = func(ev Event) { sendWebhook(e.cfg.Get(), ev) }

	switch {
	case selfInContainer():
		e.warning = "hotfixd is running inside a container and can only see that container's processes. Install it on the host (or run with --pid=host)."
	case !isElevated():
		e.warning = "hotfixd is not running as root/administrator: it can only act on its own user's processes."
	}
	return e, nil
}

// Run blocks until Stop.
func (e *engine) Run() {
	defer close(e.done)
	cfg := e.cfg.Get()
	logf("hotfixd %s started: mode=%s, threshold=%.0f%% of one core, kill after %.0fs, config %s",
		version, cfg.Mode, cfg.CPUThreshold, cfg.KillDuration, e.cfg.path)
	if e.warning != "" {
		logf("warning: %s", e.warning)
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	last := time.Now()
	e.tick(false)
	for {
		select {
		case <-e.stop:
			logf("hotfixd stopped")
			return
		case <-e.reload:
			e.tick(true)
		case now := <-ticker.C:
			// A long gap (suspend, VM pause, stalled host) makes the CPU
			// interval meaningless: start over instead of misreading it.
			if now.Sub(last) > 3*pollInterval {
				logf("monitor: %.0fs since the last sample; starting a fresh measurement", now.Sub(last).Seconds())
				e.smp.Reset()
				e.mon.Reset()
			}
			last = now
			e.tick(false)
		}
	}
}

func (e *engine) tick(forceReload bool) {
	defer func() {
		if r := recover(); r != nil {
			logf("monitor: recovered from panic: %v", r)
		}
	}()

	before := e.cfg.Get()
	if changed, err := e.cfg.Reload(forceReload); err != nil {
		logf("config: %v (keeping the previous settings)", err)
	} else if changed {
		after := e.cfg.Get()
		logf("config: reloaded: mode=%s, threshold=%.0f%%, kill after %.0fs", after.Mode, after.CPUThreshold, after.KillDuration)
		if after.CPUThreshold != before.CPUThreshold || after.KillDuration != before.KillDuration {
			e.mon.Reset()
		}
	}
	cfg := e.cfg.Get()

	procs, err := e.smp.Sample()
	if err != nil {
		logf("monitor: reading processes failed: %v", err)
		return
	}
	e.mon.Step(procs, cfg)
	e.writeStatus(cfg, len(procs))
}

func (e *engine) writeStatus(cfg Config, seen int) {
	hot, events := e.mon.Snapshot()
	st := Status{
		Version: version, PID: os.Getpid(), Started: e.started, Updated: time.Now(),
		Mode: cfg.Mode, CPUThreshold: cfg.CPUThreshold, KillDuration: cfg.KillDuration,
		Processes: seen, Events: events, Warning: e.warning,
	}
	for _, h := range hot {
		st.Hot = append(st.Hot, statusHot{PID: h.PID, Name: h.DisplayName(), CPU: h.CPU, HotFor: time.Since(h.HotSince).Seconds()})
	}
	if err := writeStatus(st); err != nil && isElevated() {
		logf("status: %v", err)
	}
}

// Reload asks the loop to reread the config now (SIGHUP, service reload).
func (e *engine) Reload() {
	select {
	case e.reload <- struct{}{}:
	default:
	}
}

func (e *engine) Stop() {
	e.stopOnce.Do(func() { close(e.stop) })
	<-e.done
}
