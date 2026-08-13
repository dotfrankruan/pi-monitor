# Pi Monitor

A small, self-contained Raspberry Pi monitoring service written in Go.

It samples CPU temperature and frequency, total and per-core CPU usage, memory,
disk, fan speed, optional NVMe temperature, 1/5/15-minute load averages and
uptime every 500 ms. Recent samples remain in memory and are written to SQLite
in batches. Completed weeks are compacted into Parquet files. The built-in web
UI shows live values, system information, per-core tables and retro-style
historical charts.

## Quick start

```sh
go build -o pi-monitor ./cmd/pi-monitor
./pi-monitor
```

Open <http://localhost:49152>. Data is stored in `./data` by default.

Run `./pi-monitor -help` for storage and sampling options.

## Data lifecycle

- Samples are collected every `500ms` and kept in memory.
- The in-memory batch is committed to SQLite every `1h` and on clean shutdown.
- Every hour, the service checks whether an ISO week has completed. Completed
  weeks are written atomically to `data/archive/metrics-YYYY-Www.parquet` using
  Zstandard compression, then removed from SQLite.
- History queries merge SQLite, the current memory batch and relevant Parquet
  archives. Responses are downsampled to a bounded number of chart points.

All intervals and paths can be changed with command-line flags. The bundled
systemd unit listens on LAN port `49152` and writes to `/var/lib/pi-monitor`.

## Raspberry Pi sensors

Pi Monitor reads Linux `sysfs` and `/proc` directly. This avoids creating an
external process twice a second. On Raspberry Pi systems, `vcgencmd` is used as
a fallback for CPU temperature and frequency. Fan RPM is discovered from the
`pwmfan` hwmon device when one is available. NVMe telemetry is optional: its
dashboard card is omitted when no NVMe temperature sensor is discovered.

## API

- `GET /api/current` — latest sample
- `GET /api/system` — hostname, model, OS, kernel and root filesystem details
- `GET /api/stream` — live Server-Sent Events stream
- `GET /api/history?from=<RFC3339>&to=<RFC3339>&max_points=1200`
- `GET /healthz` — fails when telemetry is older than 10 seconds
