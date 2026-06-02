# SysBoard — Deploy Guide

## Struktur Direktori

```
/opt/sysboard/
├── main.go
├── go.mod
├── sysboard          ← binary hasil build
└── static/
    └── index.html
```

## Quick Deploy

```bash
# 1. Salin file ke server
scp -r sysboard_v2/* root@hilmy:/opt/sysboard/

# 2. Build binary (butuh Go >= 1.21)
cd /opt/sysboard
go build -ldflags="-s -w" -o sysboard main.go

# 3. Install systemd service
cp sysboard.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now sysboard

# 4. Cek status
systemctl status sysboard
curl -s http://localhost:8888/api/login \
  -H 'Content-Type: application/json' \
  -d '{"token":"<YOUR_STATIC_TOKEN>"}'
```

## Ubah Token

Edit `main.go` baris:
```go
staticToken = "<YOUR_STATIC_TOKEN>"
```
Ganti dengan token kuat, lalu rebuild.

## API Endpoints Baru (v2)

| Endpoint | Metode | Keterangan |
|---|---|---|
| `GET /api/metrics` | GET | CPU, RAM, Disk, Temp, Network I/O |
| `GET /api/services` | GET | Semua systemd service + RAM + Uptime per service |
| `POST /api/services/action` | POST | `{service, action}` |
| `GET /api/containers` | GET | Multi-engine: docker + podman + nerdctl + k3s |
| `GET /api/containers/engines` | GET | Engine detection + versi |
| `POST /api/containers/action` | POST | `{id, engine, action}` |
| `GET /api/plugins` | GET | Daftar plugin + status installed |
| `POST /api/plugins/install` | POST | `{id}` — install plugin via bash |
| `GET /api/mc/log` | GET | 50 baris terakhir bedrock log |
| `POST /api/mc/command` | POST | `{command}` |

## Rebuild Setelah Edit

```bash
cd /opt/sysboard
go build -ldflags="-s -w" -o sysboard main.go
systemctl restart sysboard
```
