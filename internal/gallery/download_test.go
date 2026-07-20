package gallery

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// plainJPEG returns an encoded JPEG of the given dimensions filled with a
// diagonal gradient (so resizing has something to interpolate).
func plainJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

func newTestGallery(t *testing.T) (*Gallery, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "trip"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "trip", "big.jpg"), plainJPEG(t, 4000, 3000), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := New(root, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g, root
}

func TestHandleDownloadResizesToFitBox(t *testing.T) {
	g, _ := newTestGallery(t)

	req := httptest.NewRequest(http.MethodGet, "/download/trip/big.jpg?res=1080p", nil)
	rec := httptest.NewRecorder()
	g.HandleDownload(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd == "" {
		t.Error("missing Content-Disposition")
	}

	img, err := jpeg.Decode(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	b := img.Bounds()
	if b.Dx() > 1920 || b.Dy() > 1080 {
		t.Errorf("resized to %dx%d, exceeds 1920x1080 box", b.Dx(), b.Dy())
	}
	// 4000x3000 into 1920x1080 is height-bound: 1440x1080.
	if b.Dy() != 1080 {
		t.Errorf("height = %d, want 1080 (height-bound fit)", b.Dy())
	}
	// Aspect ratio (4:3) must be preserved.
	if got := float64(b.Dx()) / float64(b.Dy()); got < 1.32 || got > 1.34 {
		t.Errorf("aspect ratio = %.3f, want ~1.333", got)
	}
}

func TestHandleDownloadOriginalIsUntouched(t *testing.T) {
	g, root := newTestGallery(t)
	original, err := os.ReadFile(filepath.Join(root, "trip", "big.jpg"))
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/download/trip/big.jpg?res=original", nil)
	rec := httptest.NewRecorder()
	g.HandleDownload(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), original) {
		t.Error("original download does not match source bytes")
	}
}

func TestHandleDownloadNeverUpscales(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "small.jpg"), plainJPEG(t, 800, 600), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := New(root, "")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/download/small.jpg?res=4k", nil)
	rec := httptest.NewRecorder()
	g.HandleDownload(rec, req)

	img, err := jpeg.Decode(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 800 || b.Dy() != 600 {
		t.Errorf("size = %dx%d, want 800x600 (no upscaling)", b.Dx(), b.Dy())
	}
}

func TestHandleDownloadRejectsBadRequests(t *testing.T) {
	g, _ := newTestGallery(t)

	cases := []struct {
		name string
		path string
		want int
	}{
		{"traversal", "/download/../../../etc/passwd", http.StatusNotFound},
		{"encoded traversal", "/download/..%2f..%2fetc%2fpasswd", http.StatusNotFound},
		{"missing file", "/download/trip/nope.jpg", http.StatusNotFound},
		{"non-jpeg", "/download/trip/big.png", http.StatusNotFound},
		{"unknown resolution", "/download/trip/big.jpg?res=8k", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rec := httptest.NewRecorder()
			g.HandleDownload(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestApplyOrientationRotates(t *testing.T) {
	// A 2-wide, 1-tall image rotated 90° CW (orientation 6) becomes 1-wide, 2-tall.
	src := image.NewRGBA(image.Rect(0, 0, 2, 1))
	src.Set(0, 0, color.RGBA{255, 0, 0, 255})
	src.Set(1, 0, color.RGBA{0, 255, 0, 255})

	out := applyOrientation(src, 6)
	if b := out.Bounds(); b.Dx() != 1 || b.Dy() != 2 {
		t.Fatalf("rotated size = %dx%d, want 1x2", b.Dx(), b.Dy())
	}
	// Left pixel (red) moves to the top after a clockwise rotation.
	r, _, _, _ := out.At(0, 0).RGBA()
	if r>>8 < 200 {
		t.Errorf("top pixel not red after rotate; r=%d", r>>8)
	}
}
