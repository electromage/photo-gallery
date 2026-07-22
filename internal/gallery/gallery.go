package gallery

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"image"
	_ "image/jpeg" // register JPEG decoder for image.DecodeConfig
	"io"
	"io/fs"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf16"

	"github.com/barasher/go-exiftool"
	"github.com/rwcarlsen/goexif/exif"
)

var (
	jpgPattern      = regexp.MustCompile(`(?i)\.(jpe?g)$`)
	xmpKeywordRE    = regexp.MustCompile(`<rdf:li>([^<]+)</rdf:li>`)
	xmpSubjectRE    = regexp.MustCompile(`(?i)dc:subject`)
	xmpTitleRE      = regexp.MustCompile(`(?s)<dc:title>.*?<rdf:li[^>]*>([^<]+)</rdf:li>.*?</dc:title>`)
	titleFields     = []exif.FieldName{exif.ImageDescription, exif.XPTitle}
	tagFields       = []exif.FieldName{exif.UserComment}
	xpKeywordsField = exif.XPKeywords
)

// Config holds the gallery's settings. Add fields here as configuration grows;
// main wires each one from an environment variable (optionally via a .env file).
type Config struct {
	PhotoRoot  string // directory to index and serve
	CachePath  string // on-disk index cache; "" disables persistence
	ThumbCache string // directory for generated thumbnails/previews; "" generates on the fly (no caching)
	WarmCache  bool   // pre-generate thumbnails/previews in the background after indexing
	Title      string // site title (page <title> and header); defaults to "Photo Gallery"
	Domain     string // public base URL, e.g. https://photos.example.com; enables canonical/OG tags

	ThumbHeight    int            // thumbnail max height in px (0 = default)
	ThumbQuality   int            // thumbnail JPEG/WebP quality 1-100 (0 = default)
	PreviewMax     int            // preview max longest edge in px (0 = default)
	PreviewQuality int            // preview JPEG/WebP quality 1-100 (0 = default)
	DownloadSizes  []DownloadSize // download resolution presets (nil = default)
}

type Gallery struct {
	root      string
	cachePath string // on-disk index cache; "" disables persistence
	thumbDir  string // thumbnail/preview cache directory; "" disables caching
	warm      bool   // pre-generate renditions in the background
	cwebp     string // path to the cwebp binary; "" encodes renditions as JPEG
	title     string
	domain    string

	thumbV        variant
	previewV      variant
	downloadSizes []DownloadSize
	tmpl          *template.Template
	indexTmpl     *template.Template
	et            *exiftool.Exiftool // nil when the exiftool binary is unavailable

	mu    sync.RWMutex
	state viewModel
	ready bool                  // set once the first index completes
	cache map[string]cacheEntry // key: MediaPath; lets rescans skip unchanged files

	rescanMu sync.Mutex // serializes rescans so overlapping ticks are skipped
	warmMu   sync.Mutex // ensures only one cache-warm pass runs at a time
}

// cacheEntry remembers a processed photo along with the file stats used to decide
// whether it can be reused on the next rescan without re-reading the file.
type cacheEntry struct {
	photo Photo
	mod   int64
	size  int64
}

// fileJob is one image discovered during a rescan walk.
type fileJob struct {
	abs, rel, album string
	mod             int64
	size            int64
}

// cacheVersion is bumped when the persisted format changes so stale files on disk
// are ignored rather than misread. v2: added Photo.Orient and made Width/Height
// orientation-corrected (display) dimensions.
const cacheVersion = 2

// persistedEntry / persistedFile are the on-disk (gob) form of the cache. They use
// exported fields because gob only encodes those; Photo.searchText is unexported
// and is recomputed on load.
type persistedEntry struct {
	Photo Photo
	Mod   int64
	Size  int64
}

type persistedFile struct {
	Version int
	Entries map[string]persistedEntry
}

// AlbumNode is a folder in the album tree. Count is the number of photos in the
// whole subtree (this folder plus all descendants). Active/Open are set per request.
type AlbumNode struct {
	Path     string
	Name     string
	Count    int
	Children []*AlbumNode
	Active   bool // this folder is the current album
	Open     bool // this folder is on the path to the current album (expanded)
}

// KeywordCount is a keyword/tag and how many photos carry it.
type KeywordCount struct {
	Name  string
	Count int
}

type Photo struct {
	Title      string
	AlbumPath  string
	AlbumName  string
	MediaPath  string
	MediaURL   string
	TakenAt    time.Time
	TakenAtUTC string
	Tags       []string
	Width      int
	Height     int
	Orient     int // EXIF orientation 1-8; baked into re-encoded thumbnails/downloads
	Info       []exifKV
	searchText string
}

// exifKV is one labeled metadata field shown in the viewer (e.g. Camera → …).
type exifKV struct {
	Label string `json:"k"`
	Value string `json:"v"`
}

type viewModel struct {
	Tree      *AlbumNode
	Keywords  []KeywordCount
	Photos    []Photo
	IndexedAt time.Time
}

type pageData struct {
	Tree             *AlbumNode
	Keywords         []KeywordCount
	Photos           []Photo
	Items            []gridItem // photos interleaved with month/year section headers
	PhotosJSON       template.JS
	ResJSON          template.JS
	Query            string
	CurrentAlbum     string
	CurrentAlbumName string
	CurrentTag       string
	ActionURL        string
	Title            string
	Canonical        string // absolute URL of this page (only when Domain is set)
	OGTitle          string // Open Graph title (photo-specific for deep links)
	OGImage          string // Open Graph image (absolute) for a deep-linked photo
}

// gridItem is one entry in the rendered grid: either a section header (Header set)
// or a photo. Index is the photo's position in the PHOTOS payload, used for the
// tile's data-i so the viewer/filmstrip stay in sync.
type gridItem struct {
	Header string
	Photo  *Photo
	Index  int
}

// buildGridItems interleaves month/year headers into the (date-sorted) photos so
// the feed shows a break whenever the month changes.
func buildGridItems(photos []Photo) []gridItem {
	items := make([]gridItem, 0, len(photos)+12)
	lastKey := ""
	for i := range photos {
		key := photos[i].TakenAt.Format("2006-01")
		if key != lastKey {
			items = append(items, gridItem{Header: photos[i].TakenAt.Format("January 2006")})
			lastKey = key
		}
		items = append(items, gridItem{Photo: &photos[i], Index: i})
	}
	return items
}

// photoRef is the per-photo payload handed to the viewer's JavaScript: media URL,
// escaped path (for building /download URLs), title, and metadata shown in the
// viewer's info panel. The grid itself renders none of the metadata fields.
type photoRef struct {
	U     string   `json:"u"`  // preview URL shown in the viewer (not the full original)
	Th    string   `json:"th"` // thumbnail URL (grid/filmstrip)
	P     string   `json:"p"`  // escaped path (for /download URLs)
	T     string   `json:"t"`
	Album string   `json:"album,omitempty"`
	AU    string   `json:"au,omitempty"` // album URL (jump to the folder this photo belongs to)
	Date  string   `json:"date,omitempty"`
	Tags  []string `json:"tags,omitempty"`
	Info  []exifKV `json:"info,omitempty"`
}

// New creates a gallery from cfg. If cfg.CachePath is non-empty, the index is
// persisted there (gob) and reused on the next start so restarts skip re-reading
// unchanged files. Cache problems never prevent the server from running.
func New(cfg Config) (*Gallery, error) {
	tmpl, err := template.New("gallery").Funcs(template.FuncMap{
		"albumURL": albumURL,
		"thumbURL": func(mediaPath string) string {
			return "/thumb/" + escapePath(mediaPath)
		},
	}).Parse(pageTemplate)
	if err != nil {
		return nil, err
	}

	indexTmpl, err := template.New("indexing").Parse(indexingTemplate)
	if err != nil {
		return nil, err
	}

	title := strings.TrimSpace(cfg.Title)
	if title == "" {
		title = "Photo Gallery"
	}

	// Validate the root synchronously so misconfiguration fails fast, then index
	// in the background so the server can start listening immediately. Large
	// libraries can take a while to index on first start; the page shows an
	// "indexing" state until g.ready flips.
	root := cfg.PhotoRoot
	if info, err := os.Stat(root); err != nil {
		return nil, err
	} else if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", root)
	}

	downloadSizes := cfg.DownloadSizes
	if len(downloadSizes) == 0 {
		downloadSizes = DefaultDownloadSizes
	}

	g := &Gallery{
		root:      root,
		cachePath: cfg.CachePath,
		thumbDir:  strings.TrimSpace(cfg.ThumbCache),
		warm:      cfg.WarmCache,
		title:     title,
		domain:    strings.TrimRight(strings.TrimSpace(cfg.Domain), "/"),
		tmpl:      tmpl,
		indexTmpl: indexTmpl,
		cache:     map[string]cacheEntry{},
		thumbV: variant{
			tag:     "t",
			maxW:    1 << 20,
			maxH:    positiveOr(cfg.ThumbHeight, defaultThumbHeight),
			quality: clampQuality(cfg.ThumbQuality, defaultThumbQuality),
		},
		previewV: variant{
			tag:     "p",
			maxW:    positiveOr(cfg.PreviewMax, defaultPreviewMax),
			maxH:    positiveOr(cfg.PreviewMax, defaultPreviewMax),
			quality: clampQuality(cfg.PreviewQuality, defaultPreviewQuality),
		},
		downloadSizes: downloadSizes,
	}
	// exiftool gives the most accurate metadata (including Nikon/Canon maker-note
	// details like lens names) across camera makes and edited/exported files. If the
	// binary isn't installed, fall back to the built-in reader.
	if et, err := exiftool.NewExiftool(exiftool.Buffer(make([]byte, 128*1024), 16*1024*1024)); err != nil {
		log.Printf("exiftool unavailable, using built-in EXIF reader (install exiftool for full metadata incl. lens names): %v", err)
	} else {
		g.et = et
	}

	// cwebp yields smaller thumbnails/previews at equal quality. Without it,
	// renditions are encoded as JPEG.
	if path, err := exec.LookPath("cwebp"); err == nil {
		g.cwebp = path
	} else {
		log.Printf("cwebp not found, thumbnails/previews will be JPEG (install libwebp for smaller WebP): %v", err)
	}

	if loaded := loadCache(cfg.CachePath); loaded != nil {
		g.cache = loaded
		log.Printf("loaded %d cached entries from %s", len(loaded), cfg.CachePath)
	}

	// Drop renditions left over from a previous size/quality/format configuration.
	go g.pruneStaleCache()

	go func() {
		if err := g.Rescan(); err != nil {
			log.Printf("initial index failed: %v", err)
		}
	}()
	return g, nil
}

// loadCache reads a persisted index. It returns nil (start cold) on any problem —
// missing file, unreadable data, or a version mismatch — never an error.
func loadCache(path string) map[string]cacheEntry {
	if path == "" {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()

	var pf persistedFile
	if err := gob.NewDecoder(file).Decode(&pf); err != nil {
		log.Printf("ignoring unreadable cache %s: %v", path, err)
		return nil
	}
	if pf.Version != cacheVersion {
		return nil
	}

	out := make(map[string]cacheEntry, len(pf.Entries))
	for key, entry := range pf.Entries {
		photo := entry.Photo
		photo.searchText = searchTextFor(photo)
		out[key] = cacheEntry{photo: photo, mod: entry.Mod, size: entry.Size}
	}
	return out
}

// saveCache atomically writes the index to disk. All failures are logged and
// swallowed; persistence is best-effort.
func saveCache(path string, cache map[string]cacheEntry) {
	if path == "" {
		return
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("cache dir %s: %v", dir, err)
			return
		}
	}

	pf := persistedFile{Version: cacheVersion, Entries: make(map[string]persistedEntry, len(cache))}
	for key, entry := range cache {
		pf.Entries[key] = persistedEntry{Photo: entry.photo, Mod: entry.mod, Size: entry.size}
	}

	tmp := path + ".tmp"
	file, err := os.Create(tmp)
	if err != nil {
		log.Printf("cache write %s: %v", tmp, err)
		return
	}
	if err := gob.NewEncoder(file).Encode(pf); err != nil {
		file.Close()
		os.Remove(tmp)
		log.Printf("cache encode: %v", err)
		return
	}
	if err := file.Close(); err != nil {
		os.Remove(tmp)
		log.Printf("cache close: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("cache rename: %v", err)
	}
}

// Rescan re-indexes the library. It walks the tree (a cheap stat-only pass),
// reuses cached entries for files whose modtime and size are unchanged, and reads
// metadata for new or changed files in parallel. Overlapping rescans are skipped.
func (g *Gallery) Rescan() error {
	if !g.rescanMu.TryLock() {
		return nil // a rescan is already running
	}
	defer g.rescanMu.Unlock()

	start := time.Now()
	info, err := os.Stat(g.root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", g.root)
	}

	var jobs []fileJob
	walkErr := filepath.WalkDir(g.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !jpgPattern.MatchString(entry.Name()) {
			return nil
		}
		fi, err := entry.Info()
		if err != nil {
			return nil // skip files we cannot stat
		}
		rel, err := filepath.Rel(g.root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		album := filepath.ToSlash(filepath.Dir(rel))
		if album == "." {
			album = ""
		}
		jobs = append(jobs, fileJob{abs: path, rel: rel, album: album, mod: fi.ModTime().UnixNano(), size: fi.Size()})
		return nil
	})
	if walkErr != nil {
		return walkErr
	}

	g.mu.RLock()
	prev := g.cache
	g.mu.RUnlock()

	photos := make([]Photo, len(jobs))
	newCache := make(map[string]cacheEntry, len(jobs))
	var cacheMu sync.Mutex

	var toProcess []int
	for i, j := range jobs {
		if entry, ok := prev[j.rel]; ok && entry.mod == j.mod && entry.size == j.size {
			photos[i] = entry.photo
			newCache[j.rel] = entry
		} else {
			toProcess = append(toProcess, i)
		}
	}

	if len(toProcess) > 0 {
		// exiftool is fastest in batch mode, so extract all changed files up front
		// (serially, since the exiftool process isn't concurrency-safe); the worker
		// pool then just assembles Photos, falling back to the built-in reader for
		// any file exiftool couldn't handle.
		prefetched := g.extractExifBatch(jobs, toProcess)

		workers := runtime.NumCPU()
		if workers > 8 {
			workers = 8 // metadata reads are largely disk-bound
		}
		queue := make(chan int)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for idx := range queue {
					j := jobs[idx]
					md, ok := prefetched[j.abs]
					if !ok {
						if m, err := readMetadata(j.abs); err == nil {
							md = m
						}
					}
					photo := buildPhoto(j.abs, j.rel, j.album, time.Unix(0, j.mod), md)
					photos[idx] = photo
					cacheMu.Lock()
					newCache[j.rel] = cacheEntry{photo: photo, mod: j.mod, size: j.size}
					cacheMu.Unlock()
				}
			}()
		}
		for _, idx := range toProcess {
			queue <- idx
		}
		close(queue)
		wg.Wait()
	}

	sort.Slice(photos, func(i, j int) bool {
		if photos[i].TakenAt.Equal(photos[j].TakenAt) {
			return photos[i].MediaPath < photos[j].MediaPath
		}
		return photos[i].TakenAt.After(photos[j].TakenAt)
	})

	tree := buildAlbumTree(photos)
	keywords := buildKeywords(photos)

	g.mu.Lock()
	g.state = viewModel{Tree: tree, Keywords: keywords, Photos: photos, IndexedAt: time.Now()}
	g.cache = newCache
	g.ready = true
	g.mu.Unlock()

	log.Printf("indexed %d photos in %d folders, %d keywords (%d new/changed, %d reused) in %s",
		len(photos), countFolders(tree), len(keywords), len(toProcess), len(jobs)-len(toProcess), time.Since(start).Round(time.Millisecond))

	// Persist only when the index actually changed, so idle rescans don't rewrite.
	if len(toProcess) > 0 || len(newCache) != len(prev) {
		saveCache(g.cachePath, newCache)
	}

	// Pre-generate thumbnails/previews in the background (opt-in). Skipped renditions
	// that already exist make this cheap on unchanged rescans.
	if g.warm {
		go g.warmCache()
	}
	return nil
}

func (g *Gallery) HandleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	g.render(w, r, "")
}

func (g *Gallery) HandleAlbum(w http.ResponseWriter, r *http.Request) {
	albumPath := strings.TrimPrefix(r.URL.Path, "/albums/")
	albumPath = strings.Trim(albumPath, "/")
	if unescaped, err := url.PathUnescape(albumPath); err == nil {
		albumPath = unescaped
	}
	if albumPath == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	g.render(w, r, filepath.ToSlash(albumPath))
}

func (g *Gallery) render(w http.ResponseWriter, r *http.Request, currentAlbum string) {
	g.mu.RLock()
	state := g.state
	ready := g.ready
	g.mu.RUnlock()

	if !ready {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = g.indexTmpl.Execute(w, struct{ Title string }{g.title})
		return
	}

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	tag := strings.TrimSpace(r.URL.Query().Get("tag"))
	photos := filterPhotos(state.Photos, currentAlbum, query, tag)
	page := pageData{
		Tree:         annotateTree(state.Tree, currentAlbum),
		Keywords:     state.Keywords,
		Photos:       photos,
		Items:        buildGridItems(photos),
		PhotosJSON:   photosPayload(photos),
		ResJSON:      g.resPayload(),
		Query:        query,
		CurrentAlbum: currentAlbum,
		CurrentTag:   tag,
		ActionURL:    "/",
		Title:        g.title,
		OGTitle:      g.title,
	}
	if currentAlbum != "" {
		page.ActionURL = "/albums/" + escapePath(currentAlbum)
		page.CurrentAlbumName = prettyAlbumName(currentAlbum)
	}

	// With a configured domain, emit canonical + Open Graph tags. For a
	// deep-linked photo (?photo=path), point the preview at that image and title.
	if g.domain != "" {
		page.Canonical = g.domain + r.URL.RequestURI()
		if photoPath := r.URL.Query().Get("photo"); photoPath != "" {
			for _, ph := range state.Photos {
				if ph.MediaPath == photoPath {
					page.OGImage = g.domain + "/preview/" + escapePath(ph.MediaPath)
					if ph.Title != "" {
						page.OGTitle = ph.Title + " · " + g.title
					}
					break
				}
			}
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := g.tmpl.Execute(w, page); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// photosPayload serializes the visible photos as JSON for the viewer script.
// json.Marshal escapes <, >, and & so the blob is safe to inline in <script>.
func photosPayload(photos []Photo) template.JS {
	refs := make([]photoRef, 0, len(photos))
	for _, p := range photos {
		refs = append(refs, photoRef{
			U:     "/preview/" + escapePath(p.MediaPath),
			Th:    "/thumb/" + escapePath(p.MediaPath),
			P:     escapePath(p.MediaPath),
			T:     p.Title,
			Album: p.AlbumName,
			AU:    albumURL(p.AlbumPath),
			Date:  p.TakenAtUTC,
			Tags:  p.Tags,
			Info:  p.Info,
		})
	}
	data, err := json.Marshal(refs)
	if err != nil {
		return template.JS("[]")
	}
	return template.JS(data)
}

// resPayload serializes the resolution labels offered in the download menu.
func (g *Gallery) resPayload() template.JS {
	labels := make([]string, 0, len(g.downloadSizes))
	for _, size := range g.downloadSizes {
		labels = append(labels, size.Label)
	}
	data, err := json.Marshal(labels)
	if err != nil {
		return template.JS("[]")
	}
	return template.JS(data)
}

// buildPhoto assembles a Photo from already-extracted metadata. It is best-effort:
// a missing title falls back to the filename, and a missing capture date falls back
// to the file's modification time (modTime, collected during the walk).
func buildPhoto(absPath, relPath, albumPath string, modTime time.Time, md metadata) Photo {
	takenAt := modTime
	if !md.takenAt.IsZero() {
		takenAt = md.takenAt
	}
	title := md.title
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(relPath), filepath.Ext(relPath))
	}
	orient := md.orient
	if orient < 1 || orient > 8 {
		orient = 1
	}

	photo := Photo{
		Title:      title,
		AlbumPath:  albumPath,
		AlbumName:  prettyAlbumName(albumPath),
		MediaPath:  relPath,
		MediaURL:   "/media/" + escapePath(relPath),
		TakenAt:    takenAt.UTC(),
		TakenAtUTC: takenAt.UTC().Format("02 Jan 2006"),
		Tags:       normalizeTerms(md.tags),
		Width:      md.width,
		Height:     md.height,
		Orient:     orient,
		Info:       md.info,
	}
	photo.searchText = searchTextFor(photo)

	return photo
}

// extractMetadata returns a photo's metadata using exiftool when available, or the
// built-in reader as a fallback (the two produce equivalent metadata structs).
func (g *Gallery) extractMetadata(absPath string) metadata {
	if g.et != nil {
		results := g.et.ExtractMetadata(absPath)
		if len(results) == 1 && results[0].Err == nil {
			return metadataFromExiftool(results[0])
		}
	}
	if md, err := readMetadata(absPath); err == nil {
		return md
	}
	return metadata{}
}

// extractExifBatch runs exiftool over the files that need processing, in chunks,
// returning metadata keyed by absolute path. Returns nil when exiftool is absent
// (callers then fall back to the built-in reader per file).
func (g *Gallery) extractExifBatch(jobs []fileJob, idxs []int) map[string]metadata {
	if g.et == nil {
		return nil
	}
	paths := make([]string, len(idxs))
	for i, idx := range idxs {
		paths[i] = jobs[idx].abs
	}
	out := make(map[string]metadata, len(paths))
	const chunk = 200
	for start := 0; start < len(paths); start += chunk {
		end := start + chunk
		if end > len(paths) {
			end = len(paths)
		}
		for _, fm := range g.et.ExtractMetadata(paths[start:end]...) {
			if fm.Err == nil {
				out[fm.File] = metadataFromExiftool(fm)
			}
		}
	}
	return out
}

// photoOrient returns the stored EXIF orientation for an indexed photo (by media
// path), used to bake rotation into re-encoded thumbnails/downloads. Defaults to 1.
func (g *Gallery) photoOrient(mediaPath string) int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if entry, ok := g.cache[mediaPath]; ok && entry.photo.Orient >= 1 {
		return entry.photo.Orient
	}
	return 1
}

// Close releases the exiftool subprocess, if any.
func (g *Gallery) Close() {
	if g.et != nil {
		_ = g.et.Close()
	}
}

// searchTextFor builds the lowercased haystack used by search. It is also used to
// rebuild the field for photos loaded from the cache, since gob does not persist
// the unexported searchText.
func searchTextFor(p Photo) string {
	return strings.ToLower(strings.Join([]string{
		p.Title,
		p.AlbumName,
		p.AlbumPath,
		strings.Join(p.Tags, " "),
	}, " "))
}

type metadata struct {
	takenAt time.Time
	title   string
	tags    []string
	width   int
	height  int
	orient  int
	info    []exifKV
}

// metadataPrefixBytes bounds how much of each file is read for metadata. EXIF
// (capped at 64 KiB), XMP, and the JPEG dimension marker all live near the start,
// so reading a prefix instead of the whole file avoids gigabytes of I/O on large
// libraries of multi-megabyte photos. Files smaller than this are read in full.
const metadataPrefixBytes = 512 << 10 // 512 KiB

func readMetadata(path string) (metadata, error) {
	file, err := os.Open(path)
	if err != nil {
		return metadata{}, err
	}
	defer file.Close()

	raw := make([]byte, metadataPrefixBytes)
	n, err := io.ReadFull(file, raw)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return metadata{}, err
	}
	raw = raw[:n]

	result := metadata{}

	if cfg, _, err := image.DecodeConfig(bytes.NewReader(raw)); err == nil {
		result.width, result.height = cfg.Width, cfg.Height
	}

	if len(raw) > 0 {
		xmpTags, xmpTitle := parseXMP(raw)
		result.tags = append(result.tags, xmpTags...)
		if xmpTitle != "" {
			result.title = xmpTitle
		}
	}

	var exifData *exif.Exif
	if data, err := exif.Decode(bytes.NewReader(raw)); err == nil {
		exifData = data
		if takenAt, err := data.DateTime(); err == nil {
			result.takenAt = takenAt
		}
		if result.title == "" {
			result.title = firstExifString(data, titleFields...)
		}
		result.tags = append(result.tags, collectExifTags(data)...)
		if s := exifInt(data, "Orientation"); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= 8 {
				result.orient = n
			}
		}
	}

	// DecodeConfig returns stored (unrotated) dimensions; swap them for photos the
	// orientation flag says are rotated 90/270 so the grid gets display dimensions.
	if result.orient >= 5 && result.orient <= 8 {
		result.width, result.height = result.height, result.width
	}

	result.info = buildInfo(exifData, result.width, result.height)
	return result, nil
}

// metadataFromExiftool maps exiftool's fields onto our metadata struct. exiftool's
// print conversions give friendly values (decoded lens names, "50.0 mm", "1/250"),
// so most are used as-is.
func metadataFromExiftool(fm exiftool.FileMetadata) metadata {
	md := metadata{}

	for _, key := range []string{"DateTimeOriginal", "SubSecDateTimeOriginal", "CreateDate", "ModifyDate"} {
		if t, ok := parseExifTime(fmStr(fm, key)); ok {
			md.takenAt = t
			break
		}
	}
	for _, key := range []string{"Title", "ImageDescription", "XPTitle", "ObjectName"} {
		if v := fmStr(fm, key); v != "" {
			md.title = v
			break
		}
	}
	md.tags = append(md.tags, fmStrings(fm, "Keywords")...)
	md.tags = append(md.tags, fmStrings(fm, "Subject")...)

	md.orient = parseOrientation(fmStr(fm, "Orientation"))
	w, h := fmInt(fm, "ImageWidth"), fmInt(fm, "ImageHeight")
	if w == 0 || h == 0 {
		w, h = fmInt(fm, "ExifImageWidth"), fmInt(fm, "ExifImageHeight")
	}
	if md.orient >= 5 && md.orient <= 8 {
		w, h = h, w
	}
	md.width, md.height = w, h

	add := func(label, value string) {
		if strings.TrimSpace(value) != "" {
			md.info = append(md.info, exifKV{Label: label, Value: strings.TrimSpace(value)})
		}
	}
	add("Camera", cameraName(fmStr(fm, "Make"), fmStr(fm, "Model")))
	add("Lens", firstNonEmpty(fmStr(fm, "LensID"), fmStr(fm, "Lens"), fmStr(fm, "LensModel")))
	add("Focal length", fmStr(fm, "FocalLength"))
	if v := fmStr(fm, "FNumber"); v != "" {
		add("Aperture", "f/"+v)
	} else if v := fmStr(fm, "Aperture"); v != "" {
		add("Aperture", "f/"+v)
	}
	if v := fmStr(fm, "ExposureTime"); v != "" {
		add("Shutter", v+" s")
	}
	add("ISO", fmStr(fm, "ISO"))
	if w > 0 && h > 0 {
		add("Dimensions", fmt.Sprintf("%d × %d", w, h))
	}
	return md
}

// fmStr reads a field as a trimmed string, formatting numeric/bool values.
func fmStr(fm exiftool.FileMetadata, key string) string {
	v, ok := fm.Fields[key]
	if !ok {
		return ""
	}
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case float64:
		if x == math.Trunc(x) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", v))
	}
}

// fmInt reads a field as an int (exiftool numbers arrive as float64).
func fmInt(fm exiftool.FileMetadata, key string) int {
	switch x := fm.Fields[key].(type) {
	case float64:
		return int(x)
	case string:
		fields := strings.Fields(x)
		if len(fields) > 0 {
			n, _ := strconv.Atoi(fields[0])
			return n
		}
	}
	return 0
}

// fmStrings reads a field that may be a single value or a list (e.g. Keywords).
func fmStrings(fm exiftool.FileMetadata, key string) []string {
	v, ok := fm.Fields[key]
	if !ok {
		return nil
	}
	switch x := v.(type) {
	case []interface{}:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, fmt.Sprintf("%v", e))
		}
		return out
	case string:
		return []string{x}
	default:
		return []string{fmt.Sprintf("%v", v)}
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// parseExifTime parses exiftool/EXIF timestamps ("2006:01:02 15:04:05", optionally
// with sub-seconds or a timezone suffix), ignoring the zero value.
func parseExifTime(v string) (time.Time, bool) {
	if len(v) < 19 || strings.HasPrefix(v, "0000") {
		return time.Time{}, false
	}
	t, err := time.Parse("2006:01:02 15:04:05", v[:19])
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// parseOrientation converts exiftool's Orientation (a descriptive string with print
// conversion on, e.g. "Rotate 90 CW") or a raw number into an EXIF orientation 1-8.
func parseOrientation(s string) int {
	s = strings.TrimSpace(s)
	switch s {
	case "", "Horizontal (normal)":
		return 1
	case "Mirror horizontal":
		return 2
	case "Rotate 180":
		return 3
	case "Mirror vertical":
		return 4
	case "Mirror horizontal and rotate 270 CW":
		return 5
	case "Rotate 90 CW":
		return 6
	case "Mirror horizontal and rotate 90 CW":
		return 7
	case "Rotate 270 CW":
		return 8
	}
	if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= 8 {
		return n
	}
	if strings.Contains(s, "270") {
		return 8
	}
	if strings.Contains(s, "90") {
		return 6
	}
	return 1
}

// buildInfo assembles the ordered EXIF fields shown in the viewer's info panel.
// Missing fields are skipped. data may be nil (e.g. non-EXIF JPEG), in which case
// only dimensions are reported.
func buildInfo(data *exif.Exif, width, height int) []exifKV {
	var info []exifKV
	add := func(label, value string) {
		if strings.TrimSpace(value) != "" {
			info = append(info, exifKV{Label: label, Value: value})
		}
	}

	if data != nil {
		add("Camera", cameraName(exifString(data, "Make"), exifString(data, "Model")))
		add("Lens", exifString(data, "LensModel"))
		if fl := exifRat(data, "FocalLength"); fl > 0 {
			add("Focal length", fmt.Sprintf("%g mm", round1(fl)))
		}
		if fn := exifRat(data, "FNumber"); fn > 0 {
			add("Aperture", fmt.Sprintf("f/%g", round1(fn)))
		}
		if et := exifRat(data, "ExposureTime"); et > 0 {
			add("Shutter", formatExposure(et))
		}
		add("ISO", exifInt(data, "ISOSpeedRatings"))
	}
	if width > 0 && height > 0 {
		add("Dimensions", fmt.Sprintf("%d × %d", width, height))
	}
	return info
}

func exifString(data *exif.Exif, name string) string {
	tag, err := data.Get(exif.FieldName(name))
	if err != nil {
		return ""
	}
	value, err := tag.StringVal()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func exifInt(data *exif.Exif, name string) string {
	tag, err := data.Get(exif.FieldName(name))
	if err != nil {
		return ""
	}
	value, err := tag.Int(0)
	if err != nil {
		return ""
	}
	return strconv.Itoa(value)
}

func exifRat(data *exif.Exif, name string) float64 {
	tag, err := data.Get(exif.FieldName(name))
	if err != nil {
		return 0
	}
	rat, err := tag.Rat(0)
	if err != nil || rat == nil {
		return 0
	}
	value, _ := rat.Float64()
	return value
}

// cameraName combines make and model, avoiding duplication when the model already
// carries the brand. Cameras commonly set a verbose make ("NIKON CORPORATION") and
// a model that repeats the brand ("NIKON D750"), so matching on the make's first
// word rather than the whole string collapses those to just the model.
func cameraName(make, model string) string {
	make = strings.TrimSpace(make)
	model = strings.TrimSpace(model)
	if model == "" {
		return make
	}
	if make == "" {
		return model
	}
	brand := strings.ToLower(strings.Fields(make)[0])
	if strings.HasPrefix(strings.ToLower(model), brand) {
		return model
	}
	return make + " " + model
}

func formatExposure(seconds float64) string {
	if seconds <= 0 {
		return ""
	}
	if seconds < 1 {
		return fmt.Sprintf("1/%d s", int(math.Round(1/seconds)))
	}
	return fmt.Sprintf("%g s", round1(seconds))
}

func round1(f float64) float64 {
	return math.Round(f*10) / 10
}

func collectExifTags(data *exif.Exif) []string {
	var tags []string
	for _, field := range tagFields {
		value := strings.TrimSpace(firstExifString(data, field))
		if value != "" {
			tags = append(tags, splitTerms(value)...)
		}
	}

	if field, err := data.Get(xpKeywordsField); err == nil {
		if value := decodeIntKeywords(field); value != "" {
			decoded := decodeUTF16Keywords(value)
			tags = append(tags, splitTerms(decoded)...)
		}
	}
	return tags
}

func firstExifString(data *exif.Exif, fields ...exif.FieldName) string {
	for _, field := range fields {
		tag, err := data.Get(field)
		if err != nil {
			continue
		}
		value, err := tag.StringVal()
		if err == nil {
			value = strings.TrimSpace(value)
			if value != "" {
				return value
			}
		}
	}
	return ""
}

func decodeUTF16Keywords(value string) string {
	if value == "" {
		return ""
	}
	raw := []byte(value)
	if len(raw)%2 != 0 {
		return value
	}
	words := make([]uint16, 0, len(raw)/2)
	for i := 0; i < len(raw); i += 2 {
		word := binary.LittleEndian.Uint16(raw[i : i+2])
		if word == 0 {
			continue
		}
		words = append(words, word)
	}
	return string(utf16.Decode(words))
}

func decodeIntKeywords(field interface{ Int(int) (int, error) }) string {
	var raw []byte
	for i := 0; ; i++ {
		value, err := field.Int(i)
		if err != nil {
			break
		}
		raw = append(raw, byte(value))
	}
	return string(raw)
}

func parseXMP(raw []byte) ([]string, string) {
	if !xmpSubjectRE.Match(raw) {
		return nil, ""
	}
	packet := string(raw)
	matches := xmpKeywordRE.FindAllStringSubmatch(packet, -1)
	tags := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) == 2 {
			tags = append(tags, html.UnescapeString(match[1]))
		}
	}

	title := ""
	if match := xmpTitleRE.FindStringSubmatch(packet); len(match) == 2 {
		title = html.UnescapeString(match[1])
	}
	return tags, title
}

// filterPhotos selects photos in the current album (and its sub-albums), matching
// the full-text query, and carrying the exact keyword tag. Empty filters match all.
func filterPhotos(photos []Photo, currentAlbum, query, tag string) []Photo {
	query = strings.ToLower(strings.TrimSpace(query))
	tag = strings.ToLower(strings.TrimSpace(tag))
	filtered := make([]Photo, 0, len(photos))
	for _, photo := range photos {
		if currentAlbum != "" && photo.AlbumPath != currentAlbum && !strings.HasPrefix(photo.AlbumPath, currentAlbum+"/") {
			continue
		}
		if query != "" && !strings.Contains(photo.searchText, query) {
			continue
		}
		if tag != "" && !photoHasTag(photo, tag) {
			continue
		}
		filtered = append(filtered, photo)
	}
	return filtered
}

func photoHasTag(p Photo, lowerTag string) bool {
	for _, t := range p.Tags {
		if strings.ToLower(t) == lowerTag {
			return true
		}
	}
	return false
}

// buildAlbumTree builds the folder hierarchy from photo album paths, including
// intermediate folders that have no direct photos. Each node's Count is the number
// of photos in its whole subtree.
func buildAlbumTree(photos []Photo) *AlbumNode {
	root := &AlbumNode{Path: "", Name: "All photos"}
	nodes := map[string]*AlbumNode{"": root}

	var ensure func(path string) *AlbumNode
	ensure = func(path string) *AlbumNode {
		if n, ok := nodes[path]; ok {
			return n
		}
		parent := ""
		if i := strings.LastIndex(path, "/"); i >= 0 {
			parent = path[:i]
		}
		n := &AlbumNode{Path: path, Name: prettyAlbumName(path)}
		nodes[path] = n
		p := ensure(parent)
		p.Children = append(p.Children, n)
		return n
	}

	for _, ph := range photos {
		ensure(ph.AlbumPath)
		for path := ph.AlbumPath; ; {
			nodes[path].Count++
			if path == "" {
				break
			}
			if i := strings.LastIndex(path, "/"); i >= 0 {
				path = path[:i]
			} else {
				path = ""
			}
		}
	}

	sortAlbumNodes(root)
	return root
}

func sortAlbumNodes(n *AlbumNode) {
	sort.Slice(n.Children, func(i, j int) bool {
		return strings.ToLower(n.Children[i].Name) < strings.ToLower(n.Children[j].Name)
	})
	for _, c := range n.Children {
		sortAlbumNodes(c)
	}
}

func countFolders(n *AlbumNode) int {
	total := 0
	for _, c := range n.Children {
		total += 1 + countFolders(c)
	}
	return total
}

// annotateTree returns a copy of the tree with Active/Open set relative to the
// current album, so the branch leading to it renders expanded and highlighted.
func annotateTree(node *AlbumNode, current string) *AlbumNode {
	if node == nil {
		return nil
	}
	n := &AlbumNode{
		Path:   node.Path,
		Name:   node.Name,
		Count:  node.Count,
		Active: node.Path == current,
		Open:   node.Path == "" || node.Path == current || strings.HasPrefix(current, node.Path+"/"),
	}
	for _, c := range node.Children {
		n.Children = append(n.Children, annotateTree(c, current))
	}
	return n
}

// buildKeywords tallies keyword/tag usage across all photos, most-used first.
func buildKeywords(photos []Photo) []KeywordCount {
	counts := map[string]int{}
	display := map[string]string{}
	for _, ph := range photos {
		for _, tag := range ph.Tags {
			key := strings.ToLower(tag)
			counts[key]++
			if _, ok := display[key]; !ok {
				display[key] = tag
			}
		}
	}
	out := make([]KeywordCount, 0, len(counts))
	for key, c := range counts {
		out = append(out, KeywordCount{Name: display[key], Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

func prettyAlbumName(albumPath string) string {
	if albumPath == "" {
		return "Library"
	}
	base := filepath.Base(albumPath)
	base = strings.ReplaceAll(base, "-", " ")
	base = strings.ReplaceAll(base, "_", " ")
	return titleWords(base)
}

// albumURL is the gallery URL for an album path ("" is the root feed).
func albumURL(path string) string {
	if path == "" {
		return "/"
	}
	return "/albums/" + escapePath(path)
}

func escapePath(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func titleWords(input string) string {
	words := strings.Fields(strings.ToLower(strings.TrimSpace(input)))
	for i, word := range words {
		runes := []rune(word)
		if len(runes) == 0 {
			continue
		}
		runes[0] = unicode.ToUpper(runes[0])
		words[i] = string(runes)
	}
	return strings.Join(words, " ")
}

func splitTerms(input string) []string {
	parts := strings.FieldsFunc(input, func(r rune) bool {
		return r == ',' || r == ';' || r == '|' || r == '\n'
	})
	if len(parts) == 1 && strings.Contains(input, "\x00") {
		parts = strings.Split(strings.ReplaceAll(input, "\x00", "|"), "|")
	}
	return parts
}

func normalizeTerms(terms []string) []string {
	seen := map[string]struct{}{}
	normalized := make([]string, 0, len(terms))
	for _, term := range terms {
		clean := strings.TrimSpace(term)
		if clean == "" {
			continue
		}
		key := strings.ToLower(clean)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, clean)
	}
	sort.Strings(normalized)
	return normalized
}

// indexingTemplate is served before the first index completes. It refreshes
// itself until the gallery is ready. {{.Title}} is the configured site title.
const indexingTemplate = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta http-equiv="refresh" content="3">
  <title>{{.Title}}</title>
  <style>
    body { margin:0; min-height:100vh; display:grid; place-items:center; background:#0b0b0d; color:#ededf0;
      font-family:Inter,system-ui,-apple-system,sans-serif; }
    .box { text-align:center; padding:24px; }
    .spin { width:34px; height:34px; margin:0 auto 20px; border:3px solid #26262b; border-top-color:#ededf0;
      border-radius:50%; animation:spin 1s linear infinite; }
    @keyframes spin { to { transform:rotate(360deg); } }
    h1 { margin:0 0 8px; font-size:1.2rem; font-weight:600; }
    p { margin:0; color:#8b8b92; font-size:.9rem; }
  </style>
</head>
<body>
  <div class="box">
    <div class="spin"></div>
    <h1>Indexing your library…</h1>
    <p>This can take a moment on first start. The page refreshes automatically.</p>
  </div>
</body>
</html>
`

const pageTemplate = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.Title}}</title>
  <meta property="og:type" content="website">
  <meta property="og:site_name" content="{{.Title}}">
  {{if .OGTitle}}<meta property="og:title" content="{{.OGTitle}}">{{end}}
  {{if .Canonical}}<link rel="canonical" href="{{.Canonical}}">
  <meta property="og:url" content="{{.Canonical}}">{{end}}
  {{if .OGImage}}<meta property="og:image" content="{{.OGImage}}">
  <meta name="twitter:card" content="summary_large_image">{{end}}
  <style>
    :root { color-scheme: dark; --bg:#0b0b0d; --fg:#ededf0; --muted:#8b8b92; --line:#242428; --panel:#161619; --hi:#f4f4f6; --accent:#6366f1; }
    * { box-sizing:border-box; }
    html, body { margin:0; }
    html { background:var(--bg); }
    body { font-family:Inter,system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif; background:transparent; color:var(--fg); -webkit-font-smoothing:antialiased; }
    /* Ambient background tinted by the colors of the photos currently on screen. */
    #ambient { position:fixed; inset:0; z-index:-1; pointer-events:none; background:var(--bg); }
    a { color:inherit; text-decoration:none; }
    button { font:inherit; color:inherit; cursor:pointer; }

    header { position:sticky; top:0; z-index:20; display:flex; align-items:center; gap:20px; flex-wrap:wrap;
      padding:14px 18px; background:rgba(11,11,13,.82); backdrop-filter:blur(10px); border-bottom:1px solid var(--line); }
    header h1 { margin:0; font-size:1.25rem; font-weight:600; letter-spacing:.01em; white-space:nowrap; }
    .search { margin-left:auto; }
    .search input { width:220px; max-width:46vw; border:1px solid var(--line); background:var(--panel); color:var(--fg);
      border-radius:999px; padding:8px 14px; font:inherit; outline:none; }
    .search input:focus { border-color:#3a3a42; }

    .filters { display:flex; gap:8px; }
    .dropdown { position:relative; }
    .dropdown > summary { list-style:none; cursor:pointer; user-select:none; padding:8px 14px; border-radius:999px;
      border:1px solid var(--line); background:var(--panel); color:var(--fg); font-size:.85rem; white-space:nowrap; }
    .dropdown > summary::-webkit-details-marker { display:none; }
    .dropdown > summary::after { content:"▾"; margin-left:8px; color:var(--muted); }
    .dropdown[open] > summary { border-color:#3a3a42; }
    .dd-panel { position:absolute; top:calc(100% + 6px); left:0; z-index:30; width:300px; max-width:80vw;
      max-height:min(70vh,520px); overflow-y:auto; padding:8px; background:#161619; border:1px solid var(--line);
      border-radius:14px; box-shadow:0 18px 44px rgba(0,0,0,.55); scrollbar-width:thin; }
    .dd-empty { margin:8px 10px; color:var(--muted); font-size:.85rem; }

    .tree, .tree ul { list-style:none; margin:0; padding:0; }
    .tree ul { margin-left:14px; border-left:1px solid var(--line); padding-left:6px; }
    .tree-link { display:flex; align-items:center; gap:8px; padding:6px 10px; border-radius:9px; color:var(--fg);
      font-size:.88rem; }
    .tree-link span { flex:1; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
    .tree-link em { font-style:normal; color:var(--muted); font-size:.8rem; }
    .tree-link:hover { background:#22222a; }
    .tree-link.active { background:var(--hi); color:#0b0b0d; }
    .tree-link.active em { color:#0b0b0d; opacity:.65; }
    .tree details > summary { list-style:none; cursor:pointer; display:flex; align-items:center; }
    .tree details > summary::-webkit-details-marker { display:none; }
    .tree details > summary::before { content:"▸"; color:var(--muted); width:14px; flex:0 0 auto; font-size:.75rem; }
    .tree details[open] > summary::before { content:"▾"; }
    .tree details > summary .tree-link { flex:1; }

    .kw-filter { width:100%; margin-bottom:8px; border:1px solid var(--line); background:var(--bg); color:var(--fg);
      border-radius:8px; padding:7px 10px; font:inherit; outline:none; }
    .kw-list { list-style:none; margin:0; padding:0; }
    .kw-list a { display:flex; align-items:center; gap:8px; padding:6px 10px; border-radius:9px; color:var(--fg); font-size:.88rem; }
    .kw-list a span { flex:1; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
    .kw-list a em { font-style:normal; color:var(--muted); font-size:.8rem; }
    .kw-list a:hover { background:#22222a; }
    .kw-list a.active { background:var(--hi); color:#0b0b0d; }
    .kw-list a.active em { color:#0b0b0d; opacity:.65; }

    .active-filters { display:flex; align-items:center; gap:10px; flex-wrap:wrap; padding:10px 18px;
      border-bottom:1px solid var(--line); font-size:.85rem; }
    .active-filters .af { color:var(--muted); }
    .active-filters .af-clear { color:var(--fg); border:1px solid var(--line); border-radius:999px; padding:3px 12px; }
    .active-filters .af-clear:hover { background:#22222a; }

    .grid { display:flex; flex-wrap:wrap; justify-content:center; gap:1px; padding:1.5% 5%; align-content:flex-start; }
    .grid-break { flex:0 0 100%; display:flex; align-items:center; margin:26px 2px 10px; padding:11px 18px;
      border-left:3px solid var(--accent); border-radius:8px;
      background:linear-gradient(90deg, rgba(99,102,241,.22), rgba(99,102,241,.03)); }
    .grid-break:first-child { margin-top:10px; }
    .grid-break span { font-size:1.08rem; font-weight:600; color:var(--fg); letter-spacing:.03em;
      text-transform:uppercase; }
    .tile { position:relative; overflow:hidden; background:var(--panel); cursor:zoom-in; height:320px; flex:0 0 auto; margin:0; }
    .tile img { display:block; height:100%; width:auto; }
    .grid.js .tile img { width:100%; object-fit:cover; }
    .tile:hover img { filter:brightness(1.08); }
    .tile-dl { position:absolute; top:8px; right:8px; width:34px; height:34px; display:grid; place-items:center;
      border:0; border-radius:50%; background:rgba(0,0,0,.55); color:#fff; opacity:0; transform:translateY(-4px);
      transition:opacity .15s, transform .15s; }
    .tile:hover .tile-dl, .tile-dl:focus-visible { opacity:1; transform:none; }
    .tile-dl:hover { background:rgba(0,0,0,.8); }
    .ic { width:18px; height:18px; }

    .empty { padding:80px 20px; text-align:center; color:var(--muted); }

    .menu { position:fixed; display:none; z-index:60; min-width:132px; padding:6px; background:#1b1b1f;
      border:1px solid var(--line); border-radius:12px; box-shadow:0 12px 34px rgba(0,0,0,.55); }
    .menu a { display:block; padding:8px 12px; border-radius:8px; font-size:.88rem; color:var(--fg); }
    .menu a:hover { background:#2a2a30; }

    .viewer[hidden] { display:none; }
    .viewer { position:fixed; inset:0; z-index:50; display:flex; background:rgba(6,6,8,.97); }
    .v-content { flex:1; min-width:0; display:flex; flex-direction:column; position:relative; }
    .v-stage { position:relative; flex:1; min-height:0; }
    .v-stage img { position:absolute; inset:0; width:100%; height:100%; object-fit:contain; }
    .v-bar { position:absolute; top:0; right:0; display:flex; gap:6px; padding:14px; z-index:4; }
    .v-btn { width:42px; height:42px; display:grid; place-items:center; border:0; border-radius:50%;
      background:rgba(255,255,255,.08); color:#fff; font-size:1.4rem; line-height:1; }
    .v-btn:hover { background:rgba(255,255,255,.18); }
    .v-btn.on { background:var(--hi); color:#0b0b0d; }
    .v-nav { position:absolute; top:50%; transform:translateY(-50%); width:52px; height:72px; border:0; border-radius:12px;
      background:rgba(255,255,255,.06); color:#fff; font-size:2rem; line-height:1; z-index:2; }
    .v-nav:hover { background:rgba(255,255,255,.16); }
    .v-prev { left:12px; } .v-next { right:12px; }
    .v-crumb { position:absolute; top:14px; left:16px; z-index:4; display:inline-flex; align-items:center; gap:7px;
      max-width:min(52vw,380px); padding:8px 14px; border-radius:999px; background:rgba(255,255,255,.08); color:#fff;
      font-size:.85rem; line-height:1; text-decoration:none; white-space:nowrap; overflow:hidden; }
    .v-crumb:hover { background:rgba(255,255,255,.18); }
    .v-crumb .ic { width:15px; height:15px; flex:0 0 auto; opacity:.85; }
    .v-crumb span { overflow:hidden; text-overflow:ellipsis; }
    .v-count { position:absolute; top:56px; left:22px; color:var(--muted); font-size:.85rem; z-index:2; }

    .filmstrip { flex:0 0 auto; display:flex; gap:6px; overflow-x:auto; padding:10px 12px; background:rgba(0,0,0,.35);
      border-top:1px solid var(--line); scrollbar-width:thin; }
    .fs-thumb { flex:0 0 auto; width:92px; height:60px; object-fit:cover; border-radius:6px; opacity:.5;
      cursor:pointer; transition:opacity .15s; outline:2px solid transparent; }
    .fs-thumb:hover { opacity:.85; }
    .fs-thumb.active { opacity:1; outline-color:var(--hi); }

    .v-info { flex:0 0 0; overflow:hidden; background:#121215; border-left:1px solid var(--line); transition:flex-basis .22s ease; }
    .viewer.info-open .v-info { flex-basis:min(340px,82vw); }
    .v-info-inner { position:relative; width:min(340px,82vw); height:100%; padding:22px; overflow-y:auto; box-sizing:border-box; }
    .v-info-close { position:absolute; top:12px; right:14px; width:32px; height:32px; border:0; border-radius:50%;
      background:rgba(255,255,255,.08); color:#fff; font-size:1.25rem; line-height:1; }
    .v-info-close:hover { background:rgba(255,255,255,.18); }
    .v-info h2 { margin:0 40px 4px 0; font-size:1.05rem; font-weight:600; word-break:break-word; }
    .v-info .v-sub { margin:0 0 18px; color:var(--muted); font-size:.85rem; }
    .v-info dl { display:grid; grid-template-columns:auto 1fr; gap:8px 14px; margin:0; font-size:.85rem; }
    .v-info dt { color:var(--muted); white-space:nowrap; }
    .v-info dd { margin:0; text-align:right; word-break:break-word; }
    .v-info .v-tags { display:flex; flex-wrap:wrap; gap:6px; margin-top:20px; }
    .v-info .v-tags a { background:#26262b; color:var(--fg); border-radius:999px; padding:4px 10px; font-size:.78rem; cursor:pointer; }
    .v-info .v-tags a:hover { background:var(--hi); color:#0b0b0d; }

    .toast { position:fixed; bottom:24px; left:50%; z-index:70; background:#1b1b1f; color:#fff;
      border:1px solid var(--line); border-radius:10px; padding:10px 16px; font-size:.85rem;
      box-shadow:0 12px 34px rgba(0,0,0,.5); opacity:0; pointer-events:none;
      transform:translateX(-50%) translateY(20px); transition:opacity .18s ease, transform .18s ease; }
    .toast.show { opacity:1; transform:translateX(-50%) translateY(0); }

    @media (max-width:600px) {
      .grid { padding:4% 4%; }
      .tile { height:200px; }
      .v-nav { width:40px; height:56px; font-size:1.5rem; }
      .v-crumb { max-width:44vw; padding:7px 12px; }
    }
  </style>
</head>
<body>
  <div id="ambient"></div>
  <svg width="0" height="0" aria-hidden="true" style="position:absolute"><symbol id="ic-dl" viewBox="0 0 24 24"><path d="M12 3v11m0 0l-4-4m4 4l4-4M5 20h14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></symbol><symbol id="ic-info" viewBox="0 0 24 24"><circle cx="12" cy="12" r="9" fill="none" stroke="currentColor" stroke-width="2"/><path d="M12 11v5" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"/><circle cx="12" cy="7.6" r="1.15" fill="currentColor"/></symbol><symbol id="ic-link" viewBox="0 0 24 24"><path d="M9 15l6-6M10.5 6.5l1-1a4 4 0 015.9 5.9l-2 2M13.5 17.5l-1 1a4 4 0 01-5.9-5.9l2-2" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></symbol><symbol id="ic-folder" viewBox="0 0 24 24"><path d="M3 6.5a1.5 1.5 0 011.5-1.5h4l2 2.2h8A1.5 1.5 0 0120 8.7v9.3a1 1 0 01-1 1H4a1 1 0 01-1-1z" fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round"/></symbol></svg>

  <header>
    <h1>{{.Title}}</h1>
    <nav class="filters">
      <details class="dropdown" id="dd-folders">
        <summary>Folders</summary>
        <div class="dd-panel">
          <a class="tree-link{{if eq .CurrentAlbum ""}} active{{end}}" href="/"><span>All photos</span><em>{{.Tree.Count}}</em></a>
          {{if .Tree.Children}}<ul class="tree">{{range .Tree.Children}}{{template "albumNode" .}}{{end}}</ul>{{end}}
        </div>
      </details>
      <details class="dropdown" id="dd-keywords">
        <summary>Keywords</summary>
        <div class="dd-panel">
          <input type="search" class="kw-filter" placeholder="Filter keywords" aria-label="Filter keywords">
          {{if .Keywords}}<ul class="kw-list">{{range .Keywords}}<li><a class="{{if eq .Name $.CurrentTag}}active{{end}}" href="?tag={{.Name | urlquery}}"><span>{{.Name}}</span><em>{{.Count}}</em></a></li>{{end}}</ul>
          {{else}}<p class="dd-empty">No keywords found.</p>{{end}}
        </div>
      </details>
    </nav>
    <form class="search" method="get" action="{{.ActionURL}}">
      {{if .CurrentTag}}<input type="hidden" name="tag" value="{{.CurrentTag}}">{{end}}
      <input type="search" name="q" placeholder="Search" value="{{.Query}}" aria-label="Search photos">
    </form>
  </header>

  {{if or .CurrentAlbum .CurrentTag}}
  <div class="active-filters">
    {{if .CurrentAlbum}}<span class="af">Folder: {{.CurrentAlbumName}}</span>{{end}}
    {{if .CurrentTag}}<span class="af">Keyword: {{.CurrentTag}}</span>{{end}}
    <a class="af-clear" href="/">Clear filters</a>
  </div>
  {{end}}

  {{if .Photos}}
  <main class="grid" id="grid">
    {{range .Items}}{{if .Header}}<div class="grid-break"><span>{{.Header}}</span></div>{{else}}{{$p := .Photo}}<figure class="tile" data-i="{{.Index}}"{{if $p.Width}} data-w="{{$p.Width}}" data-h="{{$p.Height}}"{{end}}><img src="{{thumbURL $p.MediaPath}}" alt="{{$p.Title}}" loading="lazy" decoding="async"{{if $p.Width}} width="{{$p.Width}}" height="{{$p.Height}}"{{end}}><button class="tile-dl" data-i="{{.Index}}" aria-label="Download"><svg class="ic"><use href="#ic-dl"></use></svg></button></figure>{{end}}{{end}}
  </main>
  {{else}}
  <div class="empty">No photos in this album or search yet.</div>
  {{end}}

  <div class="menu" id="menu"></div>

  <div class="viewer" id="viewer" hidden>
    <div class="v-content">
      <a class="v-crumb" id="v-crumb" title="Go to this album"><svg class="ic"><use href="#ic-folder"></use></svg><span id="v-crumb-label"></span></a>
      <div class="v-count" id="v-count"></div>
      <div class="v-bar">
        <button class="v-btn" id="v-link" aria-label="Copy link to this photo"><svg class="ic" style="width:20px;height:20px"><use href="#ic-link"></use></svg></button>
        <button class="v-btn" id="v-info-btn" aria-label="Photo info"><svg class="ic" style="width:20px;height:20px"><use href="#ic-info"></use></svg></button>
        <button class="v-btn" id="v-dl" aria-label="Download"><svg class="ic" style="width:20px;height:20px"><use href="#ic-dl"></use></svg></button>
        <button class="v-btn" id="v-close" aria-label="Close">&times;</button>
      </div>
      <div class="v-stage">
        <button class="v-nav v-prev" id="v-prev" aria-label="Previous">&lsaquo;</button>
        <img id="v-img" alt="">
        <button class="v-nav v-next" id="v-next" aria-label="Next">&rsaquo;</button>
      </div>
      <div class="filmstrip" id="filmstrip"></div>
    </div>
    <aside class="v-info" id="v-info">
      <div class="v-info-inner">
        <button class="v-info-close" id="v-info-close" aria-label="Close info">&times;</button>
        <div id="v-info-body"></div>
      </div>
    </aside>
  </div>

  <div class="toast" id="toast"></div>

  <script>
  var PHOTOS = {{.PhotosJSON}};
  var RES = {{.ResJSON}};
  (function(){
    var grid = document.getElementById('grid');
    var viewer = document.getElementById('viewer');
    var vImg = document.getElementById('v-img');
    var vCount = document.getElementById('v-count');
    var strip = document.getElementById('filmstrip');
    var menu = document.getElementById('menu');
    var vInfo = document.getElementById('v-info-body');
    var infoBtn = document.getElementById('v-info-btn');
    var crumb = document.getElementById('v-crumb');
    var crumbLabel = document.getElementById('v-crumb-label');
    var cur = -1;

    // Justified rows: pack tiles left-to-right into rows scaled to a target height,
    // so photos keep their native aspect ratio (no cropping) and time reads
    // top-to-bottom, newest first.
    function layoutGrid(){
      if (!grid) return;
      var target = 320;
      var cs = getComputedStyle(grid);
      var cw = grid.clientWidth - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight);
      if (cw <= 0) return;
      // Gap ~1% of the available width, kept in sync between the CSS and the math.
      var gap = Math.max(1, Math.round(cw * 0.01));
      grid.style.columnGap = gap + 'px';
      grid.style.rowGap = gap + 'px';
      var children = grid.children, row = [], sum = 0;
      function aspect(t){ var w = +t.dataset.w, h = +t.dataset.h; return (w > 0 && h > 0) ? w / h : 1.5; }
      function flush(last){
        if (!row.length) return;
        var avail = cw - gap * (row.length - 1);
        var h = avail / sum;
        if (last && h > target) h = target;
        for (var k = 0; k < row.length; k++){
          var t = row[k];
          t.style.width = Math.floor(aspect(t) * h) + 'px';
          t.style.height = Math.round(h) + 'px';
        }
        row = []; sum = 0;
      }
      for (var i = 0; i < children.length; i++){
        var t = children[i];
        if (!t.classList || !t.classList.contains('tile')) { flush(true); continue; } // section header: end the group's row
        row.push(t); sum += aspect(t);
        if ((cw - gap * (row.length - 1)) / sum <= target) flush(false);
      }
      flush(true);
    }
    if (grid) {
      grid.classList.add('js');
      layoutGrid();
      var rz;
      addEventListener('resize', function(){ clearTimeout(rz); rz = setTimeout(layoutGrid, 120); });
    }

    function dlURL(i, res){ return '/download/' + PHOTOS[i].p + '?res=' + encodeURIComponent(res); }

    // Deep-linking: each open photo is reflected in the URL as ?photo=<path>, so
    // links are shareable and the browser Back button closes the viewer.
    var viewerPushed = false;
    function rawPath(i){ return decodeURIComponent(PHOTOS[i].p); }
    function findByPath(path){
      for (var i = 0; i < PHOTOS.length; i++) { if (rawPath(i) === path) return i; }
      return -1;
    }
    function urlWithPhoto(i){
      var u = new URL(location.href);
      u.searchParams.set('photo', rawPath(i));
      return u.pathname + u.search;
    }
    function urlBase(){
      var u = new URL(location.href);
      u.searchParams.delete('photo');
      return u.pathname + u.search;
    }

    var toastTimer;
    function toast(msg){
      var t = document.getElementById('toast');
      t.textContent = msg;
      t.classList.add('show');
      clearTimeout(toastTimer);
      toastTimer = setTimeout(function(){ t.classList.remove('show'); }, 1600);
    }
    function copyLink(text){
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(function(){ toast('Link copied'); }, function(){ toast('Copy failed'); });
        return;
      }
      var ta = document.createElement('textarea');
      ta.value = text; ta.style.position = 'fixed'; ta.style.opacity = '0';
      document.body.appendChild(ta); ta.select();
      try { document.execCommand('copy'); toast('Link copied'); } catch (_) { toast('Copy failed'); }
      document.body.removeChild(ta);
    }

    // Filter dropdowns: close on outside click / Escape; live-filter the keyword list.
    var dropdowns = document.querySelectorAll('.dropdown');
    document.addEventListener('click', function(e){
      dropdowns.forEach(function(d){ if (d.open && !d.contains(e.target)) d.open = false; });
    });
    document.addEventListener('keydown', function(e){
      if (e.key === 'Escape') dropdowns.forEach(function(d){ d.open = false; });
    });
    var kwFilter = document.querySelector('.kw-filter');
    if (kwFilter) {
      kwFilter.addEventListener('input', function(){
        var q = this.value.trim().toLowerCase();
        this.parentNode.querySelectorAll('.kw-list li').forEach(function(li){
          li.style.display = li.textContent.toLowerCase().indexOf(q) >= 0 ? '' : 'none';
        });
      });
      kwFilter.addEventListener('click', function(e){ e.stopPropagation(); });
    }

    function el(tag, cls, text){
      var e = document.createElement(tag);
      if (cls) e.className = cls;
      if (text != null) e.textContent = text;
      return e;
    }

    function renderInfo(p){
      vInfo.innerHTML = '';
      if (p.t) vInfo.appendChild(el('h2', null, p.t));
      var sub = [p.album, p.date].filter(Boolean).join(' · ');
      if (sub) vInfo.appendChild(el('p', 'v-sub', sub));
      if (p.info && p.info.length) {
        var dl = el('dl');
        p.info.forEach(function(kv){ dl.appendChild(el('dt', null, kv.k)); dl.appendChild(el('dd', null, kv.v)); });
        vInfo.appendChild(dl);
      }
      if (p.tags && p.tags.length) {
        var tg = el('div', 'v-tags');
        p.tags.forEach(function(t){
          var a = el('a', null, t);
          a.href = '?tag=' + encodeURIComponent(t); // click to filter the gallery by this keyword
          tg.appendChild(a);
        });
        vInfo.appendChild(tg);
      }
      if (!vInfo.childNodes.length) vInfo.appendChild(el('p', 'v-sub', 'No metadata available'));
    }

    function openMenu(i, x, y){
      menu.innerHTML = '';
      RES.concat(['original']).forEach(function(r){
        var a = document.createElement('a');
        a.href = dlURL(i, r);
        a.setAttribute('download', '');
        a.textContent = (r === 'original') ? 'Original' : r;
        menu.appendChild(a);
      });
      menu.style.display = 'block';
      var mw = menu.offsetWidth, mh = menu.offsetHeight;
      if (x + mw > innerWidth - 8) x = innerWidth - mw - 8;
      if (y + mh > innerHeight - 8) y = innerHeight - mh - 8;
      menu.style.left = Math.max(8, x) + 'px';
      menu.style.top = Math.max(8, y) + 'px';
    }
    function closeMenu(){ menu.style.display = 'none'; }

    if (grid) {
      PHOTOS.forEach(function(p, i){
        var t = document.createElement('img');
        t.src = p.th; t.loading = 'lazy'; t.className = 'fs-thumb'; t.dataset.i = i;
        strip.appendChild(t);
      });
    }

    function go(i){
      if (i < 0) i = 0;
      if (i >= PHOTOS.length) i = PHOTOS.length - 1;
      cur = i;
      vImg.src = PHOTOS[i].u;
      vImg.alt = PHOTOS[i].t || '';
      vCount.textContent = (i + 1) + ' / ' + PHOTOS.length;
      crumbLabel.textContent = PHOTOS[i].album || 'Library';
      crumb.href = PHOTOS[i].au || '/';
      [i - 1, i + 1].forEach(function(j){ if (j >= 0 && j < PHOTOS.length) { var im = new Image(); im.src = PHOTOS[j].u; } });
      var thumbs = strip.children;
      for (var k = 0; k < thumbs.length; k++) thumbs[k].classList.toggle('active', k === i);
      if (thumbs[i]) thumbs[i].scrollIntoView({ inline: 'center', block: 'nearest' });
      renderInfo(PHOTOS[i]);
      closeMenu();
      if (!viewer.hidden) history.replaceState({ v: 1, i: i }, '', urlWithPhoto(i));
    }
    function openViewer(i, viaHistory){
      var wasClosed = viewer.hidden;
      viewer.hidden = false; document.body.style.overflow = 'hidden';
      if (wasClosed && !viaHistory) { history.pushState({ v: 1, i: i }, '', urlWithPhoto(i)); viewerPushed = true; }
      go(i);
    }
    function closeViewer(viaHistory){
      if (viewer.hidden) return;
      viewer.hidden = true; document.body.style.overflow = ''; vImg.src = ''; closeMenu(); setInfo(false);
      if (viaHistory) return;
      if (viewerPushed) { viewerPushed = false; history.back(); }
      else history.replaceState({ v: 0 }, '', urlBase());
    }
    function next(){ if (cur < PHOTOS.length - 1) go(cur + 1); }
    function prev(){ if (cur > 0) go(cur - 1); }

    addEventListener('popstate', function(e){
      if (e.state && e.state.v === 1) openViewer(e.state.i, true);
      else { viewerPushed = false; closeViewer(true); }
    });

    if (grid) {
      grid.addEventListener('click', function(e){
        var dl = e.target.closest('.tile-dl');
        if (dl) { e.preventDefault(); e.stopPropagation(); var r = dl.getBoundingClientRect(); openMenu(+dl.dataset.i, r.right - 132, r.bottom + 6); return; }
        var tile = e.target.closest('.tile');
        if (tile) openViewer(+tile.dataset.i);
      });
    }

    function setInfo(open){ viewer.classList.toggle('info-open', open); infoBtn.classList.toggle('on', open); }
    function toggleInfo(){ setInfo(!viewer.classList.contains('info-open')); }

    document.getElementById('v-close').onclick = function(e){ e.stopPropagation(); closeViewer(); };
    document.getElementById('v-link').onclick = function(e){ e.stopPropagation(); copyLink(location.origin + urlWithPhoto(cur)); };
    infoBtn.onclick = function(e){ e.stopPropagation(); toggleInfo(); };
    document.getElementById('v-info-close').onclick = function(e){ e.stopPropagation(); setInfo(false); };
    document.getElementById('v-prev').onclick = function(e){ e.stopPropagation(); prev(); };
    document.getElementById('v-next').onclick = function(e){ e.stopPropagation(); next(); };
    document.getElementById('v-dl').onclick = function(e){ e.stopPropagation(); var r = this.getBoundingClientRect(); openMenu(cur, r.right - 132, r.bottom + 6); };
    strip.addEventListener('click', function(e){ var t = e.target.closest('.fs-thumb'); if (t) go(+t.dataset.i); });

    document.addEventListener('click', function(e){
      if (!menu.contains(e.target) && !e.target.closest('.tile-dl') && !e.target.closest('#v-dl')) closeMenu();
    });
    menu.addEventListener('click', function(){ setTimeout(closeMenu, 0); });

    document.addEventListener('keydown', function(e){
      if (viewer.hidden) return;
      if (e.key === 'ArrowLeft') prev();
      else if (e.key === 'ArrowRight') next();
      else if (e.key === 'Escape') { if (viewer.classList.contains('info-open')) setInfo(false); else closeViewer(); }
      else if (e.key === 'Home') go(0);
      else if (e.key === 'End') go(PHOTOS.length - 1);
      else if (e.key === 'i' || e.key === 'I') toggleInfo();
    });

    var stage = document.querySelector('.v-stage');
    var tx = 0;
    stage.addEventListener('touchstart', function(e){ tx = e.changedTouches[0].clientX; }, { passive: true });
    stage.addEventListener('touchend', function(e){
      var dx = e.changedTouches[0].clientX - tx;
      if (Math.abs(dx) > 40) { if (dx < 0) next(); else prev(); }
    }, { passive: true });

    // Ambient background: tint a fixed backdrop with the average colors of the
    // thumbnails currently on screen (top-of-view color at the top, bottom at the
    // bottom), blended heavily into the dark base so it stays subtle.
    (function ambient(){
      var el = document.getElementById('ambient');
      if (!el || !grid) return;
      var base = [11, 11, 13], strength = 0.28;
      var snap = matchMedia('(prefers-reduced-motion: reduce)').matches;
      var cnv = document.createElement('canvas'); cnv.width = cnv.height = 1;
      var cx = cnv.getContext('2d', { willReadFrequently: true });
      var cache = {}; // data-i -> [r,g,b] | null (failed)

      function colorOf(tile){
        var i = tile.dataset.i;
        if (i in cache) return cache[i];
        var img = tile.querySelector('img');
        if (!img) return (cache[i] = null);
        if (!img.complete) return undefined;       // still loading — try again later
        if (!img.naturalWidth) return (cache[i] = null); // failed to load
        try {
          cx.drawImage(img, 0, 0, 1, 1);           // 1x1 draw = average color
          var d = cx.getImageData(0, 0, 1, 1).data;
          return (cache[i] = [d[0], d[1], d[2]]);
        } catch (e) { return (cache[i] = null); }
      }
      function tint(c){
        return [
          Math.round(base[0] + (c[0] - base[0]) * strength),
          Math.round(base[1] + (c[1] - base[1]) * strength),
          Math.round(base[2] + (c[2] - base[2]) * strength)
        ];
      }

      var visible = new Set();
      var io = new IntersectionObserver(function(entries){
        entries.forEach(function(e){ e.isIntersecting ? visible.add(e.target) : visible.delete(e.target); });
        schedule();
      }, { threshold: 0 });
      grid.querySelectorAll('.tile').forEach(function(t){ io.observe(t); });

      var cur = [base.slice(), base.slice()], tgt = [base.slice(), base.slice()];
      var scheduled = false, raf = null;

      function schedule(){
        if (scheduled) return; scheduled = true;
        requestAnimationFrame(function(){ scheduled = false; compute(); });
      }
      function compute(){
        var mid = innerHeight / 2, waiting = false;
        var top = [0, 0, 0], tn = 0, bot = [0, 0, 0], bn = 0;
        visible.forEach(function(t){
          var c = colorOf(t);
          if (c === undefined) { waiting = true; return; }
          if (!c) return;
          var r = t.getBoundingClientRect();
          if (r.top + r.height / 2 < mid) { top[0]+=c[0]; top[1]+=c[1]; top[2]+=c[2]; tn++; }
          else { bot[0]+=c[0]; bot[1]+=c[1]; bot[2]+=c[2]; bn++; }
        });
        if (tn || bn) {
          var ta = tn ? [top[0]/tn, top[1]/tn, top[2]/tn] : [bot[0]/bn, bot[1]/bn, bot[2]/bn];
          var ba = bn ? [bot[0]/bn, bot[1]/bn, bot[2]/bn] : ta;
          tgt = [tint(ta), tint(ba)];
          animate();
        }
        if (waiting) setTimeout(schedule, 200); // some thumbnails still loading
      }
      function animate(){
        var k = snap ? 1 : 0.08, settled = true;
        for (var g = 0; g < 2; g++) for (var i = 0; i < 3; i++) {
          cur[g][i] += (tgt[g][i] - cur[g][i]) * k;
          if (Math.abs(cur[g][i] - tgt[g][i]) > 0.5) settled = false;
        }
        el.style.background = 'linear-gradient(180deg, rgb(' + rgb(cur[0]) + '), rgb(' + rgb(cur[1]) + '))';
        raf = settled ? null : requestAnimationFrame(animate);
      }
      function rgb(c){ return Math.round(c[0]) + ',' + Math.round(c[1]) + ',' + Math.round(c[2]); }

      addEventListener('scroll', schedule, { passive: true });
      addEventListener('resize', schedule, { passive: true });
      schedule();
    })();

    // Open the viewer if the page was loaded with a ?photo= deep link.
    var initPhoto = new URL(location.href).searchParams.get('photo');
    if (initPhoto) {
      var initIndex = findByPath(initPhoto);
      if (initIndex >= 0) {
        openViewer(initIndex, true);
        history.replaceState({ v: 1, i: initIndex }, '', urlWithPhoto(initIndex));
      } else {
        history.replaceState({ v: 0 }, '', urlBase());
      }
    }
  })();
  </script>
</body>
</html>
{{define "albumNode"}}<li>{{if .Children}}<details{{if .Open}} open{{end}}><summary><a class="tree-link{{if .Active}} active{{end}}" href="{{albumURL .Path}}"><span>{{.Name}}</span><em>{{.Count}}</em></a></summary><ul>{{range .Children}}{{template "albumNode" .}}{{end}}</ul></details>{{else}}<a class="tree-link{{if .Active}} active{{end}}" href="{{albumURL .Path}}"><span>{{.Name}}</span><em>{{.Count}}</em></a>{{end}}</li>{{end}}`
