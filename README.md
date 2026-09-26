# timemachine-exporter

A [Prometheus](https://prometheus.io/) exporter for Apple Time Machine on macOS. It reports whether a
backup is currently running, when the last successful backup finished, and how old that backup is —
enough to alert on a Time Machine destination that has silently stopped being written to.

There are no third-party dependencies: everything comes from the standard library plus the `tmutil`
and `plutil` commands that ship with macOS. The exporter therefore only builds and runs on macOS.

## Metrics

| Metric | Type | Description |
| --- | --- | --- |
| `timemachine_backup_running` | gauge | `1` while a backup is in progress, `0` otherwise. |
| `timemachine_last_success_timestamp_seconds` | gauge | Unix timestamp of the most recent backup, as reported by Time Machine. |
| `timemachine_destination_latest_age_seconds` | gauge | Seconds since that backup (`time() - last_success`). |
| `timemachine_destination_info` | gauge | Always `1`; the destination name, kind and ID are labels. |

`timemachine_last_success_timestamp_seconds` and `timemachine_destination_latest_age_seconds` are only
exported once a backup has been found. `timemachine_backup_running` is always exported, which makes it a
usable liveness check for the exporter itself.

The date of the latest backup is read from `tmutil latestbackup`; if that fails or yields nothing, the
exporter falls back to reading `SnapshotDates` out of `/Library/Preferences/com.apple.TimeMachine.plist`.
The destination list comes from `tmutil destinationinfo`. Failures are logged to stderr and simply omit
the affected metrics, so a single failing command never takes the endpoint down.

## Installation

With the [Homebrew tap](https://github.com/IngmarStein/homebrew-ingmarstein):

```sh
brew install ingmarstein/ingmarstein/timemachine-exporter
brew services start timemachine-exporter
```

The service listens on a Unix socket by default, at `$(brew --prefix)/var/run/timemachine-exporter.sock`,
and logs to `$(brew --prefix)/var/log/timemachine-exporter.log`. To stop it again:

```sh
brew services stop timemachine-exporter
```

## Usage

```
timemachine-exporter [-port <port>] [-socket <path>]
```

| Flag | Default | Description |
| --- | --- | --- |
| `-port` | `9155` | TCP port to listen on. Pass `-port=` (empty) to disable TCP. |
| `-socket` | *(empty)* | Unix socket path to listen on, e.g. `/tmp/timemachine.sock`. |

At least one of the two must be set; the exporter exits with status 1 otherwise. Both can be used at
once. `/metrics` is the only route.

```sh
# TCP only (the default)
timemachine-exporter

# Unix socket only — no TCP port is opened
timemachine-exporter -socket=/tmp/timemachine.sock -port=

# Inspect it by hand
curl --unix-socket /tmp/timemachine.sock http://localhost/metrics
```

## Prometheus configuration

Over TCP:

```yaml
scrape_configs:
  - job_name: "timemachine"
    static_configs:
      - targets: ["localhost:9155"]
```

Or over the Unix socket (Prometheus 3.15.0 or newer, which is the first release to support
`__unix_socket__`):

```yaml
scrape_configs:
  - job_name: "timemachine"
    static_configs:
      - targets: ["localhost:9155"]        # still supplies the `instance` label and Host header
        labels:
          __unix_socket__: "/opt/homebrew/var/run/timemachine-exporter.sock"
```

Both forms keep `instance="localhost:9155"`, so switching between them does not invalidate existing
alerts. Note that connecting to the socket needs write permission on it; when Prometheus runs as the
same user as the exporter (the case for a `brew services` install), that is satisfied already.

## Alerting

A backup older than two days means Time Machine has stopped writing — a failing destination or a
volume that can no longer be mounted:

```yaml
groups:
  - name: timemachine
    rules:
      - alert: TimeMachineBackupStale
        expr: last_over_time(timemachine_destination_latest_age_seconds[7d]) > 2 * 24 * 3600
        for: 1h
        annotations:
          summary: "Time Machine backup is {{ $value | humanizeDuration }} old"
```

`last_over_time(...)` rather than the raw metric keeps the alert meaningful across an exporter restart.

## Building from source

```sh
go build -o timemachine-exporter exporter.go
```

## License

[MIT](LICENSE)
