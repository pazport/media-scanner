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
- An optional library check finds media on disk that a server hasn't picked up yet, and scans for it.
- A web UI for every setting. Changes are saved to `/config/config.json`.
- Optional basic auth for the UI and webhooks.
- A ~10 MB `FROM scratch` Docker image (about 3 MB compressed) with no dependencies beyond the Go standard library.

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
    image: man1234/media-scanner:latest            # Docker Hub
    # image: ghcr.io/pazport/media-scanner:latest  # or GitHub Container Registry
    container_name: media-scanner
    restart: unless-stopped
    user: "1000:1000"
    environment:
      - TZ=Europe/London
    ports:
      - "3030:3030"
    volumes:
      - ./config:/config
```

```bash
mkdir config && sudo chown 1000:1000 config
docker compose up -d
```

Open `http://<host>:3030`, then:

1. **Media Servers**: add Plex, Jellyfin and/or Emby, enter the URL and token, then click **Test**.
2. **Triggers**: choose which servers Radarr, Sonarr and Lidarr notify.
3. Click **Save**.

Plain `docker run`, from Docker Hub:

```bash
docker run -d --name media-scanner -p 3030:3030 -v $(pwd)/config:/config man1234/media-scanner:latest
```

or from GitHub Container Registry:

```bash
docker run -d --name media-scanner -p 3030:3030 -v $(pwd)/config:/config ghcr.io/pazport/media-scanner:latest
```

To build it yourself: `docker build -t media-scanner .`

The container runs as UID/GID `1000:1000`. Use `user:` in compose (or `--user`) to change it, and make sure that user owns the config folder.

### Image tags

Both registries have the same tags:

| Tag | Built from |
|-----|------------|
| `latest` | every push to `main` |
| `1.2.3`, `1.2` | each release (see below) |
| `sha-abc1234` | every published build |

### Publishing

The GitHub Action in `.github/workflows/docker.yml` runs the tests first, then pushes to both registries. Pull requests build the image but don't push it.

Docker Hub publishing needs two repository secrets (**Settings → Secrets and variables → Actions**): `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN`, a Docker Hub personal access token with **Read, Write, Delete** access. Docker Hub needs that level of access for the workflow to copy this README to the Docker Hub page. Without the secrets, images go to GHCR only.

### Releases

Every merge to `main` is released automatically. The workflow takes the newest `vX.Y.Z` tag, adds one to the last number (`v1.0.0` → `v1.0.1`), builds and pushes the image with those version tags, then creates the git tag and a GitHub release with notes listing the merged pull requests. Re-running a build for a commit that's already released reuses its version instead of making a new one.

To bump the minor or major version, publish a release yourself with the new number, e.g. `v1.1.0` (**Releases → Draft a new release**, create the tag on `main`). The images are built for that tag, and later merges continue from it: `v1.1.1`, `v1.1.2`, and so on.

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

## Library check

Webhooks cover new downloads, but files can still be missed: a webhook fails, a file is copied in by hand, or a server skips a scan. The library check catches those.

On a schedule it:

1. Lists the media files in the folders you choose (every 10 minutes by default).
2. Asks each selected Plex, Jellyfin or Emby server for the files it has (every 5 minutes by default), and compares them with the files from the last folder read.
3. Requests a scan of every folder that has a file the server is missing. These scans use the same queue and path rewrites as the webhooks.

Set it up on the **Library Check** tab:

- **Folders to check:** paths as this container sees them, so mount your media into the container, e.g. `- /mnt/media:/mnt/media:ro`. Read-only is fine.
- **Path rewrites:** media paths are rewritten exactly like webhook paths (library check rewrites, then global, then server-type, then per-server). If webhooks already scan the right place, the check usually needs no extra rewrites.
- **Ignore files newer than:** skips files changed in the last few minutes, which may still be copying (default 10).
- **Re-scan the same folder after:** once a scan has been requested for a folder, it isn't requested again until this time has passed, even if the file is still missing (default 360 minutes). Without this, a file the server can't import would be re-scanned every check.
- **Symlinks:** symlinked files and folders are followed (turn off under **Advanced → Follow symlinks**). A link's target must be mounted into the container too, at the same path the link points to; links whose target can't be found are counted and listed as broken in the status, so you can see what's not mounted.
- **Advanced:** which extensions count as media, and which extras folders (`Featurettes`, `Trailers`, …) and file name endings (`-trailer`, `-sample`, …) are skipped. Servers don't list extras as normal items, so they would otherwise always look missing.

The status area shows when the last check ran, how many files each server knows, and which folders were sent for scanning or are waiting to be re-scanned. **Run check now** starts one straight away.

While a check runs, the status updates every 2 seconds with its progress: how many media files and folders it has found so far (and the folder it's in), then which server it's asking and how many of that server's items have arrived, with a progress bar.

Each check lists every item in the libraries that cover your folders, so on very large libraries you may want a longer interval, e.g. 30–60 minutes.

**Caching:** both sides are cached between checks, so the frequent comparisons are cheap.

- **Folders:** each folder's contents are remembered along with its modified time. On the next read, a folder whose modified time hasn't changed isn't listed again and its symlinks aren't looked up again, so a re-read mostly just checks each folder's modified time.
- **Media servers:** each server's file list is kept. Later comparisons only ask for items changed since the last one (Plex: `updatedAt`, Jellyfin/Emby: `MinDateLastSaved`) and merge them in. If a server refuses that request, its whole list is fetched instead.
- **Full check:** every **Full check every (hours)** (default 6), both caches are thrown away: every folder is read and each server's whole list is fetched. This catches what the caches can miss. On mergerfs, a folder's modified time may not change when a file lands on a different drive, and items deleted from a server stay in its stored list until the next full check. **Full check** on the Library Check tab does this straight away.

The caches are kept in memory, so a restart of the container starts with a full check.

**Broken symlinks:** links whose target can't be found are listed in full on the **Broken symlinks** card, with where each one points. The list can be searched, paged and downloaded as text. They're grouped by where they point, and each group is labelled:

- **Target files are gone:** the folder exists in the container and has files, but these targets don't. The links can be deleted.
- **Folder is empty:** maybe a mount (rclone, debrid, NFS) that's down. Deleting is blocked.
- **Folder not found in container:** probably not mounted. Add it to the container's volumes at the same path. Deleting is blocked.

You can override the block if you've checked the files really are gone.

**Test** looks up the targets again and drops links that work now. **Delete** removes only the links, never the files they point to, and you confirm it by typing `DELETE`. Each link is checked again just before deleting: it must still be a symlink, still be broken and inside a checked folder. Deleting needs the folder mounted without `:ro`.

**Speed:** folders are read 8 at a time by default (**Advanced → Folders read at the same time**). On union filesystems like mergerfs, network shares and FUSE mounts, most of the time goes on waiting for each folder listing, so raising this to 16–32 can make the first step much faster. On a single spinning disk, 1–4 is gentler. During a check the status shows how many folders a second it's reading. File ages for **Ignore files newer than** are only looked up for files a server is missing, so the walk itself only reads folder listings.

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
