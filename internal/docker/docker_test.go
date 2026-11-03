package docker

import (
	"encoding/binary"
	"testing"
)

func TestParseStats(t *testing.T) {
	st, sample, err := parseStats([]byte(`{
		"cpu_stats": {"cpu_usage": {"total_usage": 500}, "system_cpu_usage": 2000},
		"memory_stats": {"usage": 1000, "limit": 8000, "stats": {"inactive_file": 300}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if st.MemUsed != 700 || st.MemLimit != 8000 {
		t.Errorf("memory: %+v", st)
	}
	if sample.total != 500 || sample.system != 2000 {
		t.Errorf("sample: %+v", sample)
	}
}

func TestCPUPercent(t *testing.T) {
	for name, c := range map[string]struct {
		prev, cur cpuSample
		want      float64
	}{
		"quarter of host": {cpuSample{100, 1000}, cpuSample{350, 2000}, 25},
		"first reading":   {cpuSample{}, cpuSample{350, 2000}, 0},
		"idle":            {cpuSample{100, 1000}, cpuSample{100, 2000}, 0},
		"counter reset":   {cpuSample{900, 1000}, cpuSample{10, 2000}, 0},
	} {
		if got := cpuPercent(c.prev, c.cur); got != c.want {
			t.Errorf("%s: got %v, want %v", name, got, c.want)
		}
	}
}

func TestDemux(t *testing.T) {
	frame := func(stream byte, s string) []byte {
		h := make([]byte, 8)
		h[0] = stream
		binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
		return append(h, s...)
	}
	if got := demux(append(frame(1, "out\n"), frame(2, "err\n")...)); got != "out\nerr\n" {
		t.Errorf("framed: %q", got)
	}
	if got := demux([]byte("plain tty output\n")); got != "plain tty output\n" {
		t.Errorf("tty: %q", got)
	}
}
