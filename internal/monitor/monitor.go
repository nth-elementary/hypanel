// Package monitor samples CPU, memory, disk and network. Recent samples stay in
// memory for the live view; one per minute goes to SQLite and is kept for 7 days.
package monitor

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"

	"hypanel/internal/core"
)

const (
	interval  = 2 * time.Second
	liveCount = 900 // 30 minutes at 2 seconds
	retention = 7 * 24 * time.Hour
)

type Sample struct {
	TS       int64   `json:"ts"`
	CPU      float64 `json:"cpu"`
	Mem      float64 `json:"mem"`
	Swap     float64 `json:"swap"`
	Disk     float64 `json:"disk"`
	NetRx    float64 `json:"netRx"` // bytes per second
	NetTx    float64 `json:"netTx"`
	DiskRead float64 `json:"diskRead"`
	DiskWrit float64 `json:"diskWrite"`
}

type Monitor struct {
	app *core.App

	mu   sync.RWMutex
	live []Sample

	// Previous reading for the process table, and cached container names.
	topMu     sync.Mutex
	prevProcs procTimes
	prevTotal float64
	prevAt    time.Time
	names     map[string]string
	namesAt   time.Time
}

func New(app *core.App) (*Monitor, error) {
	_, err := app.DB.Exec(`CREATE TABLE IF NOT EXISTS metrics (ts INTEGER PRIMARY KEY, cpu REAL, mem REAL, swap REAL, disk REAL, net_rx REAL, net_tx REAL, disk_read REAL, disk_write REAL)`)
	if err != nil {
		return nil, err
	}
	return &Monitor{app: app}, nil
}

func (m *Monitor) Name() string { return "monitor" }

func (m *Monitor) Routes(r chi.Router) {
	r.Get("/monitor/current", m.current)
	r.Get("/monitor/history", m.history)
	r.Get("/monitor/top", m.top)
}

// Run samples until ctx ends. A failed read leaves that metric at zero for one
// sample rather than stopping the loop.
func (m *Monitor) Run(ctx context.Context) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	var prevRx, prevTx, prevRead, prevWrite uint64
	var prevTime time.Time
	var lastPersist int64
	_, _ = cpu.Percent(0, false) // the first call only sets the baseline
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			s := Sample{TS: now.Unix()}
			if p, err := cpu.Percent(0, false); err == nil && len(p) > 0 {
				s.CPU = p[0]
			}
			if v, err := mem.VirtualMemory(); err == nil {
				s.Mem = v.UsedPercent
			}
			if v, err := mem.SwapMemory(); err == nil {
				s.Swap = v.UsedPercent
			}
			if v, err := disk.Usage("/"); err == nil {
				s.Disk = v.UsedPercent
			}

			var rx, tx uint64
			if ifs, err := net.IOCounters(true); err == nil {
				for _, i := range ifs {
					if !virtualIface(i.Name) {
						rx += i.BytesRecv
						tx += i.BytesSent
					}
				}
			}
			var rd, wr uint64
			if ds, err := disk.IOCounters(); err == nil {
				for _, d := range ds {
					rd += d.ReadBytes
					wr += d.WriteBytes
				}
			}
			if !prevTime.IsZero() {
				dt := now.Sub(prevTime).Seconds()
				s.NetRx = rate(rx, prevRx, dt)
				s.NetTx = rate(tx, prevTx, dt)
				s.DiskRead = rate(rd, prevRead, dt)
				s.DiskWrit = rate(wr, prevWrite, dt)
			}
			prevRx, prevTx, prevRead, prevWrite, prevTime = rx, tx, rd, wr, now

			m.mu.Lock()
			m.live = append(m.live, s)
			if len(m.live) > liveCount {
				m.live = append(m.live[:0], m.live[len(m.live)-liveCount:]...)
			}
			m.mu.Unlock()

			if minute := s.TS / 60; minute != lastPersist {
				lastPersist = minute
				m.persist(s)
			}
		}
	}
}

// rate is bytes per second between two counter readings. A counter reset
// (interface recreated) yields zero, not a spike.
func rate(cur, prev uint64, dt float64) float64 {
	if cur < prev || dt <= 0 {
		return 0
	}
	return float64(cur-prev) / dt
}

// virtualIface excludes loopback and container interfaces, whose traffic would
// otherwise be counted a second time on top of the physical interface.
func virtualIface(name string) bool {
	for _, p := range []string{"lo", "docker", "br-", "veth"} {
		if len(name) >= len(p) && name[:len(p)] == p {
			return true
		}
	}
	return false
}

func (m *Monitor) persist(s Sample) {
	_, _ = m.app.DB.Exec(`INSERT OR REPLACE INTO metrics VALUES(?,?,?,?,?,?,?,?,?)`,
		s.TS, s.CPU, s.Mem, s.Swap, s.Disk, s.NetRx, s.NetTx, s.DiskRead, s.DiskWrit)
	_, _ = m.app.DB.Exec(`DELETE FROM metrics WHERE ts < ?`, time.Now().Add(-retention).Unix())
}

func (m *Monitor) current(w http.ResponseWriter, r *http.Request) {
	m.mu.RLock()
	live := make([]Sample, len(m.live))
	copy(live, m.live)
	m.mu.RUnlock()

	out := map[string]any{"live": live}
	if v, err := mem.VirtualMemory(); err == nil {
		out["memTotal"], out["memUsed"] = v.Total, v.Used
	}
	if v, err := mem.SwapMemory(); err == nil {
		out["swapTotal"], out["swapUsed"] = v.Total, v.Used
	}
	if v, err := disk.Usage("/"); err == nil {
		out["diskTotal"], out["diskUsed"] = v.Total, v.Used
	}
	if v, err := load.Avg(); err == nil {
		out["load"] = []float64{v.Load1, v.Load5, v.Load15}
	}
	if v, err := host.Info(); err == nil {
		out["hostname"], out["uptime"], out["os"], out["kernel"] = v.Hostname, v.Uptime, v.Platform+" "+v.PlatformVersion, v.KernelVersion
	}
	if n, err := cpu.Counts(true); err == nil {
		out["cores"] = n
	}
	core.OK(w, out)
}

func (m *Monitor) history(w http.ResponseWriter, r *http.Request) {
	span := map[string]time.Duration{"1h": time.Hour, "24h": 24 * time.Hour, "7d": retention}[r.URL.Query().Get("range")]
	if span == 0 {
		span = 24 * time.Hour
	}
	// Bucketing keeps a 7-day response to a few hundred points.
	bucket := int64(span.Seconds()) / 300
	if bucket < 60 {
		bucket = 60
	}
	rows, err := m.app.DB.Query(`SELECT ts/?*? AS b, AVG(cpu), AVG(mem), AVG(swap), AVG(disk), AVG(net_rx), AVG(net_tx), AVG(disk_read), AVG(disk_write)
		FROM metrics WHERE ts >= ? GROUP BY b ORDER BY b`, bucket, bucket, time.Now().Add(-span).Unix())
	if err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	defer rows.Close()
	out := []Sample{}
	for rows.Next() {
		var s Sample
		if rows.Scan(&s.TS, &s.CPU, &s.Mem, &s.Swap, &s.Disk, &s.NetRx, &s.NetTx, &s.DiskRead, &s.DiskWrit) == nil {
			out = append(out, s)
		}
	}
	core.OK(w, out)
}
