package storage

import (
	"context"
	"database/sql"
	"encoding/json"
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
			is_representative INTEGER NOT NULL DEFAULT 0,
            cpu_temp_c REAL, cpu_freq_mhz REAL, cpu_usage_pct REAL,
			cpu_core_usage_json TEXT NOT NULL DEFAULT '[]',
            memory_pct REAL NOT NULL, memory_used_bytes INTEGER NOT NULL, memory_total_bytes INTEGER NOT NULL,
            disk_pct REAL NOT NULL, disk_used_bytes INTEGER NOT NULL, disk_total_bytes INTEGER NOT NULL,
            fan_rpm REAL, fan_pwm_pct REAL, nvme_temp_c REAL,
			load_1 REAL NOT NULL, load_5 REAL NOT NULL DEFAULT 0, load_15 REAL NOT NULL DEFAULT 0,
			uptime_seconds REAL NOT NULL,
			network_json TEXT NOT NULL DEFAULT '{}'
        )`,
		`CREATE INDEX IF NOT EXISTS idx_samples_timestamp ON samples(timestamp_ms)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, fmt.Errorf("initialize SQLite: %w", err)
		}
	}
	if err := ensureColumns(db, map[string]string{
		"is_representative":   "INTEGER NOT NULL DEFAULT 0",
		"cpu_core_usage_json": "TEXT NOT NULL DEFAULT '[]'",
		"load_5":              "REAL NOT NULL DEFAULT 0",
		"load_15":             "REAL NOT NULL DEFAULT 0",
		"network_json":        "TEXT NOT NULL DEFAULT '{}'",
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate SQLite: %w", err)
	}
	return &Store{db: db, archiveDir: archiveDir}, nil
}

func ensureColumns(db *sql.DB, wanted map[string]string) error {
	rows, err := db.Query(`PRAGMA table_info(samples)`)
	if err != nil {
		return err
	}
	existing := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, kind string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		existing[name] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for name, definition := range wanted {
		if !existing[name] {
			if _, err := db.Exec(`ALTER TABLE samples ADD COLUMN ` + name + ` ` + definition); err != nil {
				return err
			}
		}
	}
	return nil
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
	if err := addBatch(ctx, tx, samples); err != nil {
		return err
	}
	return tx.Commit()
}

func addBatch(ctx context.Context, tx *sql.Tx, samples []metrics.Sample) error {
	stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO samples (
		timestamp_ms, is_representative, cpu_temp_c, cpu_freq_mhz, cpu_usage_pct, cpu_core_usage_json,
		memory_pct, memory_used_bytes, memory_total_bytes, disk_pct, disk_used_bytes, disk_total_bytes,
		fan_rpm, fan_pwm_pct, nvme_temp_c, load_1, load_5, load_15, uptime_seconds, network_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, p := range samples {
		cores, marshalErr := json.Marshal(p.CPUCoreUsagePct)
		if marshalErr != nil {
			return marshalErr
		}
		network, marshalErr := json.Marshal(p.Network)
		if marshalErr != nil {
			return marshalErr
		}
		_, err = stmt.ExecContext(ctx,
			p.Timestamp.UnixMilli(), p.Representative, nullable(p.CPUTempC), nullable(p.CPUFreqMHz), nullable(p.CPUUsagePct), string(cores),
			p.MemoryPct, p.MemoryUsed, p.MemoryTotal, p.DiskPct, p.DiskUsed, p.DiskTotal,
			nullable(p.FanRPM), nullable(p.FanPWMPct), nullable(p.NVMeTempC), p.Load1, p.Load5, p.Load15, p.UptimeSec, string(network),
		)
		if err != nil {
			return err
		}
	}
	return nil
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
	rows, err := s.db.QueryContext(ctx, `SELECT timestamp_ms, is_representative, cpu_temp_c, cpu_freq_mhz, cpu_usage_pct, cpu_core_usage_json,
        memory_pct, memory_used_bytes, memory_total_bytes, disk_pct, disk_used_bytes, disk_total_bytes,
		fan_rpm, fan_pwm_pct, nvme_temp_c, load_1, load_5, load_15, uptime_seconds, network_json
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
	var representative bool
	var cores string
	var network string
	var cpuTemp, cpuFreq, cpuUsage, fanRPM, fanPWM, nvmeTemp sql.NullFloat64
	err := row.Scan(&timestamp, &representative, &cpuTemp, &cpuFreq, &cpuUsage, &cores,
		&p.MemoryPct, &p.MemoryUsed, &p.MemoryTotal, &p.DiskPct, &p.DiskUsed, &p.DiskTotal,
		&fanRPM, &fanPWM, &nvmeTemp, &p.Load1, &p.Load5, &p.Load15, &p.UptimeSec, &network)
	if err != nil {
		return p, err
	}
	p.Timestamp = time.UnixMilli(timestamp).UTC()
	p.Representative = representative
	p.CPUTempC, p.CPUFreqMHz, p.CPUUsagePct = pointer(cpuTemp), pointer(cpuFreq), pointer(cpuUsage)
	if err := json.Unmarshal([]byte(cores), &p.CPUCoreUsagePct); err != nil {
		return p, fmt.Errorf("decode per-core usage: %w", err)
	}
	if err := json.Unmarshal([]byte(network), &p.Network); err != nil {
		return p, fmt.Errorf("decode network usage: %w", err)
	}
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
	TimestampMS     int64                            `parquet:"timestamp_ms,timestamp(millisecond)"`
	Representative  bool                             `parquet:"is_representative"`
	CPUTempC        *float64                         `parquet:"cpu_temp_c,optional"`
	CPUFreqMHz      *float64                         `parquet:"cpu_freq_mhz,optional"`
	CPUUsagePct     *float64                         `parquet:"cpu_usage_pct,optional"`
	CPUCoreUsagePct []float64                        `parquet:"cpu_core_usage_pct,list,optional"`
	MemoryPct       float64                          `parquet:"memory_pct"`
	MemoryUsed      int64                            `parquet:"memory_used_bytes"`
	MemoryTotal     int64                            `parquet:"memory_total_bytes"`
	DiskPct         float64                          `parquet:"disk_pct"`
	DiskUsed        int64                            `parquet:"disk_used_bytes"`
	DiskTotal       int64                            `parquet:"disk_total_bytes"`
	FanRPM          *float64                         `parquet:"fan_rpm,optional"`
	FanPWMPct       *float64                         `parquet:"fan_pwm_pct,optional"`
	NVMeTempC       *float64                         `parquet:"nvme_temp_c,optional"`
	Load1           float64                          `parquet:"load_1"`
	Load5           float64                          `parquet:"load_5"`
	Load15          float64                          `parquet:"load_15"`
	UptimeSec       float64                          `parquet:"uptime_seconds"`
	Network         map[string]metrics.NetworkSample `parquet:"network"`
}

func toParquet(p metrics.Sample) parquetSample {
	return parquetSample{TimestampMS: p.Timestamp.UnixMilli(), Representative: p.Representative, CPUTempC: p.CPUTempC,
		CPUFreqMHz: p.CPUFreqMHz, CPUUsagePct: p.CPUUsagePct, CPUCoreUsagePct: p.CPUCoreUsagePct,
		MemoryPct: p.MemoryPct, MemoryUsed: int64(p.MemoryUsed), MemoryTotal: int64(p.MemoryTotal),
		DiskPct: p.DiskPct, DiskUsed: int64(p.DiskUsed), DiskTotal: int64(p.DiskTotal),
		FanRPM: p.FanRPM, FanPWMPct: p.FanPWMPct, NVMeTempC: p.NVMeTempC,
		Load1: p.Load1, Load5: p.Load5, Load15: p.Load15, UptimeSec: p.UptimeSec, Network: p.Network}
}

func (p parquetSample) metric() metrics.Sample {
	return metrics.Sample{Timestamp: time.UnixMilli(p.TimestampMS).UTC(), Representative: p.Representative, CPUTempC: p.CPUTempC,
		CPUFreqMHz: p.CPUFreqMHz, CPUUsagePct: p.CPUUsagePct, CPUCoreUsagePct: p.CPUCoreUsagePct, MemoryPct: p.MemoryPct,
		MemoryUsed: uint64(p.MemoryUsed), MemoryTotal: uint64(p.MemoryTotal), DiskPct: p.DiskPct,
		DiskUsed: uint64(p.DiskUsed), DiskTotal: uint64(p.DiskTotal), FanRPM: p.FanRPM,
		FanPWMPct: p.FanPWMPct, NVMeTempC: p.NVMeTempC, Load1: p.Load1, Load5: p.Load5,
		Load15: p.Load15, UptimeSec: p.UptimeSec, Network: p.Network}
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

// CompactByInterval keeps one representative point for a stable wall-clock
// bucket. If any important metric crosses a conservative threshold, all points
// in that bucket are retained so short spikes remain visible.
func CompactByInterval(points []metrics.Sample, interval time.Duration) []metrics.Sample {
	if len(points) == 0 || interval <= 0 {
		return points
	}
	bucketWidth := interval.Milliseconds()
	if bucketWidth < 1 {
		return points
	}
	out := make([]metrics.Sample, 0, len(points))
	for begin := 0; begin < len(points); {
		bucket := points[begin].Timestamp.UnixMilli() / bucketWidth
		end := begin + 1
		for end < len(points) && points[end].Timestamp.UnixMilli()/bucketWidth == bucket {
			end++
		}
		window := points[begin:end]
		if stableWindow(window) {
			representative := window[len(window)-1]
			representative.Representative = true
			out = append(out, representative)
		} else {
			for _, point := range window {
				point.Representative = false
				out = append(out, point)
			}
		}
		begin = end
	}
	return out
}

func stableWindow(points []metrics.Sample) bool {
	if len(points) < 2 {
		return true
	}
	base := points[0]
	for _, point := range points[1:] {
		if pointerChanged(base.CPUTempC, point.CPUTempC, 3) ||
			pointerChanged(base.CPUFreqMHz, point.CPUFreqMHz, 400) ||
			pointerChanged(base.CPUUsagePct, point.CPUUsagePct, 25) ||
			pointerChanged(base.FanRPM, point.FanRPM, 500) ||
			pointerChanged(base.FanPWMPct, point.FanPWMPct, 15) ||
			pointerChanged(base.NVMeTempC, point.NVMeTempC, 3) ||
			abs(base.MemoryPct-point.MemoryPct) > 2 || abs(base.DiskPct-point.DiskPct) > 1 ||
			abs(base.Load1-point.Load1) > 0.5 || abs(base.Load5-point.Load5) > 0.4 || abs(base.Load15-point.Load15) > 0.3 ||
			coresChanged(base.CPUCoreUsagePct, point.CPUCoreUsagePct) || networkRatesChanged(base.Network, point.Network) {
			return false
		}
	}
	return true
}

func pointerChanged(a, b *float64, threshold float64) bool {
	if a == nil || b == nil {
		return a != nil || b != nil
	}
	return abs(*a-*b) > threshold
}

func coresChanged(a, b []float64) bool {
	if len(a) != len(b) {
		return true
	}
	for i := range a {
		if abs(a[i]-b[i]) > 35 {
			return true
		}
	}
	return false
}

func networkRatesChanged(a, b map[string]metrics.NetworkSample) bool {
	if len(a) != len(b) {
		return true
	}
	for name, previous := range a {
		current, ok := b[name]
		if !ok || rateChanged(previous.RXBytesPerSec, current.RXBytesPerSec) || rateChanged(previous.TXBytesPerSec, current.TXBytesPerSec) {
			return true
		}
	}
	return false
}

func rateChanged(a, b float64) bool {
	difference := abs(a - b)
	baseline := abs(a)
	if abs(b) > baseline {
		baseline = abs(b)
	}
	return difference > 64*1024 && (baseline == 0 || difference/baseline > 0.35)
}

func abs(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

// PreparePersistence downsamples legacy high-resolution rows once when the
// configured durable resolution increases, then returns unused SQLite pages to
// the filesystem. The operation is idempotent across restarts.
func (s *Store) PreparePersistence(ctx context.Context, interval time.Duration) (int64, error) {
	intervalMS := interval.Milliseconds()
	if intervalMS < 1 {
		return 0, errors.New("persistence interval must be at least 1ms")
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		return 0, err
	}
	var existing int64
	err := s.db.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM metadata WHERE key = 'persistence_interval_ms'`).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if existing >= intervalMS {
		return 0, nil
	}
	var oldest, newest sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(timestamp_ms), MAX(timestamp_ms) FROM samples`).Scan(&oldest, &newest); err != nil {
		return 0, err
	}
	var compacted []metrics.Sample
	if oldest.Valid {
		points, err := s.querySQLite(ctx, time.UnixMilli(oldest.Int64), time.UnixMilli(newest.Int64))
		if err != nil {
			return 0, err
		}
		compacted = CompactByInterval(points, interval)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM samples`)
	if err != nil {
		return 0, err
	}
	if err := addBatch(ctx, tx, compacted); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO metadata(key, value) VALUES ('persistence_interval_ms', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, intervalMS); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	original, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	removed := original - int64(len(compacted))
	if removed > 0 {
		if err := s.reclaimSQLiteSpace(ctx); err != nil {
			return removed, err
		}
	}
	return removed, nil
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
	var deleted bool
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
		result, err := s.db.ExecContext(ctx, `DELETE FROM samples WHERE timestamp_ms >= ? AND timestamp_ms < ?`, start.UnixMilli(), end.UnixMilli())
		if err != nil {
			return archived, err
		}
		if count, err := result.RowsAffected(); err != nil {
			return archived, err
		} else if count > 0 {
			deleted = true
		}
		start = end
	}
	if deleted {
		if err := s.reclaimSQLiteSpace(ctx); err != nil {
			return archived, err
		}
	}
	return archived, nil
}

func (s *Store) reclaimSQLiteSpace(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint SQLite before compaction: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("compact SQLite: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("truncate SQLite WAL: %w", err)
	}
	return nil
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
