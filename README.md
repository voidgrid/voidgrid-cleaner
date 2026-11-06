# voidgrid-cleaner

A small web app for removing media from a Plex library. Users ask to keep or
delete what they requested in Seerr, an admin approves, and the app removes it
through Radarr/Sonarr, Seerr and Plex in an order that stops it being
downloaded again. Status: early beta (sign-in, the sync from every service and a read-only
library screen work; statuses and deletion are not built yet).

## Deployment

Image: `ghcr.io/voidgrid/voidgrid-cleaner`, built by GitHub Actions on version
tags. `:beta` follows every tag, `:latest` only stable ones, and each tag
(e.g. `:v0.1.0-beta.1`) is fixed.

### Requirements

- **HTTPS in front.** Session cookies are always `Secure` and the Plex sign-in
  returns to `https://<host>/login/plex/callback`, so serve it through a
  reverse proxy with TLS, LAN users included. The proxy must pass the original
  `Host` header (most do by default).
- **Seerr, Plex, Radarr, Sonarr and Tautulli**, reachable from the container,
  with their API keys (one Radarr and one Sonarr). Tautulli needs version 2.18
  or newer: the key is sent in a header.
- A writable `/data` for the SQLite database. The image runs as uid/gid
  `1000:1000`, so the host directory must be writable by that user.

### Settings

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `VGC_SEERR_URL` | yes | | Seerr base URL as the container sees it, e.g. `http://seerr:5055` |
| `VGC_SEERR_API_KEY` | yes | | Seerr API key |
| `VGC_PLEX_URL` | yes | | Plex Media Server URL, e.g. `http://192.0.2.10:32400` |
| `VGC_PLEX_TOKEN` | yes | | The server owner's Plex token |
| `VGC_RADARR_URL` | yes | | Radarr URL, e.g. `http://radarr:7878` |
| `VGC_RADARR_API_KEY` | yes | | Radarr API key (Settings → General) |
| `VGC_SONARR_URL` | yes | | Sonarr URL, e.g. `http://sonarr:8989` |
| `VGC_SONARR_API_KEY` | yes | | Sonarr API key (Settings → General) |
| `VGC_TAUTULLI_URL` | yes | | Tautulli URL, e.g. `http://tautulli:8181` |
| `VGC_TAUTULLI_API_KEY` | yes | | Tautulli API key (Settings → Web Interface) |
| `VGC_LISTEN_ADDR` | no | `0.0.0.0:8790` | Address the app listens on |
| `VGC_DB_PATH` | no | `/data/voidgrid-cleaner.db` | SQLite database file |

### Compose

See [`examples/compose.yaml`](examples/compose.yaml). Put the container on the
same Docker network as Seerr so `VGC_SEERR_URL` can use Seerr's container
name, and route the proxy to port `8790`. The image has a healthcheck on
`/healthz`.

### Sync

Every hour the app copies Seerr users and requests, the Plex movie and show
libraries (matched to Radarr by TMDB ID and Sonarr by TVDB ID) and new
Tautulli history into its database; once a day it re-reads all of Tautulli's
history, so plays deleted there drop out. Admins see each source's last result,
errors and anything that didn't match on the **sync** page, and can start a
sync there. It only reads; nothing is changed in any service.

### Libraries

Every Plex movie and show library gets a tab; music and photo libraries are
never handled. Admins can ignore a library on the **libraries** page: it loses
its tab and nothing in it can be deleted. Items that Radarr and Sonarr don't
manage are deleted through Plex alone, which needs **Allow media deletion** on
in Plex's settings.

### Who can sign in

Anyone with a Seerr account, using Plex or a Seerr local account. Seerr users
with **Admin** or **Manage Requests** are admins here; everyone else manages
only what they requested.

### Backups

Back up the `/data` directory (one SQLite file plus its `-wal`/`-shm` files
while running).

## License

MIT, see [LICENSE](LICENSE).
