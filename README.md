# SysBoard

![Go Version](https://img.shields.io/badge/Go-%3E%3D1.21-00ADD8?logo=go&logoColor=white)
![License](https://img.shields.io/badge/License-MIT-green)
![Platform](https://img.shields.io/badge/Platform-Linux-lightgrey?logo=linux)
![Zero Dependencies](https://img.shields.io/badge/Runtime_Dependencies-none-brightgreen)

Self-hosted Linux system dashboard built with Go (stdlib only) and plain vanilla JS. Runs as a single binary with no runtime dependencies and makes zero external network requests on page load. Designed for VPS and home server use.

---

## Features

**Overview tab** -- CPU usage, RAM, CPU temperature (hwmon + thermal_zone fallback), system uptime, network RX/TX rates with total counters, disk usage per mount (auto-detected from `/proc/mounts`, no hardcoded paths).

**Processes tab** -- Top 30 processes by CPU consumption, read directly from `/proc`. Shows PID, CPU%, RAM, user, and state. Filterable by name. No external tools required.

**Services tab** -- All systemd units with live RAM usage (`MemoryCurrent`) and uptime per service. Sortable by name, RAM, or status. Full-text search. Start / stop / restart actions.

**Containers tab** -- Multi-engine container discovery: Docker, Podman, containerd (via nerdctl). Engine detection is automatic. Start / stop / restart actions.

**Minecraft tab** -- Auto-detected. Visible only when the log file path is accessible. Displays last 50 lines of Bedrock server log with live refresh. Send screen commands directly from the UI.

---

## Requirements

- Linux (Ubuntu 20.04+ / Debian 11+ recommended)
- Go >= 1.21 (build-time only; binary has no runtime deps)
- `systemd` (for service management and service metrics)
- `screen` (optional; required only for Minecraft command forwarding)

---

## Repository Structure

```
/
├── main.go                  source
├── go.mod
├── static/
│   └── index.html           frontend (single file, no build step, no CDN dependencies)
├── systemd/
│   └── sysboard.service     systemd unit template with EnvironmentFile
├── .env.example             configuration template
├── .gitignore
└── README.md
```

---

## Deploy

### 1. Clone and configure

```bash
git clone https://github.com/hilmyah/SysBoard.git /opt/sysboard
cd /opt/sysboard

cp .env.example .env
chmod 600 .env
```

Edit `.env` and set `SYSBOARD_TOKEN` to a strong random value:

```bash
openssl rand -hex 32
```

### 2. Build

```bash
go build -ldflags="-s -w" -o sysboard main.go
```

### 3. Install and start service

```bash
cp systemd/sysboard.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now sysboard
```

### 4. Verify

```bash
systemctl status sysboard
journalctl -u sysboard -f

# Test login (replace TOKEN with your value from .env)
curl -s http://localhost:8888/api/login \
  -H 'Content-Type: application/json' \
  -d '{"token":"TOKEN"}'
```

Expected response: `{"ok":true}`

---

## Configuration (.env)

All configuration is read from `/opt/sysboard/.env`. systemd injects these into the process environment via `EnvironmentFile=`.

| Variable | Required | Default | Description |
|---|---|---|---|
| `SYSBOARD_TOKEN` | Yes | none | Static token for all API authentication |
| `SYSBOARD_PORT` | No | `8888` | TCP listen port |
| `SYSBOARD_LOG_PATH` | No | `/var/log/bedrock-server.log` | Bedrock log path; set to empty string to disable Minecraft tab |

The binary will refuse to start if `SYSBOARD_TOKEN` is not set.

### Changing the token

```bash
nano /opt/sysboard/.env
systemctl restart sysboard
```

No rebuild required for configuration changes.

---

## Rebuild After Editing Source

```bash
cd /opt/sysboard
go build -ldflags="-s -w" -o sysboard main.go
systemctl restart sysboard
```

---

## API Reference

All endpoints except `/api/login` require the header `X-Auth-Token: <your token>`.

| Endpoint | Method | Description |
|---|---|---|
| `POST /api/login` | POST | `{"token":"..."}` -- returns `{"ok":true}` |
| `GET /api/metrics` | GET | CPU, RAM, disk, temp, network I/O rates |
| `GET /api/processes` | GET | Top 30 processes by CPU, read from `/proc` |
| `GET /api/services` | GET | All systemd services with RAM + uptime |
| `POST /api/services/action` | POST | `{"service":"name","action":"start\|stop\|restart"}` |
| `GET /api/containers` | GET | Containers from all detected engines |
| `POST /api/containers/action` | POST | `{"id":"...","engine":"docker\|podman\|containerd","action":"..."}` |
| `GET /api/mc/log` | GET | Last 50 lines of Bedrock log |
| `POST /api/mc/command` | POST | `{"command":"..."}` -- forwarded to screen session |

---

## Security Notes

- `.env` contains your token. Keep permissions at `600` and owner `root`.
- `.gitignore` blocks `.env` and the compiled binary from being committed.
- The binary is served over plain HTTP. For public exposure, put it behind a reverse proxy with TLS (nginx, Caddy).
- Service actions execute as `root` (the systemd `User=`). Only expose the dashboard on trusted networks.
- The frontend makes zero external network requests. No CDN, no fonts, no analytics.

---

## Contributing

1. Fork the repository.
2. Create a feature branch: `git checkout -b feature/your-feature`.
3. Keep `go.mod` free of external dependencies -- stdlib only.
4. Test on a real Linux system; this code reads `/proc` and `/sys` directly.
5. Open a pull request with a clear description of what changed and why.

Bug reports: open an issue with the output of `journalctl -u sysboard --no-pager -n 50`.