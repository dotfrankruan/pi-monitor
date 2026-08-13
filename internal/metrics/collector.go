package metrics

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Sample is one point in the monitoring time series. Optional sensors are nil.
type Sample struct {
	Timestamp       time.Time `json:"timestamp" parquet:"timestamp,timestamp(millisecond)"`
	CPUTempC        *float64  `json:"cpu_temp_c,omitempty" parquet:"cpu_temp_c,optional"`
	CPUFreqMHz      *float64  `json:"cpu_freq_mhz,omitempty" parquet:"cpu_freq_mhz,optional"`
	CPUUsagePct     *float64  `json:"cpu_usage_pct,omitempty" parquet:"cpu_usage_pct,optional"`
	CPUCoreUsagePct []float64 `json:"cpu_core_usage_pct,omitempty" parquet:"cpu_core_usage_pct,list,optional"`
	MemoryPct       float64   `json:"memory_pct" parquet:"memory_pct"`
	MemoryUsed      uint64    `json:"memory_used_bytes" parquet:"memory_used_bytes"`
	MemoryTotal     uint64    `json:"memory_total_bytes" parquet:"memory_total_bytes"`
	DiskPct         float64   `json:"disk_pct" parquet:"disk_pct"`
	DiskUsed        uint64    `json:"disk_used_bytes" parquet:"disk_used_bytes"`
	DiskTotal       uint64    `json:"disk_total_bytes" parquet:"disk_total_bytes"`
	FanRPM          *float64  `json:"fan_rpm,omitempty" parquet:"fan_rpm,optional"`
	FanPWMPct       *float64  `json:"fan_pwm_pct,omitempty" parquet:"fan_pwm_pct,optional"`
	NVMeTempC       *float64  `json:"nvme_temp_c,omitempty" parquet:"nvme_temp_c,optional"`
	Load1           float64   `json:"load_1" parquet:"load_1"`
	Load5           float64   `json:"load_5" parquet:"load_5"`
	Load15          float64   `json:"load_15" parquet:"load_15"`
	UptimeSec       float64   `json:"uptime_seconds" parquet:"uptime_seconds"`
}

func (s Sample) MarshalJSON() ([]byte, error) {
	type alias Sample
	return json.Marshal(struct {
		alias
		Timestamp string `json:"timestamp"`
	}{alias: alias(s), Timestamp: s.Timestamp.UTC().Format(time.RFC3339Nano)})
}

type cpuTimes struct{ idle, total uint64 }

type Collector struct {
	procRoot string
	sysRoot  string
	diskPath string

	cpuTempPath  string
	nvmeTempPath string
	fanRPMPath   string
	fanPWMPath   string
	freqPaths    []string

	mu       sync.Mutex
	previous map[string]cpuTimes
}

func NewCollector(procRoot, sysRoot, diskPath string) *Collector {
	if procRoot == "" {
		procRoot = "/proc"
	}
	if sysRoot == "" {
		sysRoot = "/sys"
	}
	if diskPath == "" {
		diskPath = "/"
	}
	c := &Collector{procRoot: procRoot, sysRoot: sysRoot, diskPath: diskPath}
	c.discoverSensors()
	return c
}

func (c *Collector) discoverSensors() {
	hwmons, _ := filepath.Glob(filepath.Join(c.sysRoot, "class/hwmon/hwmon*"))
	for _, dir := range hwmons {
		name, err := readText(filepath.Join(dir, "name"))
		if err != nil {
			continue
		}
		switch strings.TrimSpace(name) {
		case "cpu_thermal":
			c.cpuTempPath = filepath.Join(dir, "temp1_input")
		case "nvme":
			c.nvmeTempPath = filepath.Join(dir, "temp1_input")
		case "pwmfan":
			c.fanRPMPath = filepath.Join(dir, "fan1_input")
			c.fanPWMPath = filepath.Join(dir, "pwm1")
		}
	}
	if c.cpuTempPath == "" {
		c.cpuTempPath = filepath.Join(c.sysRoot, "class/thermal/thermal_zone0/temp")
	}
	c.freqPaths, _ = filepath.Glob(filepath.Join(c.sysRoot, "devices/system/cpu/cpufreq/policy*/scaling_cur_freq"))
}

func (c *Collector) Collect(ctx context.Context) (Sample, error) {
	s := Sample{Timestamp: time.Now().UTC()}
	var problems []error

	if v, err := readNumber(c.cpuTempPath, 1000); err == nil {
		s.CPUTempC = &v
	} else if v, fallbackErr := vcgenNumber(ctx, "measure_temp", "temp=", "'C", 1); fallbackErr == nil {
		s.CPUTempC = &v
	} else {
		problems = append(problems, fmt.Errorf("CPU temperature: %w", err))
	}

	if v, err := c.cpuFrequency(); err == nil {
		s.CPUFreqMHz = &v
	} else if v, fallbackErr := vcgenNumber(ctx, "measure_clock", "frequency(0)=", "", 1_000_000); fallbackErr == nil {
		s.CPUFreqMHz = &v
	} else {
		problems = append(problems, fmt.Errorf("CPU frequency: %w", err))
	}

	if total, cores, err := c.cpuUsage(); err == nil {
		s.CPUUsagePct = total
		s.CPUCoreUsagePct = cores
	}
	if used, total, err := c.memory(); err == nil {
		s.MemoryUsed, s.MemoryTotal = used, total
		if total > 0 {
			s.MemoryPct = float64(used) * 100 / float64(total)
		}
	} else {
		problems = append(problems, err)
	}
	if used, total, err := diskUsage(c.diskPath); err == nil {
		s.DiskUsed, s.DiskTotal = used, total
		if total > 0 {
			s.DiskPct = float64(used) * 100 / float64(total)
		}
	} else {
		problems = append(problems, err)
	}
	if v, err := readNumber(c.fanRPMPath, 1); err == nil {
		s.FanRPM = &v
	}
	if v, err := readNumber(c.fanPWMPath, 255.0/100.0); err == nil {
		s.FanPWMPct = &v
	}
	if v, err := readNumber(c.nvmeTempPath, 1000); err == nil {
		s.NVMeTempC = &v
	}
	if values, err := firstNumbers(filepath.Join(c.procRoot, "loadavg"), 3); err == nil {
		s.Load1, s.Load5, s.Load15 = values[0], values[1], values[2]
	}
	if v, err := firstNumber(filepath.Join(c.procRoot, "uptime")); err == nil {
		s.UptimeSec = v
	}

	// Optional sensors are allowed to be absent. Core memory and disk failures
	// are returned while the partially collected sample remains useful.
	return s, errors.Join(problems...)
}

func (c *Collector) cpuFrequency() (float64, error) {
	var sum float64
	var count int
	for _, path := range c.freqPaths {
		v, err := readNumber(path, 1000)
		if err == nil {
			sum += v
			count++
		}
	}
	if count == 0 {
		return 0, errors.New("no cpufreq policy available")
	}
	return sum / float64(count), nil
}

func (c *Collector) memory() (used, total uint64, err error) {
	f, err := os.Open(filepath.Join(c.procRoot, "meminfo"))
	if err != nil {
		return 0, 0, fmt.Errorf("memory: %w", err)
	}
	defer f.Close()
	values := map[string]uint64{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		v, parseErr := strconv.ParseUint(fields[1], 10, 64)
		if parseErr == nil {
			values[strings.TrimSuffix(fields[0], ":")] = v * 1024
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, fmt.Errorf("memory: %w", err)
	}
	total = values["MemTotal"]
	available := values["MemAvailable"]
	if total == 0 || available > total {
		return 0, 0, errors.New("memory: invalid MemTotal/MemAvailable")
	}
	return total - available, total, nil
}

func (c *Collector) cpuUsage() (*float64, []float64, error) {
	f, err := os.Open(filepath.Join(c.procRoot, "stat"))
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	current := make(map[string]cpuTimes)
	var order []string
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 || (fields[0] != "cpu" && !strings.HasPrefix(fields[0], "cpu")) {
			break
		}
		var values []uint64
		for _, field := range fields[1:] {
			v, parseErr := strconv.ParseUint(field, 10, 64)
			if parseErr != nil {
				return nil, nil, parseErr
			}
			values = append(values, v)
		}
		var total uint64
		for _, v := range values {
			total += v
		}
		idle := values[3]
		if len(values) > 4 {
			idle += values[4]
		}
		current[fields[0]] = cpuTimes{idle: idle, total: total}
		order = append(order, fields[0])
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, err
	}
	if len(order) == 0 || order[0] != "cpu" {
		return nil, nil, errors.New("invalid /proc/stat CPU rows")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	previous := c.previous
	c.previous = current
	if previous == nil {
		return nil, nil, nil
	}
	usage := func(name string) *float64 {
		before, ok := previous[name]
		now := current[name]
		if !ok || now.total <= before.total || now.idle < before.idle {
			return nil
		}
		deltaTotal := now.total - before.total
		value := (1 - float64(now.idle-before.idle)/float64(deltaTotal)) * 100
		return &value
	}
	total := usage("cpu")
	cores := make([]float64, 0, len(order)-1)
	for _, name := range order[1:] {
		if value := usage(name); value != nil {
			cores = append(cores, *value)
		}
	}
	return total, cores, nil
}

func diskUsage(path string) (used, total uint64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, fmt.Errorf("disk %s: %w", path, err)
	}
	blockSize := uint64(stat.Bsize)
	total = stat.Blocks * blockSize
	available := stat.Bavail * blockSize
	if available > total {
		return 0, 0, errors.New("disk: invalid filesystem values")
	}
	return total - available, total, nil
}

func readText(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}

func readNumber(path string, divisor float64) (float64, error) {
	if path == "" {
		return 0, errors.New("sensor not found")
	}
	text, err := readText(path)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil {
		return 0, err
	}
	return v / divisor, nil
}

func firstNumber(path string) (float64, error) {
	values, err := firstNumbers(path, 1)
	if err != nil {
		return 0, err
	}
	return values[0], nil
}

func firstNumbers(path string, count int) ([]float64, error) {
	text, err := readText(path)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(text)
	if len(fields) < count {
		return nil, errors.New("missing number")
	}
	values := make([]float64, count)
	for i := range values {
		values[i], err = strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return nil, err
		}
	}
	return values, nil
}

func vcgenNumber(ctx context.Context, argument, prefix, suffix string, divisor float64) (float64, error) {
	commandCtx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(commandCtx, "vcgencmd", argument).Output()
	if err != nil {
		return 0, err
	}
	text := strings.TrimSpace(string(out))
	text = strings.TrimPrefix(text, prefix)
	text = strings.TrimSuffix(text, suffix)
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, err
	}
	return v / divisor, nil
}
