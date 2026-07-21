package gallery

import (
	"bytes"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
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
	if h := img.Bounds().Dy(); h != thumbHeight {
		t.Errorf("thumb height = %d, want %d", h, thumbHeight)
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
