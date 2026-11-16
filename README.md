# Media Scanner
[![Docker image](https://github.com/pazport/media-scanner/actions/workflows/docker.yml/badge.svg)](https://github.com/pazport/media-scanner/actions/workflows/docker.yml) 
Radarr, Sonarr and Lidarr send webhooks to Media Scanner, which asks **Plex**, **Jellyfin** and/or **Emby** to scan just the folder that changed, so there are no full library scans.

- Choose which media servers each trigger notifies: one, some or all of them.
- Path rewrites (global, per trigger and per server) handle containers that mount media at different paths.
- Repeated requests for the same folder are batched and de-duplicated, with a configurable delay.
- A web UI for every setting. Changes are saved to `/config/config.json`.
- Optional basic auth for the UI and webhooks.
- A ~10 MB `FROM scratch` Docker image (about 3 MB compressed) with no dependencies beyond the Go standard library.

## Quick start

Images for `linux/amd64` and `linux/arm64` are published to `ghcr.io/pazport/media-scanner` and Docker Hub as `man1234/media-scanner`.

```bash
mkdir config && sudo chown 1000:1000 config
docker compose up -d
```

Open `http://<host>:3030`, then:

1. **Media Servers**: add Plex, Jellyfin and/or Emby, enter the URL and token, then click **Test**.
2. **Triggers**: choose which servers Radarr, Sonarr and Lidarr notify.
3. Click **Save**.

Plain `docker run`:

```bash
docker run -d --name media-scanner -p 3030:3030 -v $(pwd)/config:/config ghcr.io/pazport/media-scanner:latest
```

### Image tags

| Tag | Built from |
|-----|------------|
| `latest` | every push to `main` |
| `1.2.3`, `1.2` | git tags like `v1.2.3` |
| `sha-abc1234` | every published build |

The GitHub Action in `.github/workflows/docker.yml` runs the tests first. Pull requests build the image but don't push it.

Docker Hub publishing needs two repository secrets (**Settings → Secrets and variables → Actions**): `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN`, a Docker Hub personal access token with **Read, Write, Delete** access. Docker Hub needs that level of access for the workflow to copy this README to the Docker Hub page. Without the secrets, images go to GHCR only.

To build it yourself: `docker build -t media-scanner .`

The container runs as UID/GID `1000:1000`. Use `user:` in compose (or `--user`) to change it, and make sure that user owns the config folder.

## Connecting Radarr / Sonarr / Lidarr

In each app: **Settings → Connect → + → Webhook**

| App    | URL                                    | Events                                                         |
|--------|----------------------------------------|----------------------------------------------------------------|
| Radarr | `http://media-scanner:3030/triggers/radarr` | On Import, On Upgrade, On Rename, On Movie Delete, On Movie File Delete |
| Sonarr | `http://media-scanner:3030/triggers/sonarr` | On Import, On Upgrade, On Rename, On Series Delete, On Episode File Delete |
| Lidarr | `http://media-scanner:3030/triggers/lidarr` | On Release Import, On Upgrade, On Rename, On Track Retag      |

Method: `POST`. If auth is on, put the Media Scanner username and password in the webhook's Username/Password fields. Click **Test** in the app, and the event appears on the Media Scanner dashboard.

## Tokens

- **Plex:** your `X-Plex-Token`. See Plex's support article "Finding an authentication token / X-Plex-Token".
- **Jellyfin:** Dashboard → API Keys → +
- **Emby:** Settings → Advanced → API Keys → New API Key

## Path rewrites

Radarr might see a movie at `/data/media/movies/X`, while Plex sees it at `/movies/X`. Add a rewrite on the Plex server: `/data/media/movies` → `/movies`.

Rewrites are applied in this order, and each list uses its first matching prefix:

1. **Trigger:** Triggers tab, e.g. only for Sonarr.
2. **Global:** Settings tab, for every path.
3. **Server type:** Settings tab, one list each for all Plex, all Jellyfin and all Emby servers.
4. **Individual server:** Media Servers tab, on each server's card.

For example, if Radarr uses `/data/media/...`, Plex sees `/media/...` and Jellyfin and Emby both see `/mnt/media/...`, add `/data/media` → `/media` under Plex and `/data/media` → `/mnt/media` under both Jellyfin and Emby.

For Plex, Media Scanner finds the library whose folder contains the rewritten path and refreshes only that path. If no library matches, the dashboard shows an error, which usually means a rewrite is missing.

## Manual scans

Use the dashboard, or:

```bash
curl -X POST "http://media-scanner:3030/triggers/manual?path=/movies/Some%20Movie%20(2024)"
```

You can also send JSON to choose servers by ID: `{"paths": ["/movies/X"], "targets": ["<target id>"]}`.

## Config file

Everything in the UI is stored in `/config/config.json` (path can be overridden with `MEDIA_SCANNER_CONFIG` or `-config`). You can edit it by hand, but restart the container afterwards. Changes made in the UI apply straight away, except the port, which needs a restart.

```json
{
  "port": 3030,
  "scan_delay": 10,
  "auth": { "username": "", "password": "" },
  "rewrites": [{ "from": "/data/media", "to": "/media" }],
  "type_rewrites": {
    "plex":     [{ "from": "/media", "to": "/plex-media" }],
    "jellyfin": [],
    "emby":     []
  },
  "triggers": {
    "radarr": { "enabled": true, "all_targets": true,  "targets": [], "rewrites": [] },
    "sonarr": { "enabled": true, "all_targets": false, "targets": ["a1b2c3"], "rewrites": [] },
    "lidarr": { "enabled": true, "all_targets": true,  "targets": [], "rewrites": [] },
    "manual": { "enabled": true, "all_targets": true,  "targets": [], "rewrites": [] }
  },
  "targets": [
    { "id": "a1b2c3", "name": "Plex", "type": "plex", "url": "http://plex:32400",
      "token": "xxxx", "enabled": true, "rewrites": [{ "from": "/media", "to": "/data" }] }
  ]
}
```

The file holds your tokens and password, so it is written with `0600` permissions.

## Development

```bash
go test ./...
go run . -config ./config/config.json
```
