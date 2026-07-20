# photo-gallery
Simple Web Photo Gallery

A lightweight Node.js photo gallery that lets visitors browse photos and download them in a variety of resolutions perfect for desktop backgrounds.

## Features

- Grid gallery of all photos in the `photos/` directory
- Lightbox viewer with keyboard and click-to-close support
- One-click download in multiple resolutions:
  - **720p** — 1280 × 720
  - **1080p** — 1920 × 1080
  - **1440p** — 2560 × 1440
  - **4K** — 3840 × 2160
  - **Original** — full-size unmodified file

Images are resized on the fly using [Sharp](https://sharp.pixelplumbing.com/), scaled down proportionally (never upscaled).

## Getting started

```bash
npm install
```

Add your photos (JPEG, PNG, WebP, GIF, TIFF) to the `photos/` directory, then start the server:

```bash
npm start
```

Open <http://localhost:3000> in your browser.

## Configuration

| Environment variable | Default | Description |
|---|---|---|
| `PORT` | `3000` | Port the server listens on |
