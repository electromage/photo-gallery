package gallery

import (
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DownloadSize is a named target that a photo can be downscaled to before
// download. Width/Height are the bounding box; images are scaled to fit inside
// it proportionally and are never enlarged.
type DownloadSize struct {
	Label  string
	Width  int
	Height int
}

// DefaultDownloadSizes are the resolutions offered when DOWNLOAD_SIZES is unset.
// "original" is handled separately (served untouched) and is not listed here.
var DefaultDownloadSizes = []DownloadSize{
	{Label: "720p", Width: 1280, Height: 720},
	{Label: "1080p", Width: 1920, Height: 1080},
	{Label: "1440p", Width: 2560, Height: 1440},
	{Label: "4k", Width: 3840, Height: 2160},
}

// ParseDownloadSizes parses a spec like "720p:1280x720,1080p:1920x1080" into sizes.
// An empty spec yields the defaults; malformed input returns an error so the caller
// can fall back.
func ParseDownloadSizes(spec string) ([]DownloadSize, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return DefaultDownloadSizes, nil
	}
	var out []DownloadSize
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		colon := strings.LastIndex(part, ":")
		if colon < 0 {
			return nil, fmt.Errorf("bad size %q (want label:WxH)", part)
		}
		label := strings.TrimSpace(part[:colon])
		dims := strings.TrimSpace(part[colon+1:])
		x := strings.IndexAny(dims, "xX")
		if label == "" || x < 0 {
			return nil, fmt.Errorf("bad size %q (want label:WxH)", part)
		}
		w, err1 := strconv.Atoi(strings.TrimSpace(dims[:x]))
		h, err2 := strconv.Atoi(strings.TrimSpace(dims[x+1:]))
		if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
			return nil, fmt.Errorf("bad dimensions in %q", part)
		}
		out = append(out, DownloadSize{Label: label, Width: w, Height: h})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no sizes parsed from %q", spec)
	}
	return out, nil
}

func (g *Gallery) downloadSizeByLabel(label string) (DownloadSize, bool) {
	for _, size := range g.downloadSizes {
		if size.Label == label {
			return size, true
		}
	}
	return DownloadSize{}, false
}

// HandleDownload serves a photo for download, optionally resized to a named
// resolution via the `res` query parameter (e.g. /download/trip/beach.jpg?res=1080p).
// Without `res`, or with res=original, the untouched file is streamed.
func (g *Gallery) HandleDownload(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/download/")
	if unescaped, err := url.PathUnescape(rel); err == nil {
		rel = unescaped
	}

	absPath, relSlash, ok := g.resolvePhoto(rel)
	if !ok {
		http.NotFound(w, r)
		return
	}

	res := r.URL.Query().Get("res")
	baseName := filepath.Base(relSlash)

	// Original: stream the file as-is.
	if res == "" || res == "original" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", baseName))
		http.ServeFile(w, r, absPath)
		return
	}

	size, known := g.downloadSizeByLabel(res)
	if !known {
		http.Error(w, "unknown resolution", http.StatusBadRequest)
		return
	}

	// Bound concurrent decodes (see serveVariant): downloads decode the full-size
	// original, which is the most memory-hungry path of all.
	g.acquireRender()
	img, err := loadImageOriented(absPath, g.photoOrient(relSlash))
	if err != nil {
		g.releaseRender()
		http.Error(w, "could not read image", http.StatusInternalServerError)
		return
	}
	resized := fitInside(img, size.Width, size.Height)
	g.releaseRender()

	stem := strings.TrimSuffix(baseName, filepath.Ext(baseName))
	downloadName := fmt.Sprintf("%s-%s.jpg", stem, size.Label)
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", downloadName))
	if err := jpeg.Encode(w, resized, &jpeg.Options{Quality: 90}); err != nil {
		// Headers may already be sent; nothing safe to do but stop.
		return
	}
}

// resolvePhoto validates a request-supplied relative path and maps it to an
// absolute path inside the gallery root. It rejects traversal outside the root,
// non-JPEG files, and anything that is not a regular file.
func (g *Gallery) resolvePhoto(rel string) (absPath, relSlash string, ok bool) {
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "/")
	if rel == "" || !jpgPattern.MatchString(rel) {
		return "", "", false
	}

	cleaned := filepath.Clean(filepath.FromSlash(rel))
	if cleaned == "." || strings.HasPrefix(cleaned, "..") {
		return "", "", false
	}

	abs := filepath.Join(g.root, cleaned)

	// Defense in depth: ensure the resolved path is still under the root.
	relToRoot, err := filepath.Rel(g.root, abs)
	if err != nil || relToRoot == ".." || strings.HasPrefix(relToRoot, ".."+string(filepath.Separator)) {
		return "", "", false
	}

	info, err := os.Stat(abs)
	if err != nil || !info.Mode().IsRegular() {
		return "", "", false
	}

	return abs, filepath.ToSlash(cleaned), true
}

// loadImageOriented decodes a JPEG and applies the given EXIF orientation so the
// re-encoded output is upright (re-encoding otherwise discards EXIF). The
// orientation comes from the index, which was read by exiftool or the built-in
// reader when the photo was scanned.
func loadImageOriented(path string, orient int) (image.Image, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	img, err := jpeg.Decode(file)
	if err != nil {
		return nil, err
	}
	return applyOrientation(img, orient), nil
}

// applyOrientation returns img transformed per the EXIF orientation value (1-8).
func applyOrientation(img image.Image, orientation int) image.Image {
	switch orientation {
	case 2:
		return flipH(img)
	case 3:
		return rotate180(img)
	case 4:
		return flipV(img)
	case 5:
		return flipH(rotate90(img))
	case 6:
		return rotate90(img)
	case 7:
		return flipH(rotate270(img))
	case 8:
		return rotate270(img)
	default:
		return img
	}
}

// toRGBA converts any image to *image.RGBA for fast pixel access.
func toRGBA(img image.Image) *image.RGBA {
	if rgba, ok := img.(*image.RGBA); ok {
		return rgba
	}
	bounds := img.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(dst, dst.Bounds(), img, bounds.Min, draw.Src)
	return dst
}

func flipH(img image.Image) image.Image {
	src := toRGBA(img)
	w, h := src.Rect.Dx(), src.Rect.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dst.Set(w-1-x, y, src.At(src.Rect.Min.X+x, src.Rect.Min.Y+y))
		}
	}
	return dst
}

func flipV(img image.Image) image.Image {
	src := toRGBA(img)
	w, h := src.Rect.Dx(), src.Rect.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dst.Set(x, h-1-y, src.At(src.Rect.Min.X+x, src.Rect.Min.Y+y))
		}
	}
	return dst
}

func rotate180(img image.Image) image.Image {
	src := toRGBA(img)
	w, h := src.Rect.Dx(), src.Rect.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dst.Set(w-1-x, h-1-y, src.At(src.Rect.Min.X+x, src.Rect.Min.Y+y))
		}
	}
	return dst
}

// rotate90 rotates 90° clockwise.
func rotate90(img image.Image) image.Image {
	src := toRGBA(img)
	w, h := src.Rect.Dx(), src.Rect.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, h, w))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dst.Set(h-1-y, x, src.At(src.Rect.Min.X+x, src.Rect.Min.Y+y))
		}
	}
	return dst
}

// rotate270 rotates 90° counter-clockwise.
func rotate270(img image.Image) image.Image {
	src := toRGBA(img)
	w, h := src.Rect.Dx(), src.Rect.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, h, w))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dst.Set(y, w-1-x, src.At(src.Rect.Min.X+x, src.Rect.Min.Y+y))
		}
	}
	return dst
}

// fitInside scales img down proportionally to fit within maxW×maxH. Images
// already smaller than the box are returned unchanged (never upscaled).
func fitInside(img image.Image, maxW, maxH int) image.Image {
	src := toRGBA(img)
	w, h := src.Rect.Dx(), src.Rect.Dy()
	if w == 0 || h == 0 {
		return src
	}

	scale := min(float64(maxW)/float64(w), float64(maxH)/float64(h))
	if scale >= 1 {
		return src
	}

	newW := int(float64(w) * scale)
	newH := int(float64(h) * scale)
	if newW < 1 {
		newW = 1
	}
	if newH < 1 {
		newH = 1
	}

	return bilinearResize(src, newW, newH)
}

// bilinearResize downsamples src to newW×newH using bilinear interpolation.
func bilinearResize(src *image.RGBA, newW, newH int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	srcW, srcH := src.Rect.Dx(), src.Rect.Dy()

	xRatio := float64(srcW) / float64(newW)
	yRatio := float64(srcH) / float64(newH)

	for y := 0; y < newH; y++ {
		srcY := (float64(y) + 0.5) * yRatio
		y0 := int(srcY - 0.5)
		fy := srcY - 0.5 - float64(y0)
		y1 := y0 + 1
		y0 = clamp(y0, 0, srcH-1)
		y1 = clamp(y1, 0, srcH-1)

		for x := 0; x < newW; x++ {
			srcX := (float64(x) + 0.5) * xRatio
			x0 := int(srcX - 0.5)
			fx := srcX - 0.5 - float64(x0)
			x1 := x0 + 1
			x0 = clamp(x0, 0, srcW-1)
			x1 = clamp(x1, 0, srcW-1)

			c00 := pixel(src, x0, y0)
			c10 := pixel(src, x1, y0)
			c01 := pixel(src, x0, y1)
			c11 := pixel(src, x1, y1)

			var out [4]float64
			for i := 0; i < 4; i++ {
				top := float64(c00[i])*(1-fx) + float64(c10[i])*fx
				bottom := float64(c01[i])*(1-fx) + float64(c11[i])*fx
				out[i] = top*(1-fy) + bottom*fy
			}

			off := dst.PixOffset(x, y)
			dst.Pix[off+0] = clampByte(out[0])
			dst.Pix[off+1] = clampByte(out[1])
			dst.Pix[off+2] = clampByte(out[2])
			dst.Pix[off+3] = clampByte(out[3])
		}
	}
	return dst
}

func pixel(img *image.RGBA, x, y int) [4]uint8 {
	off := img.PixOffset(img.Rect.Min.X+x, img.Rect.Min.Y+y)
	return [4]uint8{img.Pix[off], img.Pix[off+1], img.Pix[off+2], img.Pix[off+3]}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampByte(v float64) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v + 0.5)
}
