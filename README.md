# Pi Monitor

A small, self-contained Raspberry Pi monitoring service written in Go.

It samples CPU temperature and frequency, total and per-core CPU usage, memory,
disk, fan speed, optional NVMe temperature, 1/5/15-minute load averages and
uptime every 500 ms. Every network interface is sampled for RX/TX totals and
rates, while the dashboard lets the viewer choose which interface to graph.
Recent samples remain in memory and are written to SQLite
in batches. Completed weeks are compacted into Parquet files. The built-in web
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
