# voidgrid-cleaner

A small web app for removing media from a Plex library. Users ask to keep or
delete what they requested in Seerr, an admin approves, and the app removes it
through Radarr/Sonarr, Seerr and Plex in an order that stops it being
downloaded again. Status: release candidate (deletion is a dry run until you
turn it on).

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
- Your media folders and every downloader's download folder mounted into the
  container (see [Mounts](#mounts)). Without them the app still deletes
  through Radarr, Sonarr and Plex, but the doctor can't check the disk and
  download folders aren't reported.

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
| `VGC_DISCORD_ADMIN_CHANNEL` | no | | Channel ID for tech alerts: the admin digest and unlinked Add me notices |
| `VGC_DELETE_LIVE` | no | `false` | `true` really deletes; anything else is a dry run |
| `TZ` | no | `UTC` | Time zone for times on screen and the delete window, e.g. `Etc/UTC` |
| `VGC_LISTEN_ADDR` | no | `0.0.0.0:8790` | Address the app listens on |
| `VGC_DB_PATH` | no | `/data/voidgrid-cleaner.db` | SQLite database file |

### Compose

See [`examples/compose.yaml`](examples/compose.yaml). Put the container on the
same Docker network as Seerr so `VGC_SEERR_URL` can use Seerr's container
name, and route the proxy to port `8790`. The image has a healthcheck on
`/healthz`.

### Mounts

Radarr, Sonarr and Plex tell the app where things are by path. For it to look
at those paths itself, mount them into the container:

- **Media folders, read-write, at the same path** Radarr, Sonarr and Plex use,
  e.g. `/srv/media:/srv/media`. The doctor checks them for empty folders, stray
  files and the like, and changes them only in paths you tick as managed.
- **Every downloader's download folder, read-only, at the same path** the
  downloader and Radarr and Sonarr use, e.g. `/srv/torrents:/srv/torrents:ro`.
  Their size is reported on the **admin** page and in the tech digest, next to
  the disk they're on, so you can see what seeding or a stuck import is using.
  A download folder on a disk with no library is reported on its own. Leave
  one out if you don't need it reported; dismiss its warning on the doctor page.

The **settings** page lists every path the services use, with whether this
app can see it, write it, and whether it's verified: a file the services list
really exists there. An empty mountpoint fails that, so a missing mount never
makes a library look empty. If you can't mount a path at the same place, put
where it is mounted under **mounted here at**, e.g. `/data/media` mounted at
`/media`; that covers everything under it, and saving starts a check. Paths
that aren't mounted or aren't verified show as a warning on the **admin**
page, linking to both.

Radarr and Sonarr report free space themselves, so space-based deletion works
on every disk without any of these mounts.

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
in Plex's settings. Every show belongs in Sonarr and every movie in Radarr: one
in a root folder they don't track shows on the doctor page as an untracked
folder you can add, and one outside every root folder is reported so you can
move it.

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

Every hour the app asks Radarr and Sonarr how full each disk is, and works out
each item's disk from its path. Each disk has a range on the **settings**
page: a start (default 75%, about where filesystems start to slow down) and a
floor (default 70%). When a disk reaches its start, enough on it is scheduled
for deletion to bring it down to its floor: its space-based items first, oldest
watched first, then never-watched ones; if those aren't enough, the longest
unwatched of everything else that isn't kept or ephemeral. Deletions already on
their way count. Each goes through the usual notice and delay, so anything can
still be kept. A deletion that's cancelled or fails isn't tried again until the
item's status is set again. Items Radarr and Sonarr don't manage are never
picked, because nothing says which disk they're on.

### Doctor

The **doctor** page lists where Plex, Radarr, Sonarr, Seerr and the disk disagree: wrong or questionable Plex matches, release
names as titles, untracked folders, stale requests, files off the naming scheme, and more. It runs after each sync. A service
that can't be reached only skips the checks that need it, and their last results stay. Dismiss anything you've decided is
fine, and it stays dismissed. Each section has its own **preview ticked fixes** and **dismiss ticked** buttons.

Entries with a checkbox can be fixed from here: wrong and questionable Plex matches, release names as titles, and a library's
**Prefer local metadata** setting, which is how those titles happen. If Plex's match is right and only the name is wrong, the
fix is a metadata refresh, not a rematch. Radarr and Sonarr issues can be fixed too: renaming files to match your format, adding
an untracked folder (unmonitored, with no search, so nothing downloads) or, for a show folder with no video in it, removing it
instead, importing a file Radarr won't accept with a quality you
pick, and stopping Radarr leaving empty folders. On managed paths it can also remove empty folders and delete stray files, and it
can clear stale Seerr requests. Missing items offer search, stop monitoring or remove; removing never touches a file. A movie TMDb
removed can be taken out of Radarr, keeping its file, and other Radarr and Sonarr warnings link to their help page. Stray files are gone for good; until `VGC_DELETE_LIVE=true`, that fix only reports what it would delete. Tick them and press **preview** to see exactly what would change; nothing
happens until you press **apply**. Fixes run one at a time in the background, are checked afterwards, and skip anything being
deleted.

It needs to see your media to check the disk; see [Mounts](#mounts). The doctor looks only inside paths you've ticked as
managed and that are verified. A path a service uses and this app can't see is reported, and you can dismiss it if it isn't
needed.

### Downloads

The **downloads** page shows Radarr's and Sonarr's queues together, with each item's real reason when it's stuck. A stuck
download appears on the doctor page with its fixes, and is posted once: straight away if Radarr or Sonarr won't import it or it
failed, since those never clear on their own, and after 15 minutes if it's only a warning, such as a stalled download. The fixes: import it as Radarr or Sonarr identified it, remove it,
blocklist only, or blocklist and search. With `VGC_DISCORD_ADMIN_CHANNEL` set, it's also posted there with buttons for remove
and both blocklists; only an admin whose Discord account is linked on the users page can press them.

### Log

The **log** page shows who decided what: status changes, deletions and each of their steps, Keep and Add me presses, and
settings changes. At the **audit** log level, the default, it also records every call the app makes to another service while
acting: method, path and result, never keys or tokens. **Normal** leaves those out. Entries older than the **log expiry** on the
settings page (default 90 days) are removed.

### Watch history

A watch counts if either Plex or Tautulli has it. Plex's history goes back further; Tautulli adds partial plays. If Tautulli
is down, plays keep arriving from Plex, and a deletion only needs one of the two to confirm nobody is watching. Plays of
items since removed from Plex can't be tied to anything in the library.

Plex reattaches old history when something is added again. Here a re-add starts clean: plays from before an item's added date
don't count. Plex's own watched marks aren't changed.

### Daily digest

Once a day, at the time on the **settings** page (default 05:30), the bot posts what's going in the next day, the one thing
anyone can still act on. It names shows, never episodes, tags nobody and has no buttons. What was deleted and which ephemeral
shows are waiting on someone are on the **status** page instead. With `VGC_DISCORD_ADMIN_CHANNEL` set, a second post there covers failed or held deletions, sync errors, new doctor
findings, unlinked users and disk levels. Days with nothing to say post nothing.

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

Single episodes can go too: a show's card has an **episodes** link for admins,
listing every episode Plex has with a **delete** button. Each goes through the
same notice and delay, named on its own (Show S01E02), and Sonarr stops
monitoring it so it isn't downloaded again; the show and its other episodes
stay. Keep on the show stops its episodes going.

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

### Requests

Everyone else works on what they requested, from the item's card: **make ephemeral** applies at once, because anyone can add
themselves to an ephemeral show, and only from no status, so it never overrides an admin. **ask to keep** and **ask to
delete** wait on the **admin** page, where admins approve or deny them. Approving one settles the item: any other ask on it is
denied. Conflicting asks are marked. Space-based is for admins only. Admins can let a user skip approval; their asks then
apply at once, unless someone else's ask on the same item is waiting.

The admin page also shows each disk holding a library with its range and the download folders on it, the space-based items
in the order they'd go, and a warning for paths that aren't mounted.

### Who can sign in

Anyone with a Seerr account, using Plex or a Seerr local account. Seerr users
with **Admin**, **Manage Requests** or any **Auto-Approve** permission are admins here; everyone else manages
only what they requested.

### Backups

Back up the `/data` directory (one SQLite file plus its `-wal`/`-shm` files
while running).

## License

MIT, see [LICENSE](LICENSE).
