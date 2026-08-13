# Pi Monitor

A small, self-contained Raspberry Pi monitoring service written in Go.

It samples CPU temperature and frequency, total and per-core CPU usage, memory,
disk, fan speed, optional NVMe temperature, 1/5/15-minute load averages and
uptime every 500 ms. Every network interface is sampled for RX/TX totals and
rates, while the dashboard lets the viewer choose which interface to graph.
Recent samples remain at full resolution in memory. For each 5-second bucket,
SQLite keeps one representative point when the metrics stay stable; if CPU,
temperature, load, fan, memory, disk, or network rates move sharply, every
500 ms point in that bucket is preserved. Completed weeks are compacted into
Parquet files. The built-in web
UI shows live values, system information, per-core tables and retro-style
historical charts.

The history toolbar includes high-resolution 1, 3 and 5 minute views as well as
longer ranges from 15 minutes through 30 days.

## Quick start: Raspberry Pi

```sh
curl -fsSL https://raw.githubusercontent.com/dotfrankruan/pi-monitor/main/install.sh | sudo sh
```

Open `http://<raspberry-pi-address>:49152`. The installer downloads the latest
Linux ARM64 release, verifies its SHA-256 checksum, creates a dedicated
unprivileged `pi-monitor` user, installs and enables the systemd service, and
stores history in `/var/lib/pi-monitor`. Run the same command again to upgrade;
existing history is preserved.

Optional installer settings can be placed before `sudo`:

```sh
curl -fsSL https://raw.githubusercontent.com/dotfrankruan/pi-monitor/main/install.sh | \
  sudo PI_MONITOR_PORT=49153 PI_MONITOR_VERSION=v0.1.0 sh
```

Supported settings are `PI_MONITOR_PORT`, `PI_MONITOR_VERSION`,
`PI_MONITOR_DATA_DIR`, and `PI_MONITOR_INSTALL_DIR`. For a mirror or offline
installation, `PI_MONITOR_RELEASE_BASE` can point to a directory URL containing
the release binary and `SHA256SUMS`, including a local `file:///...` URL.

## Build from source

```sh
go build -o pi-monitor ./cmd/pi-monitor
./pi-monitor
```

Open <http://localhost:49152>. Source builds store data in `./data` by default.

Run `./pi-monitor -help` for storage and sampling options.

## Data lifecycle

- Samples are collected every `500ms` and kept in memory.
- Stable in-memory windows are reduced to one representative sample per `5s`;
  volatile windows retain every sample. The `is_representative` field records
  which case was used. Batches are committed to SQLite every `1h` and on clean
  shutdown, and existing denser rows are compacted once after upgrade.
- Every hour, the service checks whether an ISO week has completed. Completed
  weeks are written atomically to `data/archive/metrics-YYYY-Www.parquet` using
  Zstandard compression, then removed from SQLite. SQLite automatically runs a
  weekly `VACUUM` and WAL truncation after archival so deleted space is returned
  to the filesystem.
- History queries merge SQLite, the current memory batch and relevant Parquet
  archives. Responses are downsampled to a bounded number of chart points.

All intervals and paths can be changed with command-line flags. The bundled
systemd unit listens on LAN port `49152` and writes to `/var/lib/pi-monitor`.

### Adaptive persistence thresholds

Each sample in a wall-clock 5-second window is compared with the first sample
in that window. The window is considered volatile if any delta is greater than
the threshold below. Volatile windows retain every 500 ms sample with
`is_representative=false`; otherwise only the newest sample is stored, with
`is_representative=true`.

| Metric | Volatile when delta is greater than |
| --- | ---: |
| CPU temperature | `3 C` |
| CPU frequency | `400 MHz` |
| Total CPU usage | `25 percentage points` |
| Any CPU core usage | `35 percentage points` |
| Memory usage | `2 percentage points` |
| Root disk usage | `1 percentage point` |
| Fan speed | `500 RPM` |
| Fan PWM | `15 percentage points` |
| NVMe temperature | `3 C` |
| 1-minute load average | `0.5` |
| 5-minute load average | `0.4` |
| 15-minute load average | `0.3` |

For each network interface, RX and TX rates trigger a volatile window only when
the absolute change is greater than `64 KiB/s` **and** the change is greater
than `35%` of the larger rate. An interface appearing or disappearing also
triggers a volatile window. Cumulative RX/TX byte counters and uptime are not
used for volatility because they normally increase continuously. An optional
sensor changing between unavailable and available is considered volatile.

These defaults are intentionally aimed at preserving short, meaningful spikes
while collapsing routine sensor jitter. The durable window size can be changed
with `-persist-interval`; live collection and dashboard updates remain at the
configured `-sample-interval` regardless of the durable resolution.

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
