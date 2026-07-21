package gallery

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildViewModelSortsByExifNewestFirstAndIndexesAlbumsAndTags(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "summer-trip"), 0o755); err != nil {
		t.Fatal(err)
	}

	newerPath := filepath.Join(root, "summer-trip", "newer.jpg")
	olderPath := filepath.Join(root, "summer-trip", "older.jpg")

	if err := os.WriteFile(newerPath, testJPEG(t, "2024:05:06 07:08:09", []string{"sunset", "beach"}, "Golden Hour"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(olderPath, testJPEG(t, "2023:01:02 03:04:05", []string{"mountain"}, "Morning Trail"), 0o644); err != nil {
		t.Fatal(err)
	}

	g, err := New(Config{PhotoRoot: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	vm := waitForIndex(t, g)

	if len(vm.Albums) != 1 {
		t.Fatalf("expected 1 album, got %d", len(vm.Albums))
	}
	if vm.Albums[0].Path != "summer-trip" {
		t.Fatalf("expected album path summer-trip, got %q", vm.Albums[0].Path)
	}
	if len(vm.Photos) != 2 {
		t.Fatalf("expected 2 photos, got %d", len(vm.Photos))
	}
	if got := vm.Photos[0].Title; got != "Golden Hour" {
		t.Fatalf("expected newest photo title Golden Hour, got %q", got)
	}
	if got := strings.Join(vm.Photos[0].Tags, ","); got != "beach,sunset" {
		t.Fatalf("expected tags beach,sunset, got %q", got)
	}
	if !vm.Photos[0].TakenAt.After(vm.Photos[1].TakenAt) {
		t.Fatal("expected photos sorted by newest EXIF date first")
	}
}

func TestFilterPhotosMatchesAlbumAndTags(t *testing.T) {
	photos := []Photo{
		{AlbumPath: "summer-trip", AlbumName: "Summer Trip", Title: "Golden Hour", Tags: []string{"beach", "sunset"}, searchText: "golden hour summer trip beach sunset"},
		{AlbumPath: "winter-hike", AlbumName: "Winter Hike", Title: "Fresh Snow", Tags: []string{"mountain"}, searchText: "fresh snow winter hike mountain"},
	}

	filtered := filterPhotos(photos, "summer-trip", "sunset")
	if len(filtered) != 1 {
		t.Fatalf("expected 1 filtered photo, got %d", len(filtered))
	}
	if filtered[0].Title != "Golden Hour" {
		t.Fatalf("unexpected photo %q", filtered[0].Title)
	}
}

func TestBuildPhotoFallsBackToFileModTimeWhenExifMissing(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "plain.jpg")
	if err := os.WriteFile(path, testPlainJPEG(t), 0o644); err != nil {
		t.Fatal(err)
	}

	want := time.Date(2022, 11, 10, 9, 8, 7, 0, time.UTC)
	if err := os.Chtimes(path, want, want); err != nil {
		t.Fatal(err)
	}

	photo := buildPhoto(path, "plain.jpg", "", want, metadata{})
	if !photo.TakenAt.Equal(want) {
		t.Fatalf("expected modtime fallback %s, got %s", want, photo.TakenAt)
	}
}

// waitForIndex blocks until the gallery's background index finishes, then returns
// a snapshot of its state.
func waitForIndex(t *testing.T, g *Gallery) viewModel {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		g.mu.RLock()
		ready := g.ready
		state := g.state
		g.mu.RUnlock()
		if ready {
			return state
		}
		if time.Now().After(deadline) {
			t.Fatal("index did not become ready in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func testPlainJPEG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 0xff, A: 0xff})
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func testJPEG(t *testing.T, dateTimeOriginal string, keywords []string, title string) []byte {
	t.Helper()
	base := testPlainJPEG(t)

	exifSegment := buildEXIFSegment(t, dateTimeOriginal, title)
	xmpSegment := buildXMPSegment(keywords, title)

	var out bytes.Buffer
	out.Write(base[:2])
	out.Write(exifSegment)
	out.Write(xmpSegment)
	out.Write(base[2:])
	return out.Bytes()
}

func buildEXIFSegment(t *testing.T, dateTimeOriginal, title string) []byte {
	t.Helper()

	titleBytes := append([]byte(title), 0)
	dateBytes := append([]byte(dateTimeOriginal), 0)
	ifd0Offset := uint32(8)
	ifd0EntryCount := uint16(3)
	ifd0DataOffset := ifd0Offset + 2 + uint32(ifd0EntryCount)*12 + 4
	titleOffset := ifd0DataOffset
	dateOffset := titleOffset + uint32(len(titleBytes))
	subIFDOffset := dateOffset + uint32(len(dateBytes))
	subIFDDateOffset := subIFDOffset + 2 + 12 + 4

	tiff := &bytes.Buffer{}
	tiff.Write([]byte{'I', 'I', 0x2a, 0x00})
	_ = binary.Write(tiff, binary.LittleEndian, ifd0Offset)

	_ = binary.Write(tiff, binary.LittleEndian, ifd0EntryCount)
	writeIFDEntry(tiff, 0x010e, 2, uint32(len(titleBytes)), titleOffset)
	writeIFDEntry(tiff, 0x0132, 2, uint32(len(dateBytes)), dateOffset)
	writeIFDEntry(tiff, 0x8769, 4, 1, subIFDOffset)
	_ = binary.Write(tiff, binary.LittleEndian, uint32(0))
	tiff.Write(titleBytes)
	tiff.Write(dateBytes)

	_ = binary.Write(tiff, binary.LittleEndian, uint16(1))
	writeIFDEntry(tiff, 0x9003, 2, uint32(len(dateBytes)), subIFDDateOffset)
	_ = binary.Write(tiff, binary.LittleEndian, uint32(0))
	tiff.Write(dateBytes)

	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	return jpegAPP1(payload)
}

func buildXMPSegment(keywords []string, title string) []byte {
	packet := `http://ns.adobe.com/xap/1.0/` + "\x00" +
		`<x:xmpmeta xmlns:x="adobe:ns:meta/" xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns:dc="http://purl.org/dc/elements/1.1/">` +
		`<rdf:RDF><rdf:Description><dc:title><rdf:Alt><rdf:li xml:lang="x-default">` + title + `</rdf:li></rdf:Alt></dc:title><dc:subject><rdf:Bag>`
	for _, keyword := range keywords {
		packet += `<rdf:li>` + keyword + `</rdf:li>`
	}
	packet += `</rdf:Bag></dc:subject></rdf:Description></rdf:RDF></x:xmpmeta>`
	return jpegAPP1([]byte(packet))
}

func jpegAPP1(payload []byte) []byte {
	length := len(payload) + 2
	buf := make([]byte, 0, length+2)
	buf = append(buf, 0xff, 0xe1, byte(length>>8), byte(length))
	buf = append(buf, payload...)
	return buf
}

func writeIFDEntry(buf *bytes.Buffer, tag uint16, fieldType uint16, count uint32, valueOrOffset uint32) {
	_ = binary.Write(buf, binary.LittleEndian, tag)
	_ = binary.Write(buf, binary.LittleEndian, fieldType)
	_ = binary.Write(buf, binary.LittleEndian, count)
	_ = binary.Write(buf, binary.LittleEndian, valueOrOffset)
}

func TestCachePersistsAndReusesAcrossRestart(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.jpg"), testJPEG(t, "2024:05:06 07:08:09", []string{"sunset"}, "Golden Hour"), 0o644); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(t.TempDir(), "index.gob")

	// First start: cold index, writes the cache.
	g1, err := New(Config{PhotoRoot: root, CachePath: cachePath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	waitForIndex(t, g1)
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("cache file not written: %v", err)
	}

	// Second start: the cache should load and the entry be reused (searchText,
	// which gob does not persist, must be rebuilt so search still works).
	g2, err := New(Config{PhotoRoot: root, CachePath: cachePath})
	if err != nil {
		t.Fatalf("New (restart): %v", err)
	}
	g2.mu.RLock()
	loaded := len(g2.cache)
	g2.mu.RUnlock()
	if loaded != 1 {
		t.Fatalf("expected 1 entry loaded from cache, got %d", loaded)
	}
	vm := waitForIndex(t, g2)
	if len(vm.Photos) != 1 {
		t.Fatalf("expected 1 photo after restart, got %d", len(vm.Photos))
	}
	if got := filterPhotos(vm.Photos, "", "sunset"); len(got) != 1 {
		t.Fatalf("search on cached photo failed; searchText not rebuilt (got %d)", len(got))
	}
}
