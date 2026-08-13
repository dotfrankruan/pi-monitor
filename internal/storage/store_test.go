package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dotfrankruan/pi-monitor/internal/metrics"
)

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
		{Timestamp: start.Add(time.Second), CPUTempC: &temp, MemoryPct: 25, MemoryUsed: 1, MemoryTotal: 4, DiskPct: 10, DiskUsed: 1, DiskTotal: 10},
		{Timestamp: start.Add(2 * time.Second), CPUTempC: &temp, MemoryPct: 30, MemoryUsed: 1, MemoryTotal: 4, DiskPct: 10, DiskUsed: 1, DiskTotal: 10},
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
}
