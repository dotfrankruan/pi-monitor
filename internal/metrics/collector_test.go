package metrics

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCollectorFromFixture(t *testing.T) {
	root := t.TempDir()
	proc := filepath.Join(root, "proc")
	sys := filepath.Join(root, "sys")
	mustWrite(t, filepath.Join(proc, "meminfo"), "MemTotal: 1000 kB\nMemAvailable: 250 kB\n")
	mustWrite(t, filepath.Join(proc, "loadavg"), "1.25 0.80 0.30 1/100 123\n")
	mustWrite(t, filepath.Join(proc, "uptime"), "86400.5 0.0\n")
	mustWrite(t, filepath.Join(proc, "stat"), "cpu 100 0 50 800 10 0 0 0 0 0\ncpu0 50 0 25 400 5 0 0 0 0 0\ncpu1 50 0 25 400 5 0 0 0 0 0\n")
	mustWrite(t, filepath.Join(sys, "class/hwmon/hwmon0/name"), "cpu_thermal\n")
	mustWrite(t, filepath.Join(sys, "class/hwmon/hwmon0/temp1_input"), "52500\n")
	mustWrite(t, filepath.Join(sys, "class/hwmon/hwmon1/name"), "pwmfan\n")
	mustWrite(t, filepath.Join(sys, "class/hwmon/hwmon1/fan1_input"), "3025\n")
	mustWrite(t, filepath.Join(sys, "class/hwmon/hwmon1/pwm1"), "75\n")
	mustWrite(t, filepath.Join(sys, "devices/system/cpu/cpufreq/policy0/scaling_cur_freq"), "1700000\n")

	c := NewCollector(proc, sys, root)
	s, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect returned error: %v", err)
	}
	if s.CPUTempC == nil || *s.CPUTempC != 52.5 {
		t.Fatalf("unexpected CPU temperature: %v", s.CPUTempC)
	}
	if s.CPUFreqMHz == nil || *s.CPUFreqMHz != 1700 {
		t.Fatalf("unexpected CPU frequency: %v", s.CPUFreqMHz)
	}
	if s.MemoryPct != 75 || s.MemoryUsed != 750*1024 {
		t.Fatalf("unexpected memory values: %+v", s)
	}
	if s.FanRPM == nil || *s.FanRPM != 3025 {
		t.Fatalf("unexpected fan RPM: %v", s.FanRPM)
	}
	if s.FanPWMPct == nil || *s.FanPWMPct < 29.4 || *s.FanPWMPct > 29.5 {
		t.Fatalf("unexpected fan PWM: %v", s.FanPWMPct)
	}
	if s.Load1 != 1.25 || s.Load5 != 0.8 || s.Load15 != 0.3 {
		t.Fatalf("unexpected load averages: %.2f %.2f %.2f", s.Load1, s.Load5, s.Load15)
	}
	mustWrite(t, filepath.Join(proc, "stat"), "cpu 120 0 60 820 10 0 0 0 0 0\ncpu0 60 0 30 410 5 0 0 0 0 0\ncpu1 60 0 30 410 5 0 0 0 0 0\n")
	s, err = c.Collect(context.Background())
	if err != nil {
		t.Fatalf("second Collect returned error: %v", err)
	}
	if s.CPUUsagePct == nil || *s.CPUUsagePct != 60 {
		t.Fatalf("unexpected total CPU usage: %v", s.CPUUsagePct)
	}
	if len(s.CPUCoreUsagePct) != 2 || s.CPUCoreUsagePct[0] != 60 || s.CPUCoreUsagePct[1] != 60 {
		t.Fatalf("unexpected per-core CPU usage: %v", s.CPUCoreUsagePct)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
