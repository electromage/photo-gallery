package gallery

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"image/jpeg"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// thumbHeight is the max height of a generated grid/filmstrip thumbnail. Sized to
// stay crisp on high-DPI displays at the grid's ~320px render height while keeping
// files small (tens of KB) so a large library loads quickly.
const thumbHeight = 512

// HandleThumb serves a small JPEG thumbnail of a photo, generated once and cached
// on disk. Unlike /media (which serves the full original), this keeps the browsing
// grid lightweight.
func (g *Gallery) HandleThumb(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/thumb/")
	if unescaped, err := url.PathUnescape(rel); err == nil {
		rel = unescaped
	}

	abs, relSlash, ok := g.resolvePhoto(rel)
	if !ok {
		http.NotFound(w, r)
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Cache-Control", "public, max-age=604800") // 7 days

	// Serve from the disk cache when a fresh thumbnail already exists.
	if g.thumbDir != "" {
		cached := filepath.Join(g.thumbDir, thumbKey(relSlash))
		if ci, err := os.Stat(cached); err == nil && !ci.ModTime().Before(info.ModTime()) {
			http.ServeFile(w, r, cached)
			return
		}
	}

	data, err := renderThumb(abs, g.photoOrient(relSlash))
	if err != nil {
		http.Error(w, "could not render thumbnail", http.StatusInternalServerError)
		return
	}
	if g.thumbDir != "" {
		writeThumbCache(g.thumbDir, thumbKey(relSlash), data)
	}

	w.Header().Set("Content-Type", "image/jpeg")
	_, _ = w.Write(data)
}

// renderThumb decodes a photo, applies the given EXIF orientation, scales it down
// to thumbHeight (never up), and encodes it as JPEG.
func renderThumb(absPath string, orient int) ([]byte, error) {
	img, err := loadImageOriented(absPath, orient)
	if err != nil {
		return nil, err
	}
	small := fitInside(img, 1<<20, thumbHeight)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, small, &jpeg.Options{Quality: 82}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func thumbKey(relSlash string) string {
	sum := sha1.Sum([]byte(relSlash))
	return hex.EncodeToString(sum[:]) + ".jpg"
}

// writeThumbCache atomically writes a thumbnail into the cache dir. Best-effort:
// any failure is ignored (the thumbnail was already served from memory).
func writeThumbCache(dir, name string, data []byte) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, name+".*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return
	}
	_ = os.Rename(tmpName, filepath.Join(dir, name))
}
