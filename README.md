# alpha2web

Web UI for managing a Sony Alpha camera remotely, built on [alpha2go](https://github.com/Knallli/alpha2go).
Tested on the a6700 only.

- Dashboard with connection status, logs
- Live view (MJPEG)
- Control: shutter (with/without autofocus), movie start/stop, camera properties
- Files: list both card slots, thumbnails, download, delete
- Settings page; environment variables override and lock the fields
- Plugins (compiled in, see `plugin/`). The Immich exporter in `plugins/immich` uploads new files and can delete them from the card afterwards.

## Run

```
CAMERA_HOST=192.168.1.50 go run ./cmd/alpha2web
```

| Variable | Meaning |
|---|---|
| `CAMERA_HOST` | camera address |
| `CAMERA_USER`, `CAMERA_PASSWORD`, `CAMERA_FINGERPRINT` | Access Authentication (SSH tunnel) |
| `HTTP_ADDR` | listen address, default `:8080` |
| `DATA_DIR` | settings and plugin state, default `data` |
| `WEBUI_PASSWORD` | optional HTTP basic auth password for the whole UI |

alpha2go is a local `replace` for now (`../alpha2go`); see the Dockerfile for the build context.

## Limits

- The camera does not announce new shots to the host, so plugins poll (Immich: every 2 minutes).
- Only one live-view stream at a time.
- Thumbnails are HEIF for HEIF shots, which not every browser shows.
- No CSRF tokens: state-changing requests are refused when the browser marks them `cross-site`; set `WEBUI_PASSWORD` and do not expose the UI to the internet.

## AI disclosure

Large parts of this repository were written with the help of an AI coding assistant (Claude) and reviewed by the author.

## License

Apache-2.0. Contains no Sony code.
