# alpha2web

Web UI for managing a Sony Alpha camera remotely, built on [alpha2go](https://github.com/Knallli/alpha2go).
Tested on the a6700 only.

- Dashboard with connection status, logs. The camera connects on demand (Connect button or any action that needs it) and disconnects after `CAMERA_IDLE_DISCONNECT` (default 30 s) without use
- Live view (MJPEG)
- Control: shutter (with/without autofocus), movie start/stop and a settings page for every camera property (see below)
- Gallery: offline catalog of both cards with thumbnails; originals are pulled to `LIBRARY_DIR` (see below)
- Files: tracked files of the plugins
- Settings page; environment variables override and lock the fields
- Plugins (compiled in, see `plugin/`), each can add dashboard blocks, a file table, actions, metrics and settings
- Immich exporter (`plugins/immich`): on "Sync new files from camera" connects, downloads every new file on both cards, disconnects and uploads it to Immich with retries, adds it to an album, stacks RAW+JPEG pairs, optionally deletes the local copy and the shot on the card. Dashboard shows queue, progress, problems and recent uploads; nothing is synced automatically; files pulled in the gallery are uploaded too; actions "Sync new files from camera" and "Retry waiting files now"; `alpha2web_immich_*` metrics.

## Control

The settings cards group the camera's properties (exposure, focus, drive, white balance, ...). Each row has a name, the code, a description and a fitting widget: switch, select, slider, number box, or plain text when the camera does not allow a change right now (the reason is shown). Changes apply at once and the page follows changes made on the camera. Search and the filters narrow the list.

- Names, descriptions and value labels come from the [alpha2go property catalog](https://github.com/Knallli/alpha2go/blob/main/docs/properties.md). A value without a known label is shown as its number.
- **Caution** marks settings that change behaviour or wear something. **Danger** marks settings that can lose shots, for example sending shots to the host only so they are not written to the card. Both stay disabled until "Enable dangerous actions" is on, and each change asks for confirmation.
- Settings the catalog does not know are listed as undocumented, hidden by default, and read-only until "Allow editing undocumented settings" is on. The server enforces both rules, not only the page.
- The server only sends values the camera currently offers. Camera buttons and dials are not on the page yet.

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

## Dev container (not for production)

`docker-compose.dev.yml` runs the newest code of a branch straight from GitHub on every container start (default `main`, set `BRANCH=...` for another): `restart` = update, no image pull. It compiles on start and runs unreviewed code, so use the pinned `ghcr.io/knallli/alpha2web` image above for production. See the header of the file.

### Immich as the only copy

Everything new is imported automatically. The gallery is for picking shots: pulled originals are handed to Immich too. Set `DELETE_LIBRARY_AFTER_UPLOAD=true` to drop the library copy once Immich confirmed it, or press **Remove local copies stored in Immich** in the gallery to clean up everything that is already there (also files pulled earlier). The gallery then shows those files straight from Immich ("in Immich" badge) and does not copy them from the camera again. Deleting from the card is a separate option (`IMMICH_DELETE_ON_CAMERA`).

## Run without Docker

```
CAMERA_HOST=192.168.1.50 go run ./cmd/alpha2web
```

| Variable | Meaning |
|---|---|
| `CAMERA_HOST` | camera address; empty = find it on the local network |
| `CAMERA_MAC` | optional, speeds up the search |
| `CAMERA_USER`, `CAMERA_PASSWORD`, `CAMERA_FINGERPRINT` | Access Authentication (SSH tunnel) |
| `CAMERA_WAKE_ON_LAN` | `true` sends Wake-on-LAN packets every 30 s while a connection is wanted and not yet up (needs a MAC) |
| `CAMERA_WAKE_ON_LAN_ADDR` | target of those packets, default `255.255.255.255:9` |
| `CAMERA_KEEPALIVE` | interval of the idle property poll that keeps the session busy and the camera properties fresh, default `10s`; `0` sends no property requests of its own after connecting (the property list is then read on demand). Also on the settings page (Diagnostics), together with "log every camera operation" (`LOG_LEVEL=debug`) |
| `CAMERA_IDLE_DISCONNECT` | how long an unused camera session stays open before it is closed, default `30s`; the session opens again on the next button press or action |
| `CAMERA_RECONNECT_INTERVAL` | first delay between connection attempts, default `5s` (backs off to 1 minute) |
| `SHUTDOWN_TIMEOUT` | grace period for open requests on SIGTERM, default `20s` |
| `LOG_LEVEL` | `debug`, `info` (default), `warn`, `error` |
| `LOG_FORMAT` | `json` (default) or `text` |
| `HTTP_ADDR` | listen address, default `:8765` |
| `DATA_DIR` | settings and plugin state, default `data` |
| `LIBRARY_DIR`, `EXPORT_PATH_TEMPLATE`, `EXPORT_CASE`, `EXPORT_TYPES`, `EXPORT_CONFLICT`, `EXPORT_SET_MTIME` | gallery export folder and naming, see "Export settings" (`LIBRARY_DIR` is `/library` in docker-compose.yml) |
| `IMMICH_URL`, `IMMICH_API_KEY`, `IMMICH_ALBUM` | Immich server, API key (needs `asset.upload`, `asset.view`, `asset.download`, `album.*`, `stack.create`) and optional album |
| `IMMICH_STACK_RAW_JPEG`, `DELETE_AFTER_UPLOAD`, `DELETE_LIBRARY_AFTER_UPLOAD`, `IMMICH_DELETE_ON_CAMERA` | stack pairs (default `true`), delete the staged copy (default `true`), delete the gallery-pulled library copy once it is in Immich (default `false`), delete from the card (default `false`) |
| `SKIP_EXTENSIONS`, `UPLOAD_CONCURRENCY`, `DOWNLOAD_STALL_TIMEOUT`, `MIN_FREE_SPACE_MB`, `RETRY_BASE_DELAY`, `RETRY_MAX_DELAY` | Immich transfer tuning, see [.env.example](.env.example) |
| `STAGING_DIR` | where downloads wait for the upload, default `<DATA_DIR>/staging` |
| `WEBUI_PASSWORD` | optional HTTP basic auth password for the whole UI (`/healthz`, `/health` (JSON) and `/metrics` (Prometheus) stay open) |

## Gallery

The gallery page never talks to the camera. "Load from camera" (later "Refresh from camera") runs a background job that lists both cards, fetches the thumbnails not cached yet and stores the result as a catalog (`<DATA_DIR>/cache/catalog.json`), so the page works with the camera off. Only one job runs at a time, it can be stopped, and the camera session closes after the idle time once it is done. If the camera refuses card access (menu or playback open, Sony app connected) the page says so and keeps the old catalog.

- Click a thumbnail to see the original (expanded, cached; HEIF is converted to JPEG). Opening it or downloading a single file connects the camera if the file has not been pulled yet.
- Select shots ("Select page", "Select all in slot", or tick them; the selection survives page changes) and "Pull selected" copies every file of each shot (JPG, RAW, ...) to the library:
  `<LIBRARY_DIR>/<camera model>/<year>/<year>-<month>-<day>/DSC00001.JPG` (the default path template, see "Export settings"). The catalog remembers where each file went, so pulling again fetches only what is new, even after the template changed; an existing file at the target with the same size and the capture time as mtime (see `EXPORT_SET_MTIME`) is not downloaded again. A file another shot was pulled to is never reused or overwritten. Downloads go to `.partial` files first. Pulled shots are marked "pulled". Deleting a shot from the card keeps the pulled copies.
- Plugins that implement `plugin.Importer` get the pulled files handed over afterwards; the Immich plugin uploads them.
- In Docker mount `/library` to a host or NAS folder, see [docker-compose.yml](docker-compose.yml).

### Export settings

Settings page, card "Export to folder"; every field has an environment variable that overrides it and locks the field. The folder is created on the first pull; if it is not writable the pull is refused with the reason.

| Setting | Variable | Default |
|---|---|---|
| Target folder | `LIBRARY_DIR` | `<DATA_DIR>/library` |
| Path template | `EXPORT_PATH_TEMPLATE` | `{camera}/{year}/{year}-{month}-{day}/{name}{ext}` |
| File name case: `keep`, `lower`, `upper` (file name only, not the folders) | `EXPORT_CASE` | `keep` |
| File types: comma list of `JPEG,HEIF,RAW,VIDEO,OTHER`; other types are skipped and counted in the result | `EXPORT_TYPES` | all |
| If the file exists: `suffix` (same size and capture time = already there, otherwise `_1`, `_2` ...), `always-suffix`, `overwrite` | `EXPORT_CONFLICT` | `suffix` |
| Set the modification time to the capture time | `EXPORT_SET_MTIME` | `true` |

Template tokens: `{camera}` (model), `{serial}`, `{slot}`, `{year}` `{month}` `{day}` `{hour}` `{minute}` `{second}` `{date}` (the camera's clock, as shown on the camera), `{folder}` (camera folder, e.g. `100MSDCF`), `{name}` (file name without extension), `{ext}` (with dot), `{type}`, `{seq}` (4-digit running number within one pull). `/` makes folders. The template needs `{name}` or `{seq}`, must be relative and must not contain `..`; characters that are illegal in file names are replaced by `_`. Settings are checked when saved.

## Limits

- Auto-discovery only searches the local layer-2 network (ARP, SSDP, port scan of the /24). Across routers set the address. With Access Authentication on it scans for the camera's SSH port.
- The camera does not announce new shots to the host and alpha2web does not poll: new shots appear after "Load / Refresh from camera" (gallery) or "Sync new files from camera" (Immich).
- The camera accepts only one remote connection. Do not use the Sony app (Creators' App / Imaging Edge) at the same time: it blocks alpha2web or drops its session.
- Only one live-view stream at a time.
- HEIF thumbnails and previews are converted to JPEG in pure Go (cached); if that fails the original is served and the browser may not show it.
- Forms carry a per-process CSRF token (it changes on restart); set `WEBUI_PASSWORD` and do not expose the UI to the internet.

## AI disclosure

Large parts of this repository were written with the help of an AI coding assistant (Claude) and reviewed by the author.

## License

Apache-2.0. Contains no Sony code.
