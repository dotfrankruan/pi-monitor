package monitor

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/dotfrankruan/pi-monitor/internal/metrics"
	"github.com/dotfrankruan/pi-monitor/internal/storage"
)

type Config struct {
	SampleInterval      time.Duration
	FlushInterval       time.Duration
	PersistenceInterval time.Duration
}

type Monitor struct {
	collector *metrics.Collector
	store     *storage.Store
	config    Config
	log       *slog.Logger

	mu          sync.RWMutex
	current     *metrics.Sample
	pending     []metrics.Sample
	subscribers map[chan metrics.Sample]struct{}
}

func New(collector *metrics.Collector, store *storage.Store, config Config, logger *slog.Logger) *Monitor {
	return &Monitor{collector: collector, store: store, config: config, log: logger,
		subscribers: make(map[chan metrics.Sample]struct{})}
}

func (m *Monitor) Run(ctx context.Context) error {
	if m.config.SampleInterval <= 0 || m.config.FlushInterval <= 0 || m.config.PersistenceInterval <= 0 {
		return errors.New("sample, flush and persistence intervals must be positive")
	}
	if names, err := m.store.ArchiveCompletedWeeks(ctx, time.Now()); err != nil {
		m.log.Error("initial weekly archive failed", "error", err)
	} else if len(names) > 0 {
		m.log.Info("created weekly archives", "files", names)
	}

	sampleTicker := time.NewTicker(m.config.SampleInterval)
	flushTicker := time.NewTicker(m.config.FlushInterval)
	archiveTicker := time.NewTicker(time.Hour)
	defer sampleTicker.Stop()
	defer flushTicker.Stop()
	defer archiveTicker.Stop()

	m.collect(ctx)
	for {
		select {
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			err := m.Flush(flushCtx)
			cancel()
			return err
		case <-sampleTicker.C:
			m.collect(ctx)
		case <-flushTicker.C:
			if err := m.Flush(ctx); err != nil {
				m.log.Error("SQLite flush failed", "error", err)
			}
		case <-archiveTicker.C:
			if names, err := m.store.ArchiveCompletedWeeks(ctx, time.Now()); err != nil {
				m.log.Error("weekly archive failed", "error", err)
			} else if len(names) > 0 {
				m.log.Info("created weekly archives", "files", names)
			}
		}
	}
}

func (m *Monitor) collect(ctx context.Context) {
	point, err := m.collector.Collect(ctx)
	if err != nil {
		m.log.Debug("partial metric sample", "error", err)
	}
	m.mu.Lock()
	m.current = &point
	m.pending = append(m.pending, point)
	for subscriber := range m.subscribers {
		select {
		case subscriber <- point:
		default:
		}
	}
	m.mu.Unlock()
}

func (m *Monitor) Current() (metrics.Sample, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.current == nil {
		return metrics.Sample{}, false
	}
	return *m.current, true
}

func (m *Monitor) PendingCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.pending)
}

func (m *Monitor) Flush(ctx context.Context) error {
	m.mu.Lock()
	batch := m.pending
	m.pending = nil
	m.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	stored := storage.CompactByInterval(batch, m.config.PersistenceInterval)
	if err := m.store.AddBatch(ctx, stored); err != nil {
		m.mu.Lock()
		m.pending = append(batch, m.pending...)
		m.mu.Unlock()
		return err
	}
	m.log.Info("flushed samples to SQLite", "collected", len(batch), "stored", len(stored))
	return nil
}

func (m *Monitor) History(ctx context.Context, from, to time.Time, maxPoints int) ([]metrics.Sample, error) {
	points, err := m.store.Query(ctx, from, to, 0)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	for _, point := range m.pending {
		if !point.Timestamp.Before(from) && !point.Timestamp.After(to) {
			points = append(points, point)
		}
	}
	m.mu.RUnlock()
	sort.Slice(points, func(i, j int) bool { return points[i].Timestamp.Before(points[j].Timestamp) })
	return storage.Downsample(points, maxPoints), nil
}

func (m *Monitor) Subscribe() (<-chan metrics.Sample, func()) {
	channel := make(chan metrics.Sample, 4)
	m.mu.Lock()
	m.subscribers[channel] = struct{}{}
	m.mu.Unlock()
	return channel, func() {
		m.mu.Lock()
		if _, exists := m.subscribers[channel]; exists {
			delete(m.subscribers, channel)
			close(channel)
		}
		m.mu.Unlock()
	}
}
