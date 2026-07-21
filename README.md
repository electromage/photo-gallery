# photo-gallery

A simple, self-hostable web photo gallery — point it at a directory of photos and
it builds a public, browsable archive. Meant as an easy way to get off Flickr.

- **Scrapes directories automatically** — each sub-folder of your photo root becomes
  an album. Drop in files and they show up; no database, no upload step, no admin UI.
- **Public feed** — the main page shows every photo, newest first, no login required.
- **Albums sidebar** — browse by directory-backed album.
- **Search** — filter by album name, embedded EXIF/XMP title, and tags/keywords.
- **Multi-resolution downloads** — grab any photo at 720p, 1080p, 1440p, 4K, or the
  full-size original — handy for desktop backgrounds. Images are scaled down
  proportionally (never upscaled) and rotated upright using their EXIF orientation.

It's a single Go binary with no runtime dependencies, so it's easy to run in Docker or
straight on a server.

> **Note:** photos are indexed by capture date, title, and tags read from **JPEG**
> EXIF/XMP metadata. JPEG (`.jpg` / `.jpeg`) is the supported input format.

---

## Quick start with Docker (recommended)

No toolchain needed — just Docker.

```bash
# 1. Build the image
docker build -t photo-gallery .

# 2. Run it, mounting your photos read-only and a writable volume for the cache
docker run -d --name photo-gallery \
  -p 8080:8080 \
  -v /path/to/your/photos:/photos:ro \
  -v pg-cache:/cache \
  photo-gallery
```

Open <http://localhost:8080>.

Replace `/path/to/your/photos` with wherever your photos live on the server. The
container reads them from `/photos` and listens on port `8080`. New files are picked
up automatically on the next rescan (every 2 minutes by default).

The `pg-cache` volume persists the index so restarts don't re-scan everything (see
[indexing & performance](#indexing--performance)). It's optional — omit it and each
fresh container just re-indexes from scratch on start.

To update after pulling changes: `docker build -t photo-gallery . && docker rm -f
photo-gallery` then re-run the command above.

### docker compose

```yaml
services:
  photo-gallery:
    build: .
    ports:
      - "8080:8080"
    volumes:
      - /path/to/your/photos:/photos:ro
      - pg-cache:/cache
    restart: unless-stopped

volumes:
  pg-cache:
```

```bash
docker compose up -d --build
```

---

## Running without Docker

Requires [Go 1.23+](https://go.dev/dl/).

```bash
# Serve ./photos on http://localhost:8080
go run .
```

Or build a standalone binary and run it anywhere:

```bash
go build -o photo-gallery .
PHOTO_ROOT=/path/to/your/photos ./photo-gallery
```

---

## How it works

- Put your photos in the photo root, organized into sub-directories. Each directory
  becomes an **album**; photos in the root itself land in the default "Library" album.
- On startup the app walks the tree, indexes every JPEG, and reads capture date,
  title, and tags/keywords from EXIF and XMP metadata.
- The gallery is **read-only and public**. There is no authentication; anyone who can
  reach the port can view and download. Put it behind a reverse proxy (and TLS) if you
  expose it to the internet.

## Indexing & performance

Designed to handle large libraries (tens of thousands of photos):

- **Non-blocking startup.** The server starts listening immediately and shows an
  "indexing…" page (that auto-refreshes) until the first pass finishes — it never
  hangs the way a blocking indexer would.
- **Fast indexing.** Files are processed in parallel, and only the first ~512 KiB of
  each photo is read (enough for EXIF/XMP and dimensions) instead of the whole file,
  so a cold index of thousands of photos takes seconds, not minutes.
- **Automatic updates.** A background rescan runs every `GALLERY_REFRESH` (default
  2 minutes). Add, change, or remove photos and they show up on the next tick — no
  restart needed. Set `GALLERY_REFRESH=15s` for faster pickup; rescans are cheap.
- **Incremental.** Rescans reuse the previous result for any file whose modification
  time and size are unchanged, so only new/changed files are read. A full rescan of an
  unchanged 3,000-photo library takes tens of milliseconds.
- **Persistent cache.** The index is saved to `GALLERY_CACHE` (gob) and reloaded on
  the next start, so restarts reuse prior work instead of re-reading every file. Cache
  problems are always non-fatal — the server falls back to a cold index. In Docker,
  mount a writable volume at `/cache` (the default `GALLERY_CACHE` path) to keep it
  across container recreation.

Each index pass logs a one-line summary, e.g.
`indexed 16000 photos in 42 albums (120 new/changed, 15880 reused) in 180ms`.

## Configuration

Configuration is via environment variables. You can also put them in a **`.env`
file** in the working directory (copy [`.env.example`](.env.example) to `.env`) —
real environment variables always override `.env`, so it's just a convenient default
layer. Point elsewhere with `GALLERY_ENV_FILE=/path/to/file`.

| Variable | Default | Description |
|---|---|---|
| `SITE_TITLE` | `Photo Gallery` | Title shown in the page `<title>` and the header |
| `PHOTO_ROOT` | `./photos` | Directory to index and serve (set to `/photos` inside the container) |
| `SITE_DOMAIN` | _(unset)_ | Public base URL, e.g. `https://photos.example.com`. When set, pages get canonical + Open Graph tags, so shared photo links (`?photo=…`) show a preview with the image |
| `ADDR` | `:8080` | Address/port to listen on |
| `GALLERY_REFRESH` | `2m` | How often to rescan for changes (Go duration, e.g. `30s`, `5m`; `0` disables) |
| `GALLERY_CACHE` | `gallery-cache.gob` | Path to the persisted index cache (`/cache/index.gob` in the Docker image). Set empty to disable persistence |
| `GALLERY_ENV_FILE` | `.env` | Path to the env file to load at startup |

## Endpoints

| Route | Description |
|---|---|
| `GET /` | Main feed (all photos, newest first). `?q=` filters by album/title/tags. |
| `GET /albums/{album}` | A single album's photos. `?q=` filters within it. |
| `GET /media/{path}` | Serves the original image file. |
| `GET /download/{path}?res=1080p` | Download resized to `720p`, `1080p`, `1440p`, `4k`, or `original`. |
| `GET /healthz` | Health check; returns `ok`. |
