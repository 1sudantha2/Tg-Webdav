# Tg-WebDAV 📦

A **complete, lightweight, high-performance WebDAV server backed by Telegram**,
written in Go. Your private Telegram channel becomes a personal cloud drive:

- **`/dav`** — a full standard WebDAV endpoint (HTTP Basic Auth) that works out of
  the box with **Windows Explorer, macOS Finder, VLC, Infuse and Cyberduck**.
  Implements `PROPFIND`, `GET`, `HEAD`, `PUT`, `DELETE`, `MKCOL`, `MOVE`, `COPY`,
  `LOCK/UNLOCK` and `PROPPATCH`.
- **`/web`** — an embedded, dark, responsive **Web UI** (vanilla HTML/CSS/JS via
  `go:embed`) with a login screen, file browser, drag & drop uploads and an
  HTML5 player (smooth video seeking, audio, image viewer).
- **Bot inbox ingestion** — send any file/video/photo/audio to your bot in DM
  and it is automatically stored and indexed in your drive.

### Performance characteristics

| Property | Value |
|---|---|
| Idle RAM | ~30–50 MB (Go + SQLite + MTProto session) |
| Disk caching of file content | **zero** — pure streaming |
| Streaming layer | custom `io.ReadSeeker` over MTProto `upload.getFile` chunks (bypasses the Bot API 20 MB limit; files up to **2 GB**) |
| Range requests | full HTTP **206 Partial Content** → instant video seeking |
| Chunk cache | sliding-window in-memory LRU (default **64 MB**, 4 MB chunks, single-flight) |
| SQLite | `modernc.org/sqlite` — **pure Go, no CGO** → fully static binary |

---

## 1. Telegram setup (5 minutes)

1. **API credentials** — go to <https://my.telegram.org> → *API development tools*
   → create an application. Note down `api_id` and `api_hash`.
2. **Bot** — talk to [@BotFather](https://t.me/BotFather) → `/newbot` → keep the
   **bot token**.
3. **Storage channel** — create a **private channel**, then add your bot as an
   **admin** with the *Post messages* right (also allow *Delete messages* if you
   want WebDAV `DELETE` to free Telegram storage).
   - Easiest channel id: forward any message from the channel to
     [@userinfobot](https://t.me/userinfobot)/[@getidsbot](https://t.me/getidsbot),
     or open the channel in a Telegram client and read the id from the URL
     (`-1001234567890`). Public channels can also be referenced by `@username`.

---

## 2. VPS setup

### Option A — prebuilt static binary (recommended)

Every push and release builds a **static Linux AMD64 binary** via GitHub Actions
(`CGO_ENABLED=0 GOOS=linux GOARCH=amd64`, see `.github/workflows/build.yml`).
Grab it from the workflow artifacts or the release assets:

```bash
sudo mkdir -p /opt/tg-webdav && cd /opt/tg-webdav
# From a release asset (once a release is published):
sudo curl -L -o tg-webdav https://github.com/1sudantha2/Tg-Webdav/releases/latest/download/tg-webdav-linux-amd64
sudo chmod +x tg-webdav
# Or grab the latest build artifact from the Actions tab:
#   https://github.com/1sudantha2/Tg-Webdav/actions  →  build  →  tg-webdav-linux-amd64
```

### Option B — build from source

```bash
git clone https://github.com/1sudantha2/Tg-Webdav.git
cd Tg-Webdav
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o tg-webdav ./cmd/server
```

### Configure `.env`

```bash
cp .env.example .env
nano .env
```

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8080` | HTTP port for `/dav` and `/web` |
| `TELEGRAM_API_ID` | — | from my.telegram.org |
| `TELEGRAM_API_HASH` | — | from my.telegram.org |
| `TELEGRAM_BOT_TOKEN` | — | from @BotFather |
| `TELEGRAM_CHANNEL_ID` | — | `-100…` numeric id or `@username` |
| `WEBDAV_USER` | — | WebDAV / Web UI username |
| `WEBDAV_PASSWORD` | — | WebDAV / Web UI password |
| `DEFAULT_FOLDER` | `general` | virtual folder for bot-DM uploads |
| `CHUNK_CACHE_SIZE_MB` | `64` | in-memory streaming cache budget |
| `DB_PATH` | `./data/metadata.db` | SQLite metadata index |

The `.env` file is optional — real environment variables take precedence.

### Run it

```bash
./tg-webdav
# log line: http server listening  addr=0.0.0.0:8080  webdav=/dav  webui=/web/
```

### systemd service

`/etc/systemd/system/tg-webdav.service`:

```ini
[Unit]
Description=Tg-WebDAV (Telegram-backed WebDAV server)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/tg-webdav
ExecStart=/opt/tg-webdav/tg-webdav
Restart=on-failure
RestartSec=5
# hardening
NoNewPrivileges=true
ProtectSystem=full
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now tg-webdav
journalctl -u tg-webdav -f
```

### Reverse proxy + HTTPS (recommended)

The Web UI login travels as plain Basic Auth unless you add TLS. Nginx example:

```nginx
server {
    listen 443 ssl http2;
    server_name dav.example.com;
    # ssl_certificate ...; ssl_certificate_key ...;

    # allow arbitrarily large uploads (Telegram caps at 2 GB per file)
    client_max_body_size 2048M;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header Authorization $http_authorization;
        proxy_buffering off;          # keep streaming low-latency
        proxy_request_buffering off;  # stream uploads straight through
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
    }
}
```

---

## 3. Connecting WebDAV clients

Base URL: `https://dav.example.com/dav` (or `http://<vps-ip>:8080/dav`),
credentials = `WEBDAV_USER` / `WEBDAV_PASSWORD`.

### Web UI

Open `https://dav.example.com/web/` → sign in → browse, upload (drag & drop),
rename, delete, and play media with instant seeking.

### Windows Explorer

1. *This PC* → right-click → **Map network drive** → pick a letter.
2. Folder: `http://<vps-ip>:8080/dav` → *Connect using different credentials*.
3. Windows requires plain-HTTP WebDAV to be allowed:
   set `HKLM\SYSTEM\CurrentControlSet\Services\WebClient\Parameters\BasicAuthLevel`
   to `2` and restart the *WebClient* service — **or just use HTTPS**, which
   works out of the box. (Cyberduck avoids this Windows quirk entirely.)

### macOS Finder

`⌘K` → `http://<vps-ip>:8080/dav` → connect with your credentials.

### VLC

*Media → Open Network Stream* → `http://user:pass@<vps-ip>:8080/dav/general/movie.mp4`
— seeking works because the server answers Range requests (206) directly from
Telegram chunks.

### Infuse (iOS/tvOS)

*Settings → Add Files → WebDAV* → host `<vps-ip>`, port `8080`, path `/dav`,
username/password as configured.

### Cyberduck / FileZilla / rclone

- Cyberduck: *Open Connection → WebDAV (HTTP/HTTPS)*, server, port, path `/dav`.
- rclone:

```ini
[tg]
type = webdav
url = http://<vps-ip>:8080/dav
vendor = other
user = admin
pass = <rclone-obscured-password>
```

---

## 4. Bot ingestion & filename rules

Send anything to your bot's DM; it is streamed into `TELEGRAM_CHANNEL_ID` and
indexed under `DEFAULT_FOLDER` (default `/general`):

1. **Caption present** → used as the file name.
2. **Caption without extension** → extension derived from the MIME type
   (`Holiday video` + `video/mp4` → `Holiday video.mp4`).
3. **No caption** → original upload file name.
4. **No name at all** → MIME-aware timestamp: `file_20260922_150405.mp4`.

Duplicates never collide (`name (1).ext`, `name (2).ext`, …).

> Note: `DELETE` removes the index entry **and** the Telegram channel message
> (when no copy references it anymore) — the storage channel stays clean.
> `MOVE`/`COPY` are pure index operations: no bytes move, copies share the same
> Telegram document.

---

## 5. Project layout

```
cmd/server/main.go        entry point: config → db → telegram → http
internal/config           .env / environment loading & validation
internal/db               pure-Go SQLite virtual file system index
internal/stream           4 MiB sliding-window chunk cache + io.ReadSeeker
internal/telegram         gotd/td MTProto client: auth, channel resolution,
                          bot inbox ingestion, streaming up/download
internal/webdav           RFC 4918 endpoint (x/net/webdav) + PUT/COPY fast paths
internal/webui            embedded dark Web UI + JSON API + session cookies
.github/workflows         static linux/amd64 cross-compile on push/release
```

## 6. FAQ / troubleshooting

- **`bot login failed`** — wrong `TELEGRAM_API_ID`/`API_HASH`/`BOT_TOKEN`.
- **`storage channel not resolved`** — the bot must be an **admin** of the
  channel; numeric ids must match exactly (`-100` prefix accepted).
- **Slow seeks** — raise `CHUNK_CACHE_SIZE_MB`; each miss fetches 4 MB chunks
  in 1 MB MTProto requests.
- **Session reset** — delete `data/session.json` to force re-login.
- **2 GB per-file limit** — Telegram's own cap for MTProto uploads/downloads.
