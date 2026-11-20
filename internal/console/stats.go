package console

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// systemdStats prints a resource summary for a unit: state, uptime, CPU (now
// and total), memory, tasks, restarts, then its processes. Everything here is
// readable without root, so it does not go through sudo.
func systemdStats(ctx context.Context, w io.Writer, unit string) error {
	props := []string{"ActiveState", "SubState", "MainPID", "ActiveEnterTimestamp", "NRestarts",
		"MemoryCurrent", "MemoryPeak", "CPUUsageNSec", "TasksCurrent", "ControlGroup"}
	show := func() (map[string]string, error) {
		args := []string{"show", "--no-pager"}
		for _, p := range props {
			args = append(args, "-p", p)
		}
		out, err := exec.CommandContext(ctx, "systemctl", append(args, "--", unit)...).Output()
		if err != nil {
			return nil, fmt.Errorf("systemctl show: %w", err)
		}
		m := map[string]string{}
		for _, l := range strings.Split(string(out), "\n") {
			if k, v, ok := strings.Cut(l, "="); ok {
				m[k] = v
			}
		}
		return m, nil
	}
	m, err := show()
	if err != nil {
		return err
	}
	// sample CPU twice to show current usage, like top
	cpuNow := ""
	if c1, ok := num(m["CPUUsageNSec"]); ok && m["ActiveState"] == "active" {
		t1 := time.Now()
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
		if m2, err := show(); err == nil {
			if c2, ok := num(m2["CPUUsageNSec"]); ok {
				pct := float64(c2-c1) / float64(time.Since(t1).Nanoseconds()) * 100
				cpuNow = fmt.Sprintf("%.1f%% of one core", pct)
				m = m2
			}
		}
	}

	row := func(k, v string) {
		if v != "" {
			fmt.Fprintf(w, "%-11s %s\n", k, v)
		}
	}
	state := m["ActiveState"]
	if s := m["SubState"]; s != "" {
		state += " (" + s + ")"
	}
	row("Unit", unit)
	row("State", state)
	if ts := m["ActiveEnterTimestamp"]; ts != "" && m["ActiveState"] == "active" {
		if t, err := time.Parse("Mon 2006-01-02 15:04:05 MST", ts); err == nil {
			row("Up since", ts+"  ("+fmtDur(time.Since(t))+")")
		} else {
			row("Up since", ts)
		}
	}
	if pid := m["MainPID"]; pid != "" && pid != "0" {
		row("Main PID", pid)
	}
	row("CPU now", cpuNow)
	if c, ok := num(m["CPUUsageNSec"]); ok {
		row("CPU total", fmtDur(time.Duration(c)))
	}
	if mem, ok := num(m["MemoryCurrent"]); ok {
		v := fmtBytes(mem)
		if peak, ok := num(m["MemoryPeak"]); ok {
			v += "  (peak " + fmtBytes(peak) + ")"
		}
		row("Memory", v)
	}
	if t, ok := num(m["TasksCurrent"]); ok {
		row("Tasks", strconv.FormatUint(t, 10))
	}
	row("Restarts", m["NRestarts"])

	// processes in the unit's cgroup
	cg := m["ControlGroup"]
	if cg == "" {
		return nil
	}
	b, err := os.ReadFile("/sys/fs/cgroup" + cg + "/cgroup.procs")
	if err != nil {
		return nil // cgroup v1 or not running: the summary above is enough
	}
	pids := strings.Fields(string(b))
	if len(pids) == 0 {
		return nil
	}
	if len(pids) > 50 {
		pids = pids[:50]
	}
	fmt.Fprintf(w, "\nProcesses (%%CPU is the average since each process started):\n")
	ps := exec.CommandContext(ctx, "ps", "-o", "pid,user,%cpu,%mem,rss,etime,args", "-p", strings.Join(pids, ","))
	ps.Stdout, ps.Stderr = w, w
	ps.Run()
	return nil
}

// num parses a systemd numeric property; "[not set]" and the max uint64 mean unknown.
func num(s string) (uint64, bool) {
	n, err := strconv.ParseUint(s, 10, 64)
	return n, err == nil && n != ^uint64(0)
}

func fmtBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func fmtDur(d time.Duration) string {
	if d < time.Minute {
		return d.Round(10 * time.Millisecond).String()
	}
	d = d.Round(time.Second)
	days := d / (24 * time.Hour)
	d -= days * 24 * time.Hour
	h, m, s := d/time.Hour, (d%time.Hour)/time.Minute, (d%time.Minute)/time.Second
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh %dm", days, h, m)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	default:
		return fmt.Sprintf("%dm %ds", m, s)
	}
}
