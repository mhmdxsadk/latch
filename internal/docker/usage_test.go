package docker

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

const statsFixture = `{
  "read": "2024-05-01T10:00:00Z",
  "cpu_stats": {"cpu_usage": {"total_usage": 300000000}, "system_cpu_usage": 2000000000, "online_cpus": 4},
  "precpu_stats": {"cpu_usage": {"total_usage": 200000000}, "system_cpu_usage": 1000000000, "online_cpus": 4},
  "memory_stats": {"usage": 1500, "limit": 4000, "stats": {"inactive_file": 500, "active_file": 99}},
  "networks": {"eth0": {"rx_bytes": 10, "tx_bytes": 20}, "eth1": {"rx_bytes": 1, "tx_bytes": 2}},
  "blkio_stats": {"io_service_bytes_recursive": [
    {"major": 8, "minor": 0, "op": "read", "value": 100},
    {"major": 8, "minor": 0, "op": "Write", "value": 200},
    {"major": 8, "minor": 0, "op": "Read", "value": 50},
    {"major": 8, "minor": 0, "op": "Total", "value": 350}]},
  "pids_stats": {"current": 7},
  "name": "/web", "id": "SECRET-ID"
}`

func usageServer(t *testing.T, stats string) (*Engine, *recorder) {
	return newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/json") {
			_, _ = w.Write([]byte(fixture(false)))
			return
		}
		_, _ = w.Write([]byte(stats))
	})
}

func TestUsage(t *testing.T) {
	e, rec := usageServer(t, statsFixture)
	u, err := e.Usage(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	want := Usage{
		CPUPercent: 40, MemoryUsedBytes: 1000, MemoryLimitBytes: 4000, MemoryPercent: 25,
		NetRxBytes: 11, NetTxBytes: 22, BlockReadBytes: 150, BlockWriteBytes: 200, PIDs: 7,
	}
	if u != want {
		t.Errorf("got %+v want %+v", u, want)
	}
	last := rec.reqs[len(rec.reqs)-1]
	if last.URL.Path != "/containers/"+testID+"/stats" || last.URL.RawQuery != "stream=false" {
		t.Errorf("stats request %s", last.URL)
	}
}

func TestUsageCgroupV1CacheAndMissingPrecpu(t *testing.T) {
	e, _ := usageServer(t, `{
	  "cpu_stats": {"cpu_usage": {"total_usage": 300, "percpu_usage": [1,2]}, "system_cpu_usage": 2000},
	  "precpu_stats": {"cpu_usage": {"total_usage": 0}, "system_cpu_usage": 0},
	  "memory_stats": {"usage": 100, "limit": 0, "stats": {"total_inactive_file": 400}}}`)
	u, err := e.Usage(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	// cache larger than usage: keep usage; no limit: no percent; no precpu: 0.
	if u.CPUPercent != 0 || u.MemoryUsedBytes != 100 || u.MemoryPercent != 0 {
		t.Errorf("got %+v", u)
	}
	e, _ = usageServer(t, `{"memory_stats": {"usage": 300, "limit": 1000, "stats": {"total_inactive_file": 100}}}`)
	if u, _ = e.Usage(context.Background(), "web"); u.MemoryUsedBytes != 200 || u.MemoryPercent != 20 {
		t.Errorf("got %+v", u)
	}
}

func TestUsagePercpuFallback(t *testing.T) {
	e, _ := usageServer(t, `{
	  "cpu_stats": {"cpu_usage": {"total_usage": 200, "percpu_usage": [1,2]}, "system_cpu_usage": 1100},
	  "precpu_stats": {"cpu_usage": {"total_usage": 100}, "system_cpu_usage": 100}}`)
	u, _ := e.Usage(context.Background(), "web")
	if u.CPUPercent != 20 {
		t.Errorf("got %v", u.CPUPercent)
	}
}

func TestUsageErrors(t *testing.T) {
	e, _ := usageServer(t, `not json`)
	if _, err := e.Usage(context.Background(), "web"); err == nil {
		t.Error("malformed stats accepted")
	}
	e, rec := usageServer(t, statsFixture)
	if _, err := e.Usage(context.Background(), "../web"); err == nil || rec.count() != 0 {
		t.Errorf("invalid name: err=%v requests=%d", err, rec.count())
	}
}
