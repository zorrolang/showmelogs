# showmelogs（smlog）

[中文文档见 README.zh-CN.md](README.zh-CN.md)

A lightweight test-result archive and retrieval service (Go) designed for
personal use on Linux test machines running `xfstests`, `LTP`, `fio`,
`blktests` and similar suites:

- **Low memory footprint**: typical usage well under 20 MB (streaming ZIP,
  no directory cache, virtual scrolling, idle memory reaper)
- **Zero third-party dependencies**: Go standard library only; the frontend
  is embedded in a single static binary — copy it to the test machine and run
- **Cross-host access over LAN**: listens on `0.0.0.0`; open
  `http://<test-machine-ip>:8080` in a browser

## Features

| Feature | Description |
| --- | --- |
| Generic browsing | Arbitrarily deep directory browsing, breadcrumb navigation, sort by name/size/time |
| Tag badges | Automatic `FAIL` / `PASS` / `xfstests` / `LTP` / `fio` / `blktests` badges derived from directory/file name keywords (case-insensitive) |
| Mode A: name/whiteboard search | Concurrent in-memory tree traversal; space-separated multi-keyword matching (e.g. `6.12 ext4`) against entry names and optional per-directory `.whiteboard` files; matched keywords are highlighted in results; millisecond responses; max 200 results |
| Whiteboard files | Drop an optional free-form text file named `.whiteboard` (suite/kernel/OS/params, any content) into a result directory: it shows in a wrapped box under the directory row (toggle button `📝 whiteboard`) and is searchable in name mode |
| Mode B: content search | Concurrent scan of plain-text logs (e.g. `Call Trace`, `BUG:`, `FAILED`), returning file path + line number + text snippet; skips hidden files, binary files and files >20 MB; max 5 matching lines per file, 100 files total |
| Regex search | A `.* 正则` toggle upgrades either mode to RE2 patterns, case-insensitive by default — safe against catastrophic backtracking by construction. Name mode splits the query on whitespace into multiple ANDed patterns (each may hit the name or the whiteboard); content mode treats it as one pattern per line |
| Split-screen preview | Click a text file to preview it with highlighting in the right pane; files >512 KB are truncated to "first 128 KB + last 384 KB" with a one-click full load (up to 10 MB); binary files prompt for download |
| Zip download | Single files served as-is (HTTP Range / resume support); directories packed as streaming ZIPs on the fly with `archive/zip` — no temp files |
| Security | Strictly read-only (no delete/modify endpoints), path-traversal blocked (`../` always 403), symlink-escape blocked, optional HTTP Basic Auth |

## Quick start

```bash
# Build (Go 1.21+, stdlib only)
make build            # or: go build -o smlog .

# Cross-compile for common test-machine architectures
make build-linux-amd64
make build-linux-arm64

# Prepare the results directory and start
mkdir -p /data/test_results
./smlog -root /data/test_results -addr 0.0.0.0:8080
```

Open `http://<machine-ip>:8080` in a browser.

> The default root is `/data/test_results`; startup fails with a clear message
> if it does not exist. Point `-root` (or env `SMLOG_ROOT`) at any directory.

## Flags and environment variables

Every flag can take its default from the same-named environment variable
(prefix `SMLOG_`, uppercase); flags win when both are set.

| Flag | Env var | Default | Description |
| --- | --- | --- | --- |
| `-root` | `SMLOG_ROOT` | `/data/test_results` | Test results root directory |
| `-addr` | `SMLOG_ADDR` | `0.0.0.0:8080` | Listen address and port |
| `-user` | `SMLOG_USER` | empty | Basic Auth username (defaults to `admin` when `-pass` is set) |
| `-pass` | `SMLOG_PASS` | empty | Basic Auth password; **non-empty enables site-wide auth**, empty means open LAN access |
| `-dev` | `SMLOG_DEV` | `false` | Dev mode: re-read `web/index.html` from disk on every request for live UI tuning (run from the project root) |
| `-follow-symlinks` | `SMLOG_FOLLOW_SYMLINKS` | `true` | Resolve symlinks inside the root; the resolved target must stay inside the root, otherwise 403 |

Examples:

```bash
# Open LAN access
./smlog -root /data/test_results

# Enable Basic Auth
./smlog -root /data/test_results -user admin -pass 's3cret!'

# Configure via environment variables
SMLOG_ROOT=/data/test_results SMLOG_ADDR=0.0.0.0:8080 \
SMLOG_PASS='s3cret!' ./smlog
```

## Directory layout and naming suggestions

Keep the root at `/data/test_results` and name each run
`<date>_<suite>_<version>_<fs>`, e.g.:

```text
/data/test_results/
├── 20261117_xfstests_6.12_xfs/      # today's xfstests run on 6.12 + xfs
├── 20261118_xfstests_6.12_ext4/
├── 20261119_ltp_6.12/
├── 20261120_fio_nvme/
└── 20261121_blktests_sda/
```

Names containing `fail` / `.bad` / `error` get a red `FAIL` badge, `pass` gets
a green `PASS` badge, and `xfstests` / `ltp` / `fio` / `blktests` show their
respective badges automatically.

### Whiteboard metadata files

Keep directory names short (e.g. `20261117_nfs_rmapbt`) and put the rest of
the run information into a free-form `.whiteboard` text file inside the run
directory:

```text
# /data/test_results/20261117_nfs_rmapbt/.whiteboard
suite: xfstests
kernel: 6.12.113-3.hs.490.el10.x86_64
os: rockylinux10
hammerspace: 5.3.1-1224
params: rmapbt=1 localio=no cache/read=2 cache/write=4
```

- The content shows in a wrapped box under the directory row (toggle with the
  `📝 whiteboard` button in the list header, persisted).
- Name-mode search matches the concatenation of the entry name and its
  whiteboard, so `rmapbt=1` or `hammerspace` finds the run even though the
  directory name is short; matched keywords are highlighted in the results
  and the whiteboard is always shown there.
- The file is optional and never listed as an entry itself; content beyond
  4 KB is truncated for display (the file on disk is untouched).

### Merging scattered test outputs with symlinks

`xfstests`' `results/` and `LTP`'s `output/` usually live in different places;
just symlink them into the root (`-follow-symlinks` is on by default):

```bash
ln -s /root/xfstests/results/20261117_run /data/test_results/20261117_xfstests_6.12_xfs
ln -s /opt/ltp/output/20261119_run      /data/test_results/20261119_ltp_6.12
```

Security policy:

- The resolved symlink target **must stay inside the root**; links pointing
  outside (e.g. `/etc`) are hidden from listings and return `403` on direct
  access;
- Symlink cycles (`a/link -> a`) are guarded by a visited set — search never
  loops forever;
- With `-follow-symlinks=false`, all symlinks are hidden from listings and
  search, and access is denied with `403`.

## Firewall (cross-host access)

```bash
# firewalld (RHEL/Rocky/CentOS/Fedora)
sudo firewall-cmd --permanent --add-port=8080/tcp
sudo firewall-cmd --reload

# ufw (Debian/Ubuntu)
sudo ufw allow 8080/tcp
```

> Note: HTTP Basic Auth is cleartext (unencrypted) and is only meant for
> trusted LANs; for public access use an SSH tunnel or a TLS reverse proxy.

## Systemd deployment

```bash
sudo make install                        # installs /usr/local/bin/smlog
sudo cp smlog.service /etc/systemd/system/
# edit the unit: User= and the directory/port/credentials in ExecStart=
sudo systemctl daemon-reload
sudo systemctl enable --now smlog
sudo systemctl status smlog
```

The unit template ships with read-only hardening (`ProtectSystem=strict` etc.)
matching the service's read-only design.

## API overview

All endpoints accept `GET` only; errors are returned as
`{"error": "..."}` with `400/401/403/404/500`.

| Endpoint | Description |
| --- | --- |
| `GET /` | Embedded single-page frontend |
| `GET /api/list?path=<rel>` | Directory listing (dirs first, mtime desc) with `current_path` / `parent_path` / `items[]` (incl. auto `tags` and optional `whiteboard` for dirs) |
| `GET /api/search?q=<kw>&mode=name\|content&scope=<rel>&regex=0\|1` | Dual-mode search; `q` is space-separated keywords (all must match, case-insensitive); name mode also matches per-dir `.whiteboard` content; with `regex=1`, name mode splits `q` on whitespace into ANDed RE2 patterns (each may hit name or whiteboard) while content mode treats `q` as one pattern per line — both linear-time and case-insensitive by default (`(?-i)` to opt out locally); empty `scope` searches the whole root |
| `GET /api/preview?path=<rel>&full=<bool>` | Text preview; `is_binary` flags binaries; >512 KB returns truncated content with `truncated`; `full=1` loads complete content (up to 10 MB) |
| `GET /api/download?path=<rel>` | File: served as-is (Range supported); directory: streaming ZIP |
| `GET /api/disk` | Disk usage of the filesystem holding the root (`total` / `free` / `avail` / `used`, via `syscall.Statfs`); shown as an avail/total bar at the bottom of the left panel |

## Developing and tuning the UI

```bash
# -dev re-reads web/index.html from disk on every refresh — edit CSS/JS and
# just reload the browser (run from the project root)
./smlog -root ./test_results -addr 127.0.0.1:8080 -dev
```

The frontend has zero external dependencies (no CDN) and works fully offline.

## Tests

```bash
make test          # go test -v ./...
```

Coverage: path-traversal blocking (`../../etc/passwd` → 403), listing order and
tag detection, dual-mode search (multi-keyword / line-number extraction /
skipping binary, hidden and oversized files), large-file truncated previews,
streaming ZIP integrity, Basic Auth on/off, symlink escape and cycle guards.

## Resource usage notes

- Listings and search traverse the tree **on demand** — no memory cache;
- ZIP packaging is **streamed** — no temp files on disk;
- The viewer uses **virtual scrolling** — even a 10 MB log only renders the
  visible window;
- The live heap is typically just a few MB (visible in GC traces); a built-in
  **idle memory reaper** (after 2 minutes idle, `FreeOSMemory` every 5 minutes)
  keeps reported RSS close to the real footprint. Together with
  `GODEBUG=madvdontneed=1` (already set in the systemd template), steady-state
  RSS stays below 20 MB.
  > Note: without that env var Go defaults to `MADV_FREE`, which keeps freed
  > pages counted in RSS while the system is idle (the number looks inflated
  > but the kernel reclaims them instantly under pressure).
