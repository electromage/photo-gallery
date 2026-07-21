# photo-gallery

A simple, self-hostable web photo gallery — point it at a directory of photos and
it builds a public, browsable archive. Meant as an easy way to get off Flickr.

- **Scrapes directories automatically** — every folder (and sub-folder) of your photo
  root becomes an album. Drop in files and they show up; no database, no upload step.
- **Public feed** — the main page shows every photo, newest first, no login required,
  with month/year section breaks.
- **Folder tree** — a collapsible **Folders** dropdown mirrors your directory
  structure; selecting a folder shows it and everything beneath it.
- **Keyword browsing & filtering** — a **Keywords** dropdown lists every EXIF/XMP
  keyword (most-used first, with a filter box); click one — or a keyword on a photo —
  to show just those photos (`?tag=…`). Folder + keyword + text search combine.
- **Search** — filter by album name, embedded EXIF/XMP title, and tags/keywords.
- **Multi-resolution downloads** — grab any photo at 720p, 1080p, 1440p, 4K, or the
  full-size original — handy for desktop backgrounds. Images are scaled down
  proportionally (never upscaled) and rotated upright using their EXIF orientation.

It's a single static Go binary, so it's easy to run in Docker or straight on a server.
Two optional command-line tools make it better when present — **exiftool** (richer
metadata) and **cwebp** (smaller WebP images); without them it falls back to a
built-in EXIF reader and JPEG.

> **Note:** photos are indexed by capture date, title, tags, and camera details read
> from **JPEG** EXIF/XMP metadata. JPEG (`.jpg` / `.jpeg`) is the supported input format.
>
> **Metadata is read with [ExifTool](https://exiftool.org/)** when the `exiftool`
> binary is available — it's the most accurate across camera makes, including Nikon
> and Canon lens names and edited/exported files. If ExifTool isn't installed the app
> still runs, falling back to a built-in reader with less coverage (and photos whose
> capture date can't be read fall back to file modification time, which can look
> out of order). Install it with `apt install libimage-exiftool-perl` (Debian/Ubuntu),
> `apk add exiftool` (Alpine), or `brew install exiftool` (macOS). The Docker image
> includes it.

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

Requires [Go 1.23+](https://go.dev/dl/). Optionally install **ExifTool** (full
metadata) and **cwebp** (smaller WebP images) — both recommended; see the notes above.

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
- **Fast indexing.** When ExifTool is present, metadata is extracted in batches; the
  built-in fallback reader processes files in parallel and reads only the first
  ~512 KiB of each. Either way the index is cached, so only the first cold pass does
  the full work.
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
- **Thumbnails and previews.** The grid/filmstrip load small `/thumb/` images and the
  viewer loads a medium `/preview/` (≤2048px) instead of the multi-megabyte original —
  so paging through photos is fast. Both are generated once (respecting EXIF
  orientation) and cached on disk under `THUMB_CACHE`. The full original is only
  fetched via **Download → Original**.
- **WebP when available.** If the `cwebp` binary is installed, renditions are encoded
  as WebP — noticeably smaller than JPEG at the same quality (faster loads, fewer
  gradient artifacts). Without it they're JPEG. Install with
  `apt install webp` (Debian/Ubuntu), `apk add libwebp-tools` (Alpine), or
  `brew install webp` (macOS). The Docker image includes it.
- **Size/quality changes auto-refresh.** Each cached rendition's filename encodes its
  size, quality, and format, so changing `THUMB_HEIGHT`, `PREVIEW_MAX`, the quality
  settings, or the WebP/JPEG choice regenerates just the affected renditions on next
  request; the now-stale files are pruned at startup (each variant independently — e.g.
  changing `THUMB_HEIGHT` won't touch previews).
- **Generation cost & warming.** Renditions are generated **lazily on first request**
  by default, so **server startup generates nothing**. The one-time cost per photo is
  a decode + resize (≈0.5s for a 24MP image), paid on first view and cached forever
  after (~2ms). Set `WARM_CACHE=true` to pre-generate everything in the background
  after indexing (low priority, one decode per photo for both sizes) so the first
  browse is instant too. Budget roughly a few hundred MB–several GB of disk for a
  large library, and expect the warm pass to run for a while in the background on
  first start (it's incremental — later runs only handle new photos).

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
| `THUMB_CACHE` | `gallery-thumbs` | Directory for generated thumbnails and previews (`/cache/thumbs` in the Docker image). Set empty to generate them on the fly without caching |
| `WARM_CACHE` | `false` | Pre-generate all thumbnails/previews in the background after indexing, so the first browse is instant (costs CPU + disk up front — see below) |
| `THUMB_HEIGHT` | `512` | Thumbnail max height in px (grid + filmstrip) |
| `THUMB_QUALITY` | `82` | Thumbnail JPEG/WebP quality (1–100) |
| `PREVIEW_MAX` | `2048` | Preview max longest edge in px (fullscreen viewer image) |
| `PREVIEW_QUALITY` | `85` | Preview JPEG/WebP quality (1–100) |
| `DOWNLOAD_SIZES` | `720p:1280x720,1080p:…` | Download presets as `label:WxH` pairs, comma-separated. "Original" is always also offered |
| `GALLERY_ENV_FILE` | `.env` | Path to the env file to load at startup (must be a real env var, not set in `.env`) |

## Endpoints

| Route | Description |
|---|---|
| `GET /` | Main feed (all photos, newest first). `?q=` full-text filter, `?tag=` exact-keyword filter. |
| `GET /albums/{album}` | An album and its sub-albums. Combines with `?q=` and `?tag=`. |
| `GET /media/{path}` | Serves the original image file. |
| `GET /thumb/{path}` | Small cached JPEG thumbnail (used by the grid/filmstrip). |
| `GET /preview/{path}` | Medium cached JPEG (≤2048px) shown in the viewer instead of the original. |
| `GET /download/{path}?res=1080p` | Download resized to `720p`, `1080p`, `1440p`, `4k`, or `original`. |
| `GET /healthz` | Health check; returns `ok`. |

## License

[MIT](LICENSE)
