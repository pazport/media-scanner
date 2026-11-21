# Media Scanner

[![Docker image](https://github.com/pazport/media-scanner/actions/workflows/docker.yml/badge.svg)](https://github.com/pazport/media-scanner/actions/workflows/docker.yml)
[![Latest release](https://img.shields.io/github/v/release/pazport/media-scanner?logo=github&sort=semver)](https://github.com/pazport/media-scanner/releases/latest)
[![GitHub stars](https://img.shields.io/github/stars/pazport/media-scanner?logo=github&style=flat)](https://github.com/pazport/media-scanner/stargazers)
[![Docker pulls](https://img.shields.io/docker/pulls/man1234/media-scanner?logo=docker&logoColor=white)](https://hub.docker.com/r/man1234/media-scanner)
[![Docker stars](https://img.shields.io/docker/stars/man1234/media-scanner?logo=docker&logoColor=white)](https://hub.docker.com/r/man1234/media-scanner)
[![Image size](https://img.shields.io/docker/image-size/man1234/media-scanner/latest?logo=docker&logoColor=white)](https://hub.docker.com/r/man1234/media-scanner/tags)
[![GHCR](https://img.shields.io/badge/ghcr.io-pazport%2Fmedia--scanner-blue?logo=github)](https://github.com/pazport/media-scanner/pkgs/container/media-scanner)

Radarr, Sonarr and Lidarr send webhooks to Media Scanner, which asks **Plex**, **Jellyfin** and/or **Emby** to scan just the folder that changed, so there are no full library scans.

- Choose which media servers each trigger notifies: one, some or all of them.
- Path rewrites (global, per trigger and per server) handle containers that mount media at different paths.
- Repeated requests for the same folder are batched and de-duplicated, with a configurable delay.
- Before a webhook-triggered scan, Media Scanner waits 5 seconds and checks the actual filesystem path inside its container for supported media files. If none are found, it skips the server scan.
- An optional library check finds media on disk that a server hasn't picked up yet, and scans for it.
- A web UI for every setting. Changes are saved to `/config/config.json`.
- Optional basic auth for the UI and webhooks.
- A ~10 MB `FROM scratch` Docker image (about 3 MB compressed) with no dependencies beyond the Go standard library.

## Webhook scan verification

After the configured queue delay, each queued location waits a further 5 seconds. Media Scanner then recursively checks that location inside its own container using the configured media extensions, ignored folders and ignored filename suffixes. Symlinked media is followed when **Follow symlinks** is enabled in Library Check settings.

- If at least one media file is found, the normal per-trigger target selection and server-specific path rewrites are applied and the scan is sent to the selected enabled servers.
- If no media file is found, no server is contacted and the dashboard history records a **skipped** event.
- If the path cannot be read or does not exist, no server is contacted and the history records an **error**.

Make sure the relevant media folders (and symlink targets, if used) are mounted into the Media Scanner container at the paths supplied by the webhook. For webhook and manual scans, the filesystem check uses the original incoming path after trigger-specific rewrites, but before global rewrites. Global, server-type and individual-server rewrites are used for the outgoing media-server scan path, not to probe the filesystem. This lets you check a real mounted path such as `/sbx/mnt/union-zfs` while sending a mapped path such as `/content-vault/content` to Plex.

## Images

The same image is published to two registries. Use whichever you prefer: both get every release, for `linux/amd64` and `linux/arm64`.

| Registry | Image | Page |
|----------|-------|------|
| Docker Hub | `man1234/media-scanner` | [hub.docker.com/r/man1234/media-scanner](https://hub.docker.com/r/man1234/media-scanner) |
| GitHub Container Registry | `ghcr.io/pazport/media-scanner` | [github.com/pazport/media-scanner/pkgs/container/media-scanner](https://github.com/pazport/media-scanner/pkgs/container/media-scanner) |

## Quick start

`docker-compose.yml`:

```yaml
services:
  media-scanner:
    image: man1234/media-scanner:latest
    container_name: media-scanner
    restart: unless-stopped
    user: "1000:1000"
    environment:
      - TZ=Europe/London
    ports:
      - "3030:3030"
    volumes:
      - ./config:/config
      - /path/to/media:/path/to/media:ro
```

```bash
mkdir config && sudo chown 1000:1000 config
docker compose up -d
```

Open `http://<host>:3030`, add your media servers under **Media Servers**, configure the webhook targets under **Triggers**, then save.

## Connecting Radarr / Sonarr / Lidarr

In each app: **Settings → Connect → + → Webhook**

| App | URL | Events |
|-----|-----|--------|
| Radarr | `http://media-scanner:3030/triggers/radarr` | On Import, On Upgrade, On Rename, On Movie Delete, On Movie File Delete |
| Sonarr | `http://media-scanner:3030/triggers/sonarr` | On Import, On Upgrade, On Rename, On Series Delete, On Episode File Delete |
| Lidarr | `http://media-scanner:3030/triggers/lidarr` | On Release Import, On Upgrade, On Rename, On Track Retag |

Method: `POST`. If auth is on, put the Media Scanner username and password in the webhook's Username/Password fields.

## Path rewrites

Rewrites are applied in this order: trigger, global, server type, then individual server. The first matching prefix in each list is used. The filesystem verification checks the path after trigger and global rewrites, before server-specific rewrites, because that is the path mounted inside the Media Scanner container.

## Library check

The optional Library Check tab compares media files on disk with what selected media servers have indexed. Configure folders as seen inside this container and select the servers to compare. It supports configurable extensions, ignored folders and suffixes, symlink following, caching and periodic full refreshes.

## Manual scans

Use the dashboard, or:

```bash
curl -X POST "http://media-scanner:3030/triggers/manual?path=/movies/Some%20Movie%20(2024)"
```

Manual scans use the same 5-second filesystem verification before a scan is sent.

## Config file

Everything in the UI is stored in `/config/config.json` (path can be overridden with `MEDIA_SCANNER_CONFIG` or `-config`). The file holds your tokens and password, so it is written with `0600` permissions.

## Development

```bash
go test ./...
go run . -config ./config/config.json
```
