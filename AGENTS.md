# AGENTS.md

Guidance for agents working in this repository. [README.md](README.md) is the user-facing document;
this file records what is non-obvious when changing or debugging the exporter.

## What this is

A single-file (`exporter.go`), stdlib-only Prometheus exporter for Apple Time Machine. macOS only —
it shells out to `tmutil` and `plutil`. One route, `/metrics`, no third-party dependencies; `go.mod`
has no `require` block and should keep it that way.

## Build, test, verify

CI (`.github/workflows/ci.yml`) runs `gofmt -l .`, `go vet ./...`, `go build ./...` and a Unix-socket
smoke test on macOS. Reproduce locally:

```sh
gofmt -l . && go vet ./... && go build -o timemachine-exporter exporter.go
./timemachine-exporter -socket=/tmp/timemachine.sock -port= &
curl -sS --unix-socket /tmp/timemachine.sock http://localhost/metrics
kill %1
```

`-port=` (an *empty* value) is what disables TCP; omitting `-port` leaves it on the default 9155. The
Homebrew formula's `service do` block depends on that empty-value form to make the service
socket-only, so don't change the flag's semantics.

## The failure mode to know about: Full Disk Access

`tmutil latestbackup` reads the Time Machine destination volume, and macOS attributes that access to
the process running the exporter. Without Full Disk Access for the exporter binary the command fails
in a way that is easy to misread:

* The scrape still returns 200, with `timemachine_destination_info` and `timemachine_backup_running`
  present — `tmutil destinationinfo` and `tmutil status` don't touch the volume, so they keep working.
* `timemachine_last_success_timestamp_seconds` and `timemachine_destination_latest_age_seconds` are
  silently absent, and the log says nothing more than `error="exit status 1"`.
* The exporter's own output gives no hint, because both `exec.Command(...).Output()` calls discard
  stderr (see "Known rough edges").

Diagnose with:

```sh
# The grant is per binary: auth_value 2 = allowed, 0 = denied
sqlite3 "/Library/Application Support/com.apple.TCC/TCC.db" \
  "select client, auth_value from access where service='kTCCServiceSystemPolicyAllFiles';"

# A denial appears as an attribution chain naming the exporter as the responsible process
log show --last 30m --info --debug \
  --predicate 'eventMessage CONTAINS "tmutil"' | grep -i denied
```

Grant it in System Settings > Privacy & Security > Full Disk Access. The tap formula's `caveats`
repeat this. The binary is ad-hoc signed (`Identifier=a.out`, true for both `go build exporter.go`
and `go build .`), so the grant is tied to that exact binary — a rebuild or a version upgrade needs
the grant re-issued. This is not a launchd problem: the job's context (label, session type, binary
path under `opt`) makes no difference, and `launchctl kickstart -k` won't help.

## How the data is obtained

| Metric | Source |
| --- | --- |
| `timemachine_last_success_timestamp_seconds` | `tmutil latestbackup`, whose output path ends in a `2006-01-02-150405` directory name parsed in local time |
| `timemachine_destination_latest_age_seconds` | `time() - last_success` |
| `timemachine_destination_info` | `tmutil destinationinfo`; name, kind and ID become labels |
| `timemachine_backup_running` | `tmutil status`, matched on `Running = 1` |

Nothing is cached — the handler runs all of this synchronously per scrape, ~5–6 s here, nearly all of
it in `tmutil`. Scrape intervals shorter than ~15 s will overlap.

## Known rough edges

* **stderr is discarded.** `getLatestBackupFromTmutil` and `getLatestBackupFromPlist` both use
  `cmd.Output()`, so any failure surfaces only as `exit status 1`. Capturing stderr
  (`.CombinedOutput()`, or `cmd.Stderr = &buf`) turns this whole class of diagnosis into a one-line
  answer and is the first thing a 1.0.1 should change.
* **The plist fallback doesn't work.** `plutil -extract Destinations json` exits 1 with "Invalid
  object in plist for JSON format", because destination entries contain `<date>` values that JSON
  cannot represent. So when `tmutil latestbackup` fails the metric is simply absent — there is no
  working fallback, despite what the README's wording implies. A real fallback needs another
  encoding (`plutil -convert xml1` plus XML parsing) or per-key reads.
* **`tmutil status` errors are dropped** (assigned to `_`), so `timemachine_backup_running 0` can be
  a silent default rather than an observed "not running".

## Relationship to the rest of the setup

* **Homebrew tap:** `IngmarStein/homebrew-ingmarstein`, `Formula/timemachine-exporter.rb`. It pins a
  release tarball by `sha256`, builds with `go build *std_go_args exporter.go` (so the formula's
  `depends_on "go" => :build` is load-bearing), and declares the socket-only service. `brew services`
  runs it as the LaunchAgent `sh.brew.timemachine-exporter`, logging to
  `$(brew --prefix)/var/log/timemachine-exporter.log`.
* **Releasing:** tag `vX.Y.Z` here. The tap's `autobump` workflow (every 3 hours, `brew livecheck`
  plus `brew bump-formula-pr --write-only`) opens the version bump, and `publish.yml` builds the
  bottle. Don't hand-edit the formula's `url`/`sha256` in a way the autobump would fight.
* **Homelab consumer:** a Mac Studio's Prometheus scrapes
  `__unix_socket__: /opt/homebrew/var/run/timemachine-exporter.sock` while `__address__` stays
  `localhost:9155`, so the `instance` label — and anything keyed to it — survives the socket/TCP
  choice.
* **Alert:** `TimeMachineBackupStale` on
  `last_over_time(timemachine_destination_latest_age_seconds[7d]) > 2 * 24 * 3600`. That is precisely
  the metric Full Disk Access loss removes, so "the alert stopped firing" can mean the metric
  disappeared rather than the backups recovering.
