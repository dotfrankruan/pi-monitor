package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dotfrankruan/pi-monitor/internal/metrics"
	"github.com/parquet-go/parquet-go"
	_ "modernc.org/sqlite"
)

type Store struct {
	db         *sql.DB
	archiveDir string
}

func Open(dataDir string) (*Store, error) {
	archiveDir := filepath.Join(dataDir, "archive")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "metrics.db"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=NORMAL`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS samples (
            timestamp_ms INTEGER PRIMARY KEY,
            cpu_temp_c REAL, cpu_freq_mhz REAL, cpu_usage_pct REAL,
            memory_pct REAL NOT NULL, memory_used_bytes INTEGER NOT NULL, memory_total_bytes INTEGER NOT NULL,
            disk_pct REAL NOT NULL, disk_used_bytes INTEGER NOT NULL, disk_total_bytes INTEGER NOT NULL,
            fan_rpm REAL, fan_pwm_pct REAL, nvme_temp_c REAL,
            load_1 REAL NOT NULL, uptime_seconds REAL NOT NULL
        )`,
		`CREATE INDEX IF NOT EXISTS idx_samples_timestamp ON samples(timestamp_ms)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, fmt.Errorf("initialize SQLite: %w", err)
		}
	}
	return &Store{db: db, archiveDir: archiveDir}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) AddBatch(ctx context.Context, samples []metrics.Sample) error {
	if len(samples) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO samples VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, p := range samples {
		_, err = stmt.ExecContext(ctx,
			p.Timestamp.UnixMilli(), nullable(p.CPUTempC), nullable(p.CPUFreqMHz), nullable(p.CPUUsagePct),
			p.MemoryPct, p.MemoryUsed, p.MemoryTotal, p.DiskPct, p.DiskUsed, p.DiskTotal,
			nullable(p.FanRPM), nullable(p.FanPWMPct), nullable(p.NVMeTempC), p.Load1, p.UptimeSec,
		)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func nullable(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func (s *Store) Query(ctx context.Context, from, to time.Time, maxPoints int) ([]metrics.Sample, error) {
	var all []metrics.Sample
	database, err := s.querySQLite(ctx, from, to)
	if err != nil {
		return nil, err
	}
	all = append(all, database...)
	archives, err := filepath.Glob(filepath.Join(s.archiveDir, "*.parquet"))
	if err != nil {
		return nil, err
	}
	for _, path := range archives {
		points, err := readParquetRange(path, from, to)
		if err != nil {
			return nil, fmt.Errorf("read archive %s: %w", filepath.Base(path), err)
		}
		all = append(all, points...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Timestamp.Before(all[j].Timestamp) })
	all = deduplicate(all)
	return Downsample(all, maxPoints), nil
}

func (s *Store) querySQLite(ctx context.Context, from, to time.Time) ([]metrics.Sample, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT timestamp_ms, cpu_temp_c, cpu_freq_mhz, cpu_usage_pct,
        memory_pct, memory_used_bytes, memory_total_bytes, disk_pct, disk_used_bytes, disk_total_bytes,
        fan_rpm, fan_pwm_pct, nvme_temp_c, load_1, uptime_seconds
        FROM samples WHERE timestamp_ms >= ? AND timestamp_ms <= ? ORDER BY timestamp_ms`, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []metrics.Sample
	for rows.Next() {
		p, err := scanSample(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(...any) error }

func scanSample(row scanner) (metrics.Sample, error) {
	var p metrics.Sample
	var timestamp int64
	var cpuTemp, cpuFreq, cpuUsage, fanRPM, fanPWM, nvmeTemp sql.NullFloat64
	err := row.Scan(&timestamp, &cpuTemp, &cpuFreq, &cpuUsage,
		&p.MemoryPct, &p.MemoryUsed, &p.MemoryTotal, &p.DiskPct, &p.DiskUsed, &p.DiskTotal,
		&fanRPM, &fanPWM, &nvmeTemp, &p.Load1, &p.UptimeSec)
	if err != nil {
		return p, err
	}
	p.Timestamp = time.UnixMilli(timestamp).UTC()
	p.CPUTempC, p.CPUFreqMHz, p.CPUUsagePct = pointer(cpuTemp), pointer(cpuFreq), pointer(cpuUsage)
	p.FanRPM, p.FanPWMPct, p.NVMeTempC = pointer(fanRPM), pointer(fanPWM), pointer(nvmeTemp)
	return p, nil
}

func pointer(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	v := value.Float64
	return &v
}

type parquetSample struct {
	TimestampMS int64    `parquet:"timestamp_ms,timestamp(millisecond)"`
	CPUTempC    *float64 `parquet:"cpu_temp_c,optional"`
	CPUFreqMHz  *float64 `parquet:"cpu_freq_mhz,optional"`
	CPUUsagePct *float64 `parquet:"cpu_usage_pct,optional"`
	MemoryPct   float64  `parquet:"memory_pct"`
	MemoryUsed  int64    `parquet:"memory_used_bytes"`
	MemoryTotal int64    `parquet:"memory_total_bytes"`
	DiskPct     float64  `parquet:"disk_pct"`
	DiskUsed    int64    `parquet:"disk_used_bytes"`
	DiskTotal   int64    `parquet:"disk_total_bytes"`
	FanRPM      *float64 `parquet:"fan_rpm,optional"`
	FanPWMPct   *float64 `parquet:"fan_pwm_pct,optional"`
	NVMeTempC   *float64 `parquet:"nvme_temp_c,optional"`
	Load1       float64  `parquet:"load_1"`
	UptimeSec   float64  `parquet:"uptime_seconds"`
}

func toParquet(p metrics.Sample) parquetSample {
	return parquetSample{p.Timestamp.UnixMilli(), p.CPUTempC, p.CPUFreqMHz, p.CPUUsagePct,
		p.MemoryPct, int64(p.MemoryUsed), int64(p.MemoryTotal), p.DiskPct, int64(p.DiskUsed), int64(p.DiskTotal),
		p.FanRPM, p.FanPWMPct, p.NVMeTempC, p.Load1, p.UptimeSec}
}

func (p parquetSample) metric() metrics.Sample {
	return metrics.Sample{Timestamp: time.UnixMilli(p.TimestampMS).UTC(), CPUTempC: p.CPUTempC,
		CPUFreqMHz: p.CPUFreqMHz, CPUUsagePct: p.CPUUsagePct, MemoryPct: p.MemoryPct,
		MemoryUsed: uint64(p.MemoryUsed), MemoryTotal: uint64(p.MemoryTotal), DiskPct: p.DiskPct,
		DiskUsed: uint64(p.DiskUsed), DiskTotal: uint64(p.DiskTotal), FanRPM: p.FanRPM,
		FanPWMPct: p.FanPWMPct, NVMeTempC: p.NVMeTempC, Load1: p.Load1, UptimeSec: p.UptimeSec}
}

func readParquetRange(path string, from, to time.Time) ([]metrics.Sample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := parquet.NewGenericReader[parquetSample](f)
	defer r.Close()
	buffer := make([]parquetSample, 4096)
	var out []metrics.Sample
	for {
		n, readErr := r.Read(buffer)
		for _, p := range buffer[:n] {
			point := p.metric()
			if !point.Timestamp.Before(from) && !point.Timestamp.After(to) {
				out = append(out, point)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	return out, nil
}

func deduplicate(points []metrics.Sample) []metrics.Sample {
	if len(points) < 2 {
		return points
	}
	out := points[:1]
	for _, point := range points[1:] {
		if point.Timestamp.Equal(out[len(out)-1].Timestamp) {
			out[len(out)-1] = point
			continue
		}
		out = append(out, point)
	}
	return out
}

func Downsample(points []metrics.Sample, max int) []metrics.Sample {
	if max <= 0 || len(points) <= max {
		return points
	}
	// Select evenly spaced source points. This keeps endpoint timing exact and
	// bounds JSON size even for multi-week views.
	out := make([]metrics.Sample, max)
	last := len(points) - 1
	for i := range out {
		out[i] = points[i*last/(max-1)]
	}
	return out
}

func (s *Store) Count(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM samples`).Scan(&count)
	return count, err
}

func ArchiveFilename(start time.Time) string {
	year, week := start.ISOWeek()
	return fmt.Sprintf("metrics-%04d-W%02d.parquet", year, week)
}

func weekStart(t time.Time) time.Time {
	u := t.UTC()
	weekday := (int(u.Weekday()) + 6) % 7
	return time.Date(u.Year(), u.Month(), u.Day()-weekday, 0, 0, 0, 0, time.UTC)
}

func (s *Store) ArchiveCompletedWeeks(ctx context.Context, now time.Time) ([]string, error) {
	var oldest sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(timestamp_ms) FROM samples`).Scan(&oldest); err != nil {
		return nil, err
	}
	if !oldest.Valid {
		return nil, nil
	}
	currentWeek := weekStart(now)
	start := weekStart(time.UnixMilli(oldest.Int64))
	var archived []string
	for start.Before(currentWeek) {
		end := start.AddDate(0, 0, 7)
		name := ArchiveFilename(start)
		path := filepath.Join(s.archiveDir, name)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			points, queryErr := s.querySQLite(ctx, start, end.Add(-time.Millisecond))
			if queryErr != nil {
				return archived, queryErr
			}
			if len(points) > 0 {
				if err := writeParquetAtomic(path, points); err != nil {
					return archived, err
				}
				archived = append(archived, name)
			}
		} else if err != nil {
			return archived, err
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM samples WHERE timestamp_ms >= ? AND timestamp_ms < ?`, start.UnixMilli(), end.UnixMilli()); err != nil {
			return archived, err
		}
		start = end
	}
	return archived, nil
}

func writeParquetAtomic(path string, points []metrics.Sample) (err error) {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer func() {
		f.Close()
		if err != nil {
			os.Remove(tmp)
		}
	}()
	w := parquet.NewGenericWriter[parquetSample](f, parquet.Compression(&parquet.Zstd))
	batch := make([]parquetSample, len(points))
	for i, point := range points {
		batch[i] = toParquet(point)
	}
	if _, err = w.Write(batch); err != nil {
		return err
	}
	if err = w.Close(); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func ListArchives(dataDir string) ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(dataDir, "archive", "*.parquet"))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(paths))
	for _, path := range paths {
		names = append(names, strings.TrimSuffix(filepath.Base(path), ".parquet"))
	}
	return names, nil
}
