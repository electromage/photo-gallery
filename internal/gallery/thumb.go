package gallery

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// variant is a cached, resized rendition of a photo. Two are served: a small
// thumbnail for the browsing grid/filmstrip, and a larger preview for the viewer
// (so paging through photos doesn't download the multi-megabyte originals).
type variant struct {
	tag     string // cache-file prefix, keeps variants from colliding
	maxW    int
	maxH    int
	quality int
}

// Defaults used when the corresponding env vars are unset. The thumbnail is
// height-bound (the grid lays out fixed-height rows); the preview is bound by its
// longest edge — big enough for full-screen viewing but a fraction of the original.
const (
	defaultThumbHeight    = 512
	defaultThumbQuality   = 82
	defaultPreviewMax     = 2048
	defaultPreviewQuality = 85
)

// clampQuality keeps a JPEG/WebP quality within 1-100, applying def for non-positive.
func clampQuality(q, def int) int {
	if q <= 0 {
		return def
	}
	if q > 100 {
		return 100
	}
	return q
}

// positiveOr returns v when positive, otherwise def.
func positiveOr(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

func (v variant) cacheName(relSlash, ext string) string {
	sum := sha256.Sum256([]byte(relSlash))
	return v.tag + "_" + hex.EncodeToString(sum[:]) + "_" + v.sig() + "." + ext
}

// sig is a short signature of the variant's resize parameters, embedded in cache
// filenames so that changing the size or quality invalidates old renditions (a
// changed sig produces a new filename, missing the cache).
func (v variant) sig() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%d", v.maxW, v.maxH, v.quality)))
	return hex.EncodeToString(sum[:4])
}

// pruneStaleCache removes cached renditions whose variant signature or format no
// longer matches the current config, so changing THUMB_HEIGHT / PREVIEW_MAX /
// quality (or switching WebP/JPEG) frees the old files instead of orphaning them.
func (g *Gallery) pruneStaleCache() {
	if g.thumbDir == "" {
		return
	}
	ext := g.imageExt()
	thumbSuffix := "_" + g.thumbV.sig() + "." + ext
	previewSuffix := "_" + g.previewV.sig() + "." + ext

	entries, err := os.ReadDir(g.thumbDir)
	if err != nil {
		return
	}
	removed := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasSuffix(name, ".tmp") {
			continue
		}
		var want string
		switch {
		case strings.HasPrefix(name, "t_"):
			want = thumbSuffix
		case strings.HasPrefix(name, "p_"):
			want = previewSuffix
		default:
			continue
		}
		if !strings.HasSuffix(name, want) {
			if os.Remove(filepath.Join(g.thumbDir, name)) == nil {
				removed++
			}
		}
	}
	if removed > 0 {
		log.Printf("pruned %d stale cached renditions", removed)
	}
}

// HandleThumb serves a small thumbnail (grid/filmstrip).
func (g *Gallery) HandleThumb(w http.ResponseWriter, r *http.Request) {
	g.serveVariant(w, r, "/thumb/", g.thumbV)
}

// HandlePreview serves a larger preview for the viewer.
func (g *Gallery) HandlePreview(w http.ResponseWriter, r *http.Request) {
	g.serveVariant(w, r, "/preview/", g.previewV)
}

func (g *Gallery) serveVariant(w http.ResponseWriter, r *http.Request, prefix string, v variant) {
	rel := strings.TrimPrefix(r.URL.Path, prefix)
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

	// Serve from the disk cache when a fresh rendition (in the current format)
	// already exists.
	if g.thumbDir != "" {
		ext := g.imageExt()
		cached := filepath.Join(g.thumbDir, v.cacheName(relSlash, ext))
		if ci, err := os.Stat(cached); err == nil && !ci.ModTime().Before(info.ModTime()) {
			w.Header().Set("Content-Type", contentTypeForExt(ext))
			http.ServeFile(w, r, cached)
			return
		}
	}

	// Bound concurrent decodes: a large original balloons in memory while decoded
	// and resized, so hold a render slot for that work (released before we write
	// the response, which needs no slot).
	g.acquireRender()
	img, err := loadImageOriented(abs, g.photoOrient(relSlash))
	if err != nil {
		g.releaseRender()
		http.Error(w, "could not read image", http.StatusInternalServerError)
		return
	}
	data, ext, err := g.encodeImage(fitInside(img, v.maxW, v.maxH), v.quality)
	g.releaseRender()
	if err != nil {
		http.Error(w, "could not render image", http.StatusInternalServerError)
		return
	}
	if g.thumbDir != "" {
		writeCacheFile(g.thumbDir, v.cacheName(relSlash, ext), data)
	}

	w.Header().Set("Content-Type", contentTypeForExt(ext))
	_, _ = w.Write(data)
}

// imageExt is the file extension/format used for generated renditions.
func (g *Gallery) imageExt() string {
	if g.cwebp != "" {
		return "webp"
	}
	return "jpg"
}

func contentTypeForExt(ext string) string {
	if ext == "webp" {
		return "image/webp"
	}
	return "image/jpeg"
}

// encodeImage encodes img as WebP when cwebp is available (smaller at equal
// quality), otherwise JPEG. Returns the bytes and the format extension used.
func (g *Gallery) encodeImage(img image.Image, quality int) ([]byte, string, error) {
	if g.cwebp != "" {
		if data, err := encodeWebP(g.cwebp, img, quality); err == nil {
			return data, "webp", nil
		} else {
			log.Printf("webp encode failed, falling back to JPEG: %v", err)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), "jpg", nil
}

// encodeWebP pipes a lossless PNG of img through cwebp and returns the WebP bytes.
func encodeWebP(cwebpPath string, img image.Image, quality int) ([]byte, error) {
	var in bytes.Buffer
	if err := png.Encode(&in, img); err != nil {
		return nil, err
	}
	// "-o -" writes to stdout; "-- -" reads the PNG from stdin.
	cmd := exec.Command(cwebpPath, "-quiet", "-q", strconv.Itoa(quality), "-o", "-", "--", "-")
	cmd.Stdin = &in
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("cwebp: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	if out.Len() == 0 {
		return nil, fmt.Errorf("cwebp produced no output")
	}
	return out.Bytes(), nil
}

// writeCacheFile atomically writes a rendition into the cache dir. Best-effort:
// any failure is ignored (the image was already served from memory).
func writeCacheFile(dir, name string, data []byte) {
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
