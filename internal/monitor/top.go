package monitor

import (
	"context"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"

	"hypanel/internal/core"
)

const (
	topRows      = 15
	containerTTL = 15 * time.Second
	// A previous reading older than this says nothing about what's busy right now.
	staleReading = 30 * time.Second
)

var containerInCgroup = regexp.MustCompile(`(?:docker-|docker/)([0-9a-f]{64})`)

// Proc is one row of the process table.
type Proc struct {
	PID       int32   `json:"pid"`
	User      string  `json:"user"`
	Command   string  `json:"command"`
	Container string  `json:"container"`
	CPU       float64 `json:"cpu"` // share of the whole server, 0-100, like the container page
	Memory    uint64  `json:"memory"`
	MemoryPct float64 `json:"memoryPct"`
}

type procTimes map[int32]float64

// reading is a snapshot of cumulative CPU time, per process and for the whole server.
func reading(ctx context.Context) (procTimes, float64, []*process.Process, error) {
	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return nil, 0, nil, err
	}
	times := make(procTimes, len(procs))
	for _, p := range procs {
		if t, err := p.TimesWithContext(ctx); err == nil {
			times[p.Pid] = t.User + t.System
		}
	}
	all, err := cpu.TimesWithContext(ctx, false)
	if err != nil || len(all) == 0 {
		return nil, 0, nil, err
	}
	return times, all[0].Total(), procs, nil
}

// containerNames maps full container IDs to names, briefly cached so a refresh of the
// process table doesn't run docker every time.
func (m *Monitor) containerNames(ctx context.Context) map[string]string {
	m.topMu.Lock()
	defer m.topMu.Unlock()
	if m.names != nil && time.Since(m.namesAt) < containerTTL {
		return m.names
	}
	names := map[string]string{}
	if out, err := core.Run(ctx, "docker", "ps", "--no-trunc", "--format", "{{.ID}} {{.Names}}"); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if id, name, ok := strings.Cut(line, " "); ok {
				names[id] = name
			}
		}
	}
	m.names, m.namesAt = names, time.Now()
	return names
}

func containerOf(pid int32, names map[string]string) string {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(int(pid)) + "/cgroup")
	if err != nil {
		return ""
	}
	if match := containerInCgroup.FindSubmatch(data); match != nil {
		return names[string(match[1])]
	}
	return ""
}

// top lists the busiest processes. CPU is measured between this request and the last, so
// the cost is only ever paid while someone has the dashboard open.
func (m *Monitor) top(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	m.topMu.Lock()
	prevProcs, prevTotal, prevAt := m.prevProcs, m.prevTotal, m.prevAt
	m.topMu.Unlock()

	if prevProcs == nil || time.Since(prevAt) > staleReading {
		times, total, _, err := reading(ctx)
		if err != nil {
			core.Fail(w, http.StatusInternalServerError, "%v", err)
			return
		}
		prevProcs, prevTotal = times, total
		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			return
		}
	}
	times, total, procs, err := reading(ctx)
	if err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	m.topMu.Lock()
	m.prevProcs, m.prevTotal, m.prevAt = times, total, time.Now()
	m.topMu.Unlock()

	var hostMem uint64
	if v, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		hostMem = v.Total
	}
	elapsed := total - prevTotal
	rows := make([]Proc, 0, len(procs))
	byPID := make(map[int32]*process.Process, len(procs))
	for _, p := range procs {
		byPID[p.Pid] = p
		row := Proc{PID: p.Pid}
		if now, ok := times[p.Pid]; ok && elapsed > 0 {
			if before, seen := prevProcs[p.Pid]; seen && now > before {
				row.CPU = (now - before) / elapsed * 100
			}
		}
		if info, err := p.MemoryInfoWithContext(ctx); err == nil && info != nil {
			row.Memory = info.RSS
			if hostMem > 0 {
				row.MemoryPct = float64(info.RSS) / float64(hostMem) * 100
			}
		}
		rows = append(rows, row)
	}
	if r.URL.Query().Get("sort") == "memory" {
		sort.Slice(rows, func(i, j int) bool { return rows[i].Memory > rows[j].Memory })
	} else {
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].CPU != rows[j].CPU {
				return rows[i].CPU > rows[j].CPU
			}
			return rows[i].Memory > rows[j].Memory
		})
	}
	count := len(rows)
	if len(rows) > topRows {
		rows = rows[:topRows]
	}
	// Names and command lines are only read for the rows being shown.
	names := m.containerNames(ctx)
	for i := range rows {
		p := byPID[rows[i].PID]
		rows[i].User, _ = p.UsernameWithContext(ctx)
		rows[i].Command, _ = p.CmdlineWithContext(ctx)
		if rows[i].Command == "" {
			name, _ := p.NameWithContext(ctx)
			rows[i].Command = "[" + name + "]"
		}
		if len(rows[i].Command) > 300 {
			rows[i].Command = rows[i].Command[:300] + "..."
		}
		rows[i].Container = containerOf(rows[i].PID, names)
	}
	core.OK(w, map[string]any{"processes": rows, "total": count})
}
