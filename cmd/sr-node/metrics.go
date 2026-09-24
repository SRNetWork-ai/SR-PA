package main

import (
	"bufio"
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// What the machine is doing, read straight from /proc rather than through a
// dependency. These numbers exist for one purpose - a column on the fleet page
// that tells an operator which node is in trouble - so being approximately
// right and never failing beats being exact: every reading here returns zero
// rather than an error, because a missing number must not cost a heartbeat.

func (a *agent) cpuPct() float64 {
	idle, total, err := cpuSample()
	if err != nil {
		return 0
	}
	defer func() {
		a.cpuIdle, a.cpuTotal = idle, total
	}()
	if total <= a.cpuTotal {
		return 0
	}
	deltaTotal := float64(total - a.cpuTotal)
	deltaIdle := float64(idle - a.cpuIdle)
	busy := (deltaTotal - deltaIdle) / deltaTotal * 100
	return clampPct(busy)
}

func cpuSample() (uint64, uint64, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		var idle, total uint64
		for i, raw := range strings.Fields(line)[1:] {
			v, convErr := strconv.ParseUint(raw, 10, 64)
			if convErr != nil {
				continue
			}
			total += v
			// idle and iowait: a box waiting on disk is not a box doing work.
			if i == 3 || i == 4 {
				idle += v
			}
		}
		return idle, total, nil
	}
	return 0, 0, errors.New("no cpu line in /proc/stat")
}

func memPct() float64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()

	var total, available float64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		value, convErr := strconv.ParseFloat(fields[1], 64)
		if convErr != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = value
		case "MemAvailable:":
			available = value
		}
	}
	if total <= 0 {
		return 0
	}
	// MemAvailable, not MemFree: cache is memory the kernel will hand back on
	// demand, and counting it as used makes every healthy Linux box look full.
	return clampPct((total - available) / total * 100)
}

func diskPct(path string) float64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	total := float64(st.Blocks) * float64(st.Bsize)
	free := float64(st.Bavail) * float64(st.Bsize)
	if total <= 0 {
		return 0
	}
	// Bavail, not Bfree: the blocks reserved for root are not space this node can
	// use for logs or a core download.
	return clampPct((total - free) / total * 100)
}

// netTotals sums the machine's real interfaces. Loopback and container bridges
// are skipped: counting them would report traffic that never left the box.
func netTotals() (int64, int64) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	var up, down int64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		colon := strings.Index(line, ":")
		if colon < 0 {
			continue
		}
		name := strings.TrimSpace(line[:colon])
		if name == "lo" || strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "br-") {
			continue
		}
		fields := strings.Fields(line[colon+1:])
		if len(fields) < 9 {
			continue
		}
		if rx, convErr := strconv.ParseInt(fields[0], 10, 64); convErr == nil {
			down += rx
		}
		if tx, convErr := strconv.ParseInt(fields[8], 10, 64); convErr == nil {
			up += tx
		}
	}
	return up, down
}

func hostUptime() int64 {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return int64(seconds)
}

func clampPct(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
