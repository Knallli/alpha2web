# alpha2web

Web UI for managing a Sony Alpha camera remotely, built on [alpha2go](https://github.com/Knallli/alpha2go).
Tested on the a6700 only.

- Dashboard with connection status, logs
- Live view (MJPEG)
- Control: shutter (with/without autofocus), movie start/stop, camera properties
- Files: list both card slots, thumbnails, download, delete
- Settings page; environment variables override and lock the fields
- Plugins (compiled in, see `plugin/`), each can add dashboard blocks, a file table, actions, metrics and settings
- Immich exporter (`plugins/immich`): downloads everything on both cards, uploads it to Immich with retries, adds it to an album, stacks RAW+JPEG pairs, optionally deletes the local copy and the shot on the card. Dashboard shows queue, progress, problems and recent uploads; actions "Retry waiting files now" and "Read camera again"; `alpha2web_immich_*` metrics.

## Install (Docker)

```
curl -O https://raw.githubusercontent.com/knallli/alpha2web/main/docker-compose.yml
docker compose up -d
```

Open `http://<host>:8765`. The camera is found automatically on the local network (the container uses `network_mode: host` for that); a fixed address can be set on the Settings page. Settings are kept in the `data` volume.
Optional: copy [.env.example](.env.example) to `.env` (camera address, Access Authentication, `WEBUI_PASSWORD`).
**Set `WEBUI_PASSWORD`**: the UI can fire the shutter and delete files from the card.
Update with `docker compose pull && docker compose up -d`. Images are published to `ghcr.io/knallli/alpha2web` for amd64 and arm64.

Camera setup (Wi-Fi, PC Remote, Access Authentication): see [alpha2go](https://github.com/Knallli/alpha2go/blob/main/docs/camera-setup.md).

## Run without Docker

```
CAMERA_HOST=192.168.1.50 go run ./cmd/alpha2web
```

| Variable | Meaning |
|---|---|
| `CAMERA_HOST` | camera address; empty = find it on the local network |
| `CAMERA_MAC` | optional, speeds up the search |
| `CAMERA_USER`, `CAMERA_PASSWORD`, `CAMERA_FINGERPRINT` | Access Authentication (SSH tunnel) |
| `CAMERA_WAKE_ON_LAN` | `true` sends Wake-on-LAN packets every 30 s while the camera is disconnected (needs a MAC) |
| `CAMERA_WAKE_ON_LAN_ADDR` | target of those packets, default `255.255.255.255:9` |
| `CAMERA_KEEPALIVE` | interval of the idle property poll that keeps the session busy and the camera properties fresh, default `10s`, `0` disables |
| `CAMERA_RECONNECT_INTERVAL` | first delay between connection attempts, default `5s` (backs off to 1 minute) |
| `SHUTDOWN_TIMEOUT` | grace period for open requests on SIGTERM, default `20s` |
| `LOG_LEVEL` | `debug`, `info` (default), `warn`, `error` |
| `LOG_FORMAT` | `json` (default) or `text` |
| `HTTP_ADDR` | listen address, default `:8765` |
| `DATA_DIR` | settings and plugin state, default `data` |
| `IMMICH_URL`, `IMMICH_API_KEY`, `IMMICH_ALBUM` | Immich server, API key (needs `asset.upload`, `album.*`, `stack.create`) and optional album |
| `IMMICH_STACK_RAW_JPEG`, `DELETE_AFTER_UPLOAD`, `IMMICH_DELETE_ON_CAMERA` | stack pairs (default `true`), delete the staged copy (default `true`), delete from the card (default `false`) |
| `SKIP_EXTENSIONS`, `UPLOAD_CONCURRENCY`, `RECONCILE_INTERVAL`, `DOWNLOAD_STALL_TIMEOUT`, `MIN_FREE_SPACE_MB`, `RETRY_BASE_DELAY`, `RETRY_MAX_DELAY` | Immich transfer tuning, see [.env.example](.env.example) |
| `STAGING_DIR` | where downloads wait for the upload, default `<DATA_DIR>/staging` |
| `WEBUI_PASSWORD` | optional HTTP basic auth password for the whole UI (`/healthz`, `/health` (JSON) and `/metrics` (Prometheus) stay open) |

## Limits

- Auto-discovery only searches the local layer-2 network (ARP, SSDP, port scan of the /24). Across routers set the address. With Access Authentication on it scans for the camera's SSH port.
- The camera does not announce new shots to the host, so plugins poll (Immich: every 2 minutes).
- Only one live-view stream at a time.
- HEIF thumbnails and previews are converted to JPEG in pure Go (cached); if that fails the original is served and the browser may not show it.
- Forms carry a per-process CSRF token (it changes on restart); set `WEBUI_PASSWORD` and do not expose the UI to the internet.

## AI disclosure

Large parts of this repository were written with the help of an AI coding assistant (Claude) and reviewed by the author.

## License

Apache-2.0. Contains no Sony code.
