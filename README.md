# photo-gallery

Simple public web photo gallery in Go.

## Features

- scans a photo directory and turns folders into albums
- serves JPEGs without authentication
- sorts photos newest-first using EXIF capture time, with file time as a fallback
- searches album names and embedded XMP/EXIF tag metadata
- ships with a small Docker image that fits behind Caddy or another reverse proxy

## Run locally

```bash
mkdir -p ./photos
PHOTO_ROOT=./photos go run .
```

Then open http://localhost:8080.

## Docker

```bash
docker build -t photo-gallery .
docker run --rm -p 8080:8080 -v "$PWD/photos:/photos:ro" photo-gallery
```

## Reverse proxy with Caddy

```caddyfile
gallery.example.com {
	reverse_proxy 127.0.0.1:8080
}
```
