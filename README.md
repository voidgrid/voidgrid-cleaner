# voidgrid-cleaner

A small web app for removing media from a Plex library. Users ask to keep or
delete what they requested in Seerr, an admin approves, and the app removes it
through Radarr/Sonarr, Seerr and Plex in an order that stops it being
downloaded again. Status: early beta (sign-in, the sync from every service and the library
screen with statuses, filters and deletion work; deletion is a dry run until
you turn it on).

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
| `VGC_DISCORD_BOT_TOKEN` | for live deletion | | Discord bot token |
| `VGC_DISCORD_PUBLIC_KEY` | for live deletion | | The Discord application's public key |
| `VGC_DISCORD_CHANNEL` | for live deletion | | Channel ID for notifications |
| `VGC_DELETE_LIVE` | no | `false` | `true` really deletes; anything else is a dry run |
| `TZ` | no | `UTC` | Time zone for times on screen and the delete window, e.g. `Etc/UTC` |
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

### Statuses and filters

Each item is normal, keep or space-based; shows can also be ephemeral. Admins
set this in the item's details. Each library tab can filter by status, age,
how long since anyone watched and whether a requester watched; filters combine.

### Plex accounts

Requester-watched needs to know which Plex account is whose. Seerr knows this
for Plex users; for everyone else, admins link accounts on the **users** page,
which lists each Plex account in Tautulli's history that Seerr doesn't link to
anyone. One person can have several accounts. Everything played on a linked
account counts as that person, so leave shared accounts unlinked. Links don't
affect sign-in.

### Space-based items

Every hour the app asks Radarr and Sonarr how full each disk is. When a disk is
used past the level on the **settings** page (default 90%), its space-based
items are scheduled for deletion, oldest watched first, then never-watched
ones, until enough is going to get back under the level. Each goes through the
usual notice and delay. A deletion that's cancelled or fails isn't tried again
until the item is marked space-based again. Items Radarr and Sonarr don't
manage are never picked, because nothing says which disk they're on.

### Ephemeral shows

Each episode of an ephemeral show is deleted once everyone on the show has
watched it, after the usual notice and delay. The show and later episodes stay.
Its requesters are always on it, and setting a show ephemeral posts it to
Discord with an **Add me** button. Anyone can also add themselves on the
**ephemeral** page, which lists these shows for everyone signed in.

Someone whose Discord account isn't linked to a Seerr user is added once an
admin links them on the **users** page; the bot tags the user set on the
**settings** page (default: the server owner) to say so. Discord and the status
screen never list episodes: one notice names the show and how many are going, and Keep on it
keeps those episodes while the show stays ephemeral. Only shows Sonarr manages lose episodes, and a
show with nobody on it loses none.

### Deletion

Admins press **delete** on an item. A Discord bot posts it to a channel, and
the app deletes it 24 hours after Discord confirms the post; if the post fails,
the clock doesn't start. Until then anyone in the channel can press **Keep**,
and the **status** screen can keep, postpone or cancel it. Right before deleting, the app checks that nobody is watching and
asks Radarr and Sonarr whether they manage the item. On the **settings** page,
admins can change the window deletions run in, 03:00 to 05:00 by default, or
clear it to delete at any time; anything due outside it waits for the window
to open.

Items Radarr or Sonarr manage are unmonitored and cleared from the queue
before their files go, so they can't be downloaded again, then removed from
Seerr. Anything else is deleted through Plex.

Until `VGC_DELETE_LIVE=true`, everything runs as a dry run: the status screen
shows what would have happened and nothing changes. Items scheduled during a
dry run stay dry-run after you turn it on.

### Discord bot

Create an application in the Discord Developer Portal, invite its bot to your
server with View Channels and Send Messages, and set the three `VGC_DISCORD_`
variables. Once the app is running, set the application's **Interactions
Endpoint URL** to `https://<host>/discord/interactions`.

That path must be reachable from the internet and must not sit behind anything
that bans on failed requests: Discord sends invalid signatures on purpose to
test it, and every request is checked against the application's public key.
The only action it accepts is Keep. A Keep works straight away; people who
press it appear on the **users** page, where admins can link them to a Seerr
user to show who it was.

### Pages

Library pages start with 32 items and load more as you scroll. Posters are
fetched in the background, so they're usually on disk before anyone needs them. Open pages check
for changes every 5 seconds, so a Keep pressed in Discord shows up by itself.
Everything also works without JavaScript, as plain links and forms.

### Who can sign in

Anyone with a Seerr account, using Plex or a Seerr local account. Seerr users
with **Admin** or **Manage Requests** are admins here; everyone else manages
only what they requested.

### Backups

Back up the `/data` directory (one SQLite file plus its `-wal`/`-shm` files
while running).

## License

MIT, see [LICENSE](LICENSE).
