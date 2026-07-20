'use strict';

const express = require('express');
const rateLimit = require('express-rate-limit');
const path = require('path');
const fs = require('fs');
const sharp = require('sharp');

const app = express();
const PORT = process.env.PORT || 3000;
const PHOTOS_DIR = path.join(__dirname, 'photos');

const IMAGE_EXTENSIONS = new Set(['.jpg', '.jpeg', '.png', '.webp', '.gif', '.tiff']);

const MIME_TYPES = {
  '.jpg':  'image/jpeg',
  '.jpeg': 'image/jpeg',
  '.png':  'image/png',
  '.webp': 'image/webp',
  '.gif':  'image/gif',
  '.tiff': 'image/tiff',
};

const RESOLUTIONS = {
  '720p':     { width: 1280, height: 720 },
  '1080p':    { width: 1920, height: 1080 },
  '1440p':    { width: 2560, height: 1440 },
  '4k':       { width: 3840, height: 2160 },
  'original': null,
};

const limiter = rateLimit({
  windowMs: 60 * 1000,
  max: 60,
  standardHeaders: true,
  legacyHeaders: false,
});

app.use(express.static(path.join(__dirname, 'public')));

// Sanitize a user-supplied filename to just a plain filename (no path traversal).
function safeFilename(raw) {
  return path.basename(raw);
}

// GET /api/photos - list all photos
app.get('/api/photos', limiter, async (req, res) => {
  try {
    const files = await fs.promises.readdir(PHOTOS_DIR);
    const photos = files
      .filter((f) => IMAGE_EXTENSIONS.has(path.extname(f).toLowerCase()))
      .map((f) => ({ name: f, url: `/photos/${encodeURIComponent(f)}` }));
    res.json(photos);
  } catch (err) {
    res.status(500).json({ error: 'Could not read photos directory' });
  }
});

// GET /photos/:filename - serve original photo
app.get('/photos/:filename', limiter, (req, res) => {
  const filename = safeFilename(req.params.filename);
  const filepath = path.join(PHOTOS_DIR, filename);

  const ext = path.extname(filename).toLowerCase();
  const mimeType = MIME_TYPES[ext] || 'application/octet-stream';
  res.setHeader('Content-Type', mimeType);

  const stream = fs.createReadStream(filepath);
  stream.on('error', (err) => {
    if (!res.headersSent) {
      res.status(err.code === 'ENOENT' ? 404 : 500).send('Photo not found');
    }
  });
  stream.pipe(res);
});

// GET /download/:filename?resolution=1080p - download photo at given resolution
app.get('/download/:filename', limiter, (req, res) => {
  const filename = safeFilename(req.params.filename);
  const resolution = req.query.resolution || 'original';
  const filepath = path.join(PHOTOS_DIR, filename);

  const ext = path.extname(filename).toLowerCase();
  if (!IMAGE_EXTENSIONS.has(ext)) {
    return res.status(400).send('Unsupported file type');
  }

  if (!Object.prototype.hasOwnProperty.call(RESOLUTIONS, resolution)) {
    return res.status(400).send('Unknown resolution. Valid options: ' + Object.keys(RESOLUTIONS).join(', '));
  }

  const base = path.basename(filename, ext);
  const downloadName = resolution === 'original' ? filename : `${base}-${resolution}.jpg`;
  res.setHeader('Content-Disposition', `attachment; filename="${downloadName}"`);

  if (resolution === 'original') {
    const mimeType = MIME_TYPES[ext] || 'application/octet-stream';
    res.setHeader('Content-Type', mimeType);
    const stream = fs.createReadStream(filepath);
    stream.on('error', (err) => {
      if (!res.headersSent) {
        res.status(err.code === 'ENOENT' ? 404 : 500).send('Photo not found');
      }
    });
    stream.pipe(res);
    return;
  }

  const { width, height } = RESOLUTIONS[resolution];
  res.setHeader('Content-Type', 'image/jpeg');
  const resizer = sharp(filepath)
    .resize(width, height, { fit: 'inside', withoutEnlargement: true })
    .toFormat('jpeg', { quality: 90 });
  resizer.on('error', (err) => {
    if (!res.headersSent) {
      res.status(err.code === 'ENOENT' ? 404 : 500).send('Could not process photo');
    }
  });
  resizer.pipe(res);
});

app.listen(PORT, () => {
  console.log(`Photo gallery running at http://localhost:${PORT}`);
});
