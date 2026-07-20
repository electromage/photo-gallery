'use strict';

const express = require('express');
const path = require('path');
const fs = require('fs');
const sharp = require('sharp');

const app = express();
const PORT = process.env.PORT || 3000;
const PHOTOS_DIR = path.join(__dirname, 'photos');

const IMAGE_EXTENSIONS = new Set(['.jpg', '.jpeg', '.png', '.webp', '.gif', '.tiff']);

const RESOLUTIONS = {
  '720p':     { width: 1280, height: 720 },
  '1080p':    { width: 1920, height: 1080 },
  '1440p':    { width: 2560, height: 1440 },
  '4k':       { width: 3840, height: 2160 },
  'original': null,
};

app.use(express.static(path.join(__dirname, 'public')));

// GET /api/photos - list all photos
app.get('/api/photos', (req, res) => {
  fs.readdir(PHOTOS_DIR, (err, files) => {
    if (err) {
      return res.status(500).json({ error: 'Could not read photos directory' });
    }
    const photos = files
      .filter((f) => IMAGE_EXTENSIONS.has(path.extname(f).toLowerCase()))
      .map((f) => ({ name: f, url: `/photos/${encodeURIComponent(f)}` }));
    res.json(photos);
  });
});

// GET /photos/:filename - serve original photo
app.get('/photos/:filename', (req, res) => {
  const filename = req.params.filename;
  const filepath = path.join(PHOTOS_DIR, filename);

  // Prevent path traversal
  if (!filepath.startsWith(PHOTOS_DIR + path.sep) && filepath !== PHOTOS_DIR) {
    return res.status(400).send('Invalid filename');
  }

  if (!fs.existsSync(filepath)) {
    return res.status(404).send('Photo not found');
  }

  res.sendFile(filepath);
});

// GET /download/:filename?resolution=1080p - download photo at given resolution
app.get('/download/:filename', (req, res) => {
  const filename = req.params.filename;
  const resolution = req.query.resolution || 'original';
  const filepath = path.join(PHOTOS_DIR, filename);

  // Prevent path traversal
  if (!filepath.startsWith(PHOTOS_DIR + path.sep) && filepath !== PHOTOS_DIR) {
    return res.status(400).send('Invalid filename');
  }

  if (!IMAGE_EXTENSIONS.has(path.extname(filename).toLowerCase())) {
    return res.status(400).send('Unsupported file type');
  }

  if (!fs.existsSync(filepath)) {
    return res.status(404).send('Photo not found');
  }

  if (!Object.prototype.hasOwnProperty.call(RESOLUTIONS, resolution)) {
    return res.status(400).send('Unknown resolution. Valid options: ' + Object.keys(RESOLUTIONS).join(', '));
  }

  const ext = path.extname(filename).toLowerCase();
  const base = path.basename(filename, ext);
  const downloadName = resolution === 'original' ? filename : `${base}-${resolution}${ext}`;

  res.setHeader('Content-Disposition', `attachment; filename="${downloadName}"`);

  if (resolution === 'original') {
    res.setHeader('Content-Type', 'image/jpeg');
    fs.createReadStream(filepath).pipe(res);
    return;
  }

  const { width, height } = RESOLUTIONS[resolution];
  sharp(filepath)
    .resize(width, height, { fit: 'inside', withoutEnlargement: true })
    .toFormat('jpeg', { quality: 90 })
    .pipe(res);
});

app.listen(PORT, () => {
  console.log(`Photo gallery running at http://localhost:${PORT}`);
});
