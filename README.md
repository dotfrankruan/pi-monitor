# Pi Monitor

A small, self-contained Raspberry Pi monitoring service written in Go.

It samples CPU temperature and frequency, memory, disk, fan speed, NVMe
temperature, load and uptime every 500 ms. Recent samples remain in memory and
are written to SQLite in batches. Completed weeks are compacted into Parquet
files. The built-in web UI shows live values and retro-style historical charts.

## Quick start

```sh
go build -o pi-monitor ./cmd/pi-monitor
./pi-monitor
```

Open <http://localhost:49152>. Data is stored in `./data` by default.

Run `./pi-monitor -help` for storage and sampling options.

