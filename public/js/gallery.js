'use strict';

const RESOLUTIONS = [
  { key: 'original', label: 'Original', dims: 'Full size' },
  { key: '720p',     label: '720p',     dims: '1280 × 720' },
  { key: '1080p',    label: '1080p',    dims: '1920 × 1080' },
  { key: '1440p',    label: '1440p',    dims: '2560 × 1440' },
  { key: '4k',       label: '4K',       dims: '3840 × 2160' },
];

const gallery       = document.getElementById('gallery');
const lightbox      = document.getElementById('lightbox');
const lightboxImg   = document.getElementById('lightbox-img');
const lightboxTitle = document.getElementById('lightbox-title');
const downloadBtns  = document.getElementById('download-buttons');
const closeBtn      = document.getElementById('lightbox-close');

let currentPhoto = null;

async function loadPhotos() {
  try {
    const res = await fetch('/api/photos');
    if (!res.ok) throw new Error(`Failed to fetch photos: ${res.status} ${res.statusText}`);
    const photos = await res.json();

    gallery.innerHTML = '';

    if (photos.length === 0) {
      gallery.innerHTML = '<p class="empty-message">No photos found. Add images to the <code>photos/</code> folder to get started.</p>';
      return;
    }

    photos.forEach((photo) => {
      const item = document.createElement('li');
      item.className = 'gallery-item';
      item.setAttribute('role', 'listitem');
      item.setAttribute('tabindex', '0');
      item.setAttribute('aria-label', `Open ${photo.name}`);

      const img = document.createElement('img');
      img.src = photo.url;
      img.alt = photo.name;
      img.loading = 'lazy';

      const overlay = document.createElement('div');
      overlay.className = 'overlay';
      const label = document.createElement('span');
      label.className = 'overlay-label';
      label.textContent = photo.name;
      overlay.appendChild(label);

      item.appendChild(img);
      item.appendChild(overlay);

      item.addEventListener('click', () => openLightbox(photo));
      item.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          openLightbox(photo);
        }
      });

      gallery.appendChild(item);
    });
  } catch (err) {
    gallery.innerHTML = '<p class="empty-message">Could not load photos. Make sure the server is running.</p>';
    console.error(err);
  }
}

function openLightbox(photo) {
  currentPhoto = photo;
  lightboxImg.src = photo.url;
  lightboxImg.alt = photo.name;
  lightboxTitle.textContent = photo.name;

  downloadBtns.innerHTML = '';
  RESOLUTIONS.forEach(({ key, label, dims }) => {
    const a = document.createElement('a');
    a.className = 'download-btn';
    a.href = `/download/${encodeURIComponent(photo.name)}?resolution=${key}`;
    a.download = '';
    a.setAttribute('aria-label', `Download ${photo.name} at ${label} (${dims})`);

    const resLabel = document.createElement('span');
    resLabel.className = 'res-label';
    resLabel.textContent = label;

    const resDims = document.createElement('span');
    resDims.className = 'res-dims';
    resDims.textContent = dims;

    a.appendChild(resLabel);
    a.appendChild(resDims);
    downloadBtns.appendChild(a);
  });

  lightbox.classList.remove('hidden');
  document.body.style.overflow = 'hidden';
  closeBtn.focus();
}

function closeLightbox() {
  lightbox.classList.add('hidden');
  document.body.style.overflow = '';
  currentPhoto = null;
}

closeBtn.addEventListener('click', closeLightbox);

lightbox.addEventListener('click', (e) => {
  if (e.target === lightbox) closeLightbox();
});

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && !lightbox.classList.contains('hidden')) {
    closeLightbox();
  }
});

loadPhotos();
