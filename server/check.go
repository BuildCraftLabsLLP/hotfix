package main

// `hotfixd check`: one measurement of every process, showing what the monitor
// would do with each, so admins can tune settings before enforcing.

import (
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"
	"time"
)

func runCheck(w io.Writer, cfg Config, interval time.Duration, top int) error {
	s := newSampler()
	if _, err := s.Sample(); err != nil {
		return err
	}
	time.Sleep(interval)
	procs, err := s.Sample()
	if err != nil {
		return err
	}
	sort.Slice(procs, func(i, j int) bool { return procs[i].CPU > procs[j].CPU })

	mon := newMonitor(os.Getpid(), selfUID(), isElevated())
	parents := parentMap(procs)

	fmt.Fprintf(w, "Mode: %s. A process above %.0f%% of one core for %.0fs is %s.\n\n",
		cfg.Mode, cfg.CPUThreshold, cfg.KillDuration, map[bool]string{true: "killed", false: "reported (monitor mode)"}[cfg.Mode == ModeEnforce])
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PID\tCPU%\tNAME\tWHAT HOTFIX DOES")
	for i, p := range procs {
		if i >= top {
			break
		}
		var exe, user string
		verdict := mon.skipReason(p, cfg, &exe, &user, parents)
		switch {
		case verdict != "":
			verdict = "never: " + verdict
		case p.CPU >= cfg.CPUThreshold:
			verdict = fmt.Sprintf("over the threshold: %s if it stays there for %.0fs",
				map[bool]string{true: "killed", false: "reported"}[cfg.Mode == ModeEnforce], cfg.KillDuration)
		default:
			verdict = "fine (below the threshold)"
		}
		h := HotProcess{Process: p, Exe: exe}
		fmt.Fprintf(tw, "%d\t%.1f\t%s\t%s\n", p.PID, p.CPU, h.DisplayName(), verdict)
	}
	tw.Flush()
	if !isElevated() {
		fmt.Fprintln(w, "\nNote: not running as root/administrator, so other users' processes may be missing or shown as never killed.")
	}
	return nil
}
