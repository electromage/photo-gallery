package gallery

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHandleThumbShrinksAndCaches(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "trip"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := plainJPEG(t, 4000, 3000)
	if err := os.WriteFile(filepath.Join(root, "trip", "big.jpg"), orig, 0o644); err != nil {
		t.Fatal(err)
	}
	thumbDir := filepath.Join(t.TempDir(), "thumbs")
	g, err := New(Config{PhotoRoot: root, ThumbCache: thumbDir})
	if err != nil {
		t.Fatal(err)
	}
	g.cwebp = "" // force JPEG so the assertions below are format-deterministic

	req := httptest.NewRequest(http.MethodGet, "/thumb/trip/big.jpg", nil)
	rec := httptest.NewRecorder()
	g.HandleThumb(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", ct)
	}
	body := rec.Body.Bytes()
	if len(body) >= len(orig) {
		t.Errorf("thumbnail (%d bytes) is not smaller than original (%d bytes)", len(body), len(orig))
	}
	img, err := jpeg.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("decode thumb: %v", err)
	}
	if h := img.Bounds().Dy(); h != defaultThumbHeight {
		t.Errorf("thumb height = %d, want %d", h, defaultThumbHeight)
	}

	// It should have been cached to disk.
	entries, _ := os.ReadDir(thumbDir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 cached thumbnail, found %d", len(entries))
	}
}

func TestHandleThumbRejectsTraversal(t *testing.T) {
	g, _ := newTestGallery(t)
	req := httptest.NewRequest(http.MethodGet, "/thumb/..%2f..%2fetc%2fpasswd", nil)
	rec := httptest.NewRecorder()
	g.HandleThumb(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandlePreviewBoundsLongestEdge(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "wide.jpg"), plainJPEG(t, 6000, 4000), 0o644); err != nil {
		t.Fatal(err)
	}
	thumbDir := filepath.Join(t.TempDir(), "cache")
	g, err := New(Config{PhotoRoot: root, ThumbCache: thumbDir})
	if err != nil {
		t.Fatal(err)
	}

	g.cwebp = "" // force JPEG so the assertions below are format-deterministic
	req := httptest.NewRequest(http.MethodGet, "/preview/wide.jpg", nil)
	rec := httptest.NewRecorder()
	g.HandlePreview(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	img, err := jpeg.Decode(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	b := img.Bounds()
	if b.Dx() != defaultPreviewMax {
		t.Errorf("preview longest edge = %d, want %d", b.Dx(), defaultPreviewMax)
	}
	if b.Dx() > defaultPreviewMax || b.Dy() > defaultPreviewMax {
		t.Errorf("preview %dx%d exceeds %d box", b.Dx(), b.Dy(), defaultPreviewMax)
	}
}

func TestWarmCacheGeneratesBothVariants(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.jpg"), plainJPEG(t, 3000, 2000), 0o644); err != nil {
		t.Fatal(err)
	}
	thumbDir := filepath.Join(t.TempDir(), "cache")
	g, err := New(Config{PhotoRoot: root, ThumbCache: thumbDir, WarmCache: true})
	if err != nil {
		t.Fatal(err)
	}
	waitForIndex(t, g)

	// Rescan triggers warming asynchronously; poll for both renditions to appear.
	deadline := time.Now().Add(3 * time.Second)
	for {
		entries, _ := os.ReadDir(thumbDir)
		if len(entries) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected 2 cached renditions (thumb+preview), got %d", len(entries))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSizeChangeRefreshesCache(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.jpg"), plainJPEG(t, 2000, 1500), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "cache")

	reqThumb := func(g *Gallery) image.Image {
		t.Helper()
		g.cwebp = "" // deterministic JPEG
		rec := httptest.NewRecorder()
		g.HandleThumb(rec, httptest.NewRequest(http.MethodGet, "/thumb/a.jpg", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		img, err := jpeg.Decode(bytes.NewReader(rec.Body.Bytes()))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		return img
	}

	// Default height (512) → cache one rendition.
	g1, err := New(Config{PhotoRoot: root, ThumbCache: dir})
	if err != nil {
		t.Fatal(err)
	}
	if h := reqThumb(g1).Bounds().Dy(); h != defaultThumbHeight {
		t.Fatalf("first thumb height = %d, want %d", h, defaultThumbHeight)
	}

	// New gallery, same cache dir, different height. Pruning should drop the old
	// rendition, and a request must regenerate at the new height.
	g2, err := New(Config{PhotoRoot: root, ThumbCache: dir, ThumbHeight: 300})
	if err != nil {
		t.Fatal(err)
	}
	g2.cwebp = ""
	g2.pruneStaleCache()

	names, _ := os.ReadDir(dir)
	for _, n := range names {
		if !n.IsDir() && !strings.Contains(n.Name(), g2.thumbV.sig()) && n.Name()[:2] == "t_" {
			t.Errorf("stale rendition not pruned: %s", n.Name())
		}
	}
	if h := reqThumb(g2).Bounds().Dy(); h != 300 {
		t.Fatalf("second thumb height = %d, want 300", h)
	}
}

func TestVariantCacheNameUsesDeterministicSHA256(t *testing.T) {
	v := variant{tag: "t", maxW: 1024, maxH: 512, quality: 80}
	rel := "trip/photo.jpg"

	got := v.cacheName(rel, "jpg")
	sum := sha256.Sum256([]byte(rel))
	expectedPrefix := "t_" + hex.EncodeToString(sum[:]) + "_"
	if !strings.HasPrefix(got, expectedPrefix) {
		t.Fatalf("cacheName prefix = %q, want prefix %q", got, expectedPrefix)
	}
	if !strings.HasSuffix(got, ".jpg") {
		t.Fatalf("cacheName suffix = %q, want .jpg", got)
	}
	if len(v.sig()) != 8 {
		t.Fatalf("sig length = %d, want 8", len(v.sig()))
	}
}
