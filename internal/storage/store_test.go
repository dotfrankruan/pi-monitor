package storage

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dotfrankruan/pi-monitor/internal/metrics"
)

func TestOpenMigratesOriginalSchema(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE samples (
		timestamp_ms INTEGER PRIMARY KEY,
		cpu_temp_c REAL, cpu_freq_mhz REAL, cpu_usage_pct REAL,
		memory_pct REAL NOT NULL, memory_used_bytes INTEGER NOT NULL, memory_total_bytes INTEGER NOT NULL,
		disk_pct REAL NOT NULL, disk_used_bytes INTEGER NOT NULL, disk_total_bytes INTEGER NOT NULL,
		fan_rpm REAL, fan_pwm_pct REAL, nvme_temp_c REAL,
		load_1 REAL NOT NULL, uptime_seconds REAL NOT NULL
	)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	defer store.Close()
	point := metrics.Sample{Timestamp: time.Now(), CPUCoreUsagePct: []float64{12, 34},
		MemoryTotal: 1, DiskTotal: 1, Load1: 1, Load5: 2, Load15: 3}
	if err := store.AddBatch(context.Background(), []metrics.Sample{point}); err != nil {
		t.Fatalf("write after migration failed: %v", err)
	}
	got, err := store.Query(context.Background(), point.Timestamp.Add(-time.Second), point.Timestamp.Add(time.Second), 10)
	if err != nil || len(got) != 1 || len(got[0].CPUCoreUsagePct) != 2 || got[0].Load15 != 3 {
		t.Fatalf("unexpected migrated data: %+v, err=%v", got, err)
	}
}

func TestSQLiteParquetRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	start := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	temp := 52.5
	points := []metrics.Sample{
		{Timestamp: start.Add(time.Second), CPUTempC: &temp, CPUCoreUsagePct: []float64{10, 20, 30, 40}, MemoryPct: 25, MemoryUsed: 1, MemoryTotal: 4, DiskPct: 10, DiskUsed: 1, DiskTotal: 10, Load1: 1, Load5: 2, Load15: 3},
		{Timestamp: start.Add(2 * time.Second), CPUTempC: &temp, CPUCoreUsagePct: []float64{11, 21, 31, 41}, MemoryPct: 30, MemoryUsed: 1, MemoryTotal: 4, DiskPct: 10, DiskUsed: 1, DiskTotal: 10, Load1: 2, Load5: 3, Load15: 4},
	}
	if err := store.AddBatch(context.Background(), points); err != nil {
		t.Fatal(err)
	}
	names, err := store.ArchiveCompletedWeeks(context.Background(), start.AddDate(0, 0, 8))
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 {
		t.Fatalf("expected one archive, got %v", names)
	}
	if _, err := os.Stat(filepath.Join(dir, "archive", names[0])); err != nil {
		t.Fatal(err)
	}
	count, err := store.Count(context.Background())
	if err != nil || count != 0 {
		t.Fatalf("expected empty SQLite after archive, count=%d err=%v", count, err)
	}
	got, err := store.Query(context.Background(), start, start.Add(time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].CPUTempC == nil || *got[0].CPUTempC != temp {
		t.Fatalf("unexpected round trip: %+v", got)
	}
	if len(got[0].CPUCoreUsagePct) != 4 || got[0].CPUCoreUsagePct[3] != 40 || got[0].Load15 != 3 {
		t.Fatalf("per-core/load history was not preserved: %+v", got[0])
	}
}
