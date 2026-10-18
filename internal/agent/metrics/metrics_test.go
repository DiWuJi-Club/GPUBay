package metrics_test

import (
	"context"
	"errors"
	"testing"

	"spark-cloud/internal/agent/cmdrun"
	"spark-cloud/internal/agent/metrics"
)

func TestParseNvidiaSMIHandlesNA(t *testing.T) {
	u, tc, p := metrics.ParseNvidiaSMI("12, 45, 30.50\n")
	if u != 12 || tc != 45 || p != 30.5 {
		t.Fatalf("parsed = %v %v %v", u, tc, p)
	}
	u, tc, p = metrics.ParseNvidiaSMI("[N/A], 51, [N/A]\n")
	if u != 0 || tc != 51 || p != 0 {
		t.Fatalf("N/A = %v %v %v", u, tc, p)
	}
	if u, tc, p = metrics.ParseNvidiaSMI(""); u != 0 || tc != 0 || p != 0 {
		t.Fatal("empty")
	}
}

func TestParseMeminfoDFLoadavg(t *testing.T) {
	total, used := metrics.ParseMeminfo([]byte("MemTotal:       131072000 kB\nMemFree:  1000 kB\nMemAvailable:   100000000 kB\n"))
	if total != 131072000*1024 || used != (131072000-100000000)*1024 {
		t.Fatalf("meminfo = %d %d", total, used)
	}
	dt, du := metrics.ParseDF("        1B-blocks       Used\n4000787030016 1234567\n")
	if dt != 4000787030016 || du != 1234567 {
		t.Fatalf("df = %d %d", dt, du)
	}
	if l := metrics.ParseLoadavg([]byte("0.52 0.40 0.30 1/900 1234\n")); l != 0.52 {
		t.Fatalf("load = %v", l)
	}
}

func TestCollectNeverFails(t *testing.T) {
	r := &cmdrun.Recorder{}
	r.Stub("nvidia-smi", "", errors.New("no gpu"))
	r.Stub("df -B1 --output=size,used /", "1B-blocks Used\n1000 900\n", nil)
	r.Stub("df -B1 --output=size,used /var/lib/spark-agent/mnt/ins-a", "1B-blocks Used\n2000 5\n", nil)
	files := map[string]string{"/proc/meminfo": "MemTotal: 100 kB\nMemAvailable: 40 kB\n", "/proc/loadavg": "1.25 0 0 1/1 1\n"}
	c := &metrics.Collector{Run: r, ReadFile: func(p string) ([]byte, error) {
		if s, ok := files[p]; ok {
			return []byte(s), nil
		}
		return nil, errors.New("missing")
	}}
	m := c.Collect(context.Background(), "/var/lib/spark-agent/mnt/ins-a")
	if m.GPUUtil != 0 || m.RootDiskTotal != 1000 || m.RootDiskUsed != 900 || m.DataDiskUsed != 5 || m.MemTotal != 100*1024 || m.MemUsed != 60*1024 || m.Load1 != 1.25 {
		t.Fatalf("metrics = %+v", m)
	}
	if m := c.Collect(context.Background(), ""); m.DataDiskUsed != 0 {
		t.Fatal("no data mount must give 0")
	}
	if r.Lines() != "nvidia-smi --query-gpu=utilization.gpu,temperature.gpu,power.draw --format=csv,noheader,nounits\ndf -B1 --output=size,used /\ndf -B1 --output=size,used /var/lib/spark-agent/mnt/ins-a\nnvidia-smi --query-gpu=utilization.gpu,temperature.gpu,power.draw --format=csv,noheader,nounits\ndf -B1 --output=size,used /" {
		t.Fatalf("calls:\n%s", r.Lines())
	}
}
