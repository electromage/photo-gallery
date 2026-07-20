package gallery

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"html"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf16"

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

type Gallery struct {
	root string
	tmpl *template.Template

	mu    sync.RWMutex
	state viewModel
}

type Album struct {
	Path   string
	Name   string
	Count  int
	Cover  string
	Active bool
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
	searchText string
}

type viewModel struct {
	Albums    []Album
	Photos    []Photo
	IndexedAt time.Time
}

type pageData struct {
	Albums       []Album
	Photos       []Photo
	Query        string
	CurrentAlbum string
	ActionURL    string
	IndexedAt    string
}

func New(root string) (*Gallery, error) {
	tmpl, err := template.New("gallery").Funcs(template.FuncMap{
		"albumURL": func(path string) string {
			if path == "" {
				return "/"
			}
			return "/albums/" + escapePath(path)
		},
	}).Parse(pageTemplate)
	if err != nil {
		return nil, err
	}

	g := &Gallery{root: root, tmpl: tmpl}
	if err := g.Rescan(); err != nil {
		return nil, err
	}
	return g, nil
}

func (g *Gallery) Rescan() error {
	state, err := buildViewModel(g.root)
	if err != nil {
		return err
	}
	g.mu.Lock()
	g.state = state
	g.mu.Unlock()
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
	g.mu.RUnlock()

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	page := pageData{
		Albums:       filterAlbums(state.Albums, currentAlbum),
		Photos:       filterPhotos(state.Photos, currentAlbum, query),
		Query:        query,
		CurrentAlbum: currentAlbum,
		ActionURL:    "/",
		IndexedAt:    state.IndexedAt.Format("02 Jan 2006 15:04"),
	}
	if currentAlbum != "" {
		page.ActionURL = "/albums/" + escapePath(currentAlbum)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := g.tmpl.Execute(w, page); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func buildViewModel(root string) (viewModel, error) {
	info, err := os.Stat(root)
	if err != nil {
		return viewModel{}, err
	}
	if !info.IsDir() {
		return viewModel{}, fmt.Errorf("%s is not a directory", root)
	}

	var photos []Photo
	albums := map[string]*Album{}

	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !jpgPattern.MatchString(entry.Name()) {
			return nil
		}

		relPath, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relPath = filepath.ToSlash(relPath)
		albumPath := filepath.ToSlash(filepath.Dir(relPath))
		if albumPath == "." {
			albumPath = ""
		}

		photo, err := buildPhoto(path, relPath, albumPath)
		if err != nil {
			return err
		}
		photos = append(photos, photo)

		album := albums[albumPath]
		if album == nil {
			album = &Album{Path: albumPath, Name: prettyAlbumName(albumPath)}
			albums[albumPath] = album
		}
		album.Count++
		if album.Cover == "" {
			album.Cover = photo.MediaPath
		}
		return nil
	})
	if err != nil {
		return viewModel{}, err
	}

	sort.Slice(photos, func(i, j int) bool {
		if photos[i].TakenAt.Equal(photos[j].TakenAt) {
			return photos[i].MediaPath < photos[j].MediaPath
		}
		return photos[i].TakenAt.After(photos[j].TakenAt)
	})

	albumList := make([]Album, 0, len(albums))
	for _, album := range albums {
		albumList = append(albumList, *album)
	}
	sort.Slice(albumList, func(i, j int) bool {
		if albumList[i].Path == "" {
			return true
		}
		if albumList[j].Path == "" {
			return false
		}
		return albumList[i].Name < albumList[j].Name
	})

	return viewModel{
		Albums:    albumList,
		Photos:    photos,
		IndexedAt: time.Now(),
	}, nil
}

func buildPhoto(absPath, relPath, albumPath string) (Photo, error) {
	fileInfo, err := os.Stat(absPath)
	if err != nil {
		return Photo{}, err
	}

	takenAt := fileInfo.ModTime()
	title := strings.TrimSuffix(filepath.Base(relPath), filepath.Ext(relPath))
	tags := make([]string, 0, 4)

	if metadata, err := readMetadata(absPath); err == nil {
		if !metadata.takenAt.IsZero() {
			takenAt = metadata.takenAt
		}
		if metadata.title != "" {
			title = metadata.title
		}
		tags = append(tags, metadata.tags...)
	}

	tags = normalizeTerms(tags)

	photo := Photo{
		Title:      title,
		AlbumPath:  albumPath,
		AlbumName:  prettyAlbumName(albumPath),
		MediaPath:  relPath,
		MediaURL:   "/media/" + escapePath(relPath),
		TakenAt:    takenAt.UTC(),
		TakenAtUTC: takenAt.UTC().Format("02 Jan 2006"),
		Tags:       tags,
	}
	photo.searchText = strings.ToLower(strings.Join([]string{
		photo.Title,
		photo.AlbumName,
		photo.AlbumPath,
		strings.Join(photo.Tags, " "),
	}, " "))

	return photo, nil
}

type metadata struct {
	takenAt time.Time
	title   string
	tags    []string
}

func readMetadata(path string) (metadata, error) {
	file, err := os.Open(path)
	if err != nil {
		return metadata{}, err
	}
	defer file.Close()

	raw, err := io.ReadAll(file)
	if err != nil {
		return metadata{}, err
	}

	result := metadata{}
	if len(raw) > 0 {
		xmpTags, xmpTitle := parseXMP(raw)
		result.tags = append(result.tags, xmpTags...)
		if xmpTitle != "" {
			result.title = xmpTitle
		}
	}

	exifData, err := exif.Decode(bytes.NewReader(raw))
	if err != nil {
		return result, nil
	}

	if takenAt, err := exifData.DateTime(); err == nil {
		result.takenAt = takenAt
	}
	if result.title == "" {
		result.title = firstExifString(exifData, titleFields...)
	}
	result.tags = append(result.tags, collectExifTags(exifData)...)
	return result, nil
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

func filterAlbums(albums []Album, currentAlbum string) []Album {
	filtered := make([]Album, len(albums))
	for i, album := range albums {
		filtered[i] = album
		filtered[i].Active = album.Path == currentAlbum
	}
	return filtered
}

func filterPhotos(photos []Photo, currentAlbum, query string) []Photo {
	query = strings.ToLower(strings.TrimSpace(query))
	filtered := make([]Photo, 0, len(photos))
	for _, photo := range photos {
		if currentAlbum != "" && photo.AlbumPath != currentAlbum {
			continue
		}
		if query != "" && !strings.Contains(photo.searchText, query) {
			continue
		}
		filtered = append(filtered, photo)
	}
	return filtered
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

const pageTemplate = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Photo Gallery</title>
  <style>
    :root { color-scheme: light; --bg:#f5f7fb; --panel:#ffffff; --text:#132238; --muted:#687487; --accent:#2563eb; --line:#d8e0ee; }
    * { box-sizing:border-box; }
    body { margin:0; font-family:Inter,system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif; background:var(--bg); color:var(--text); }
    a { color:inherit; text-decoration:none; }
    .shell { max-width:1280px; margin:0 auto; padding:32px 20px 48px; }
    .hero { display:flex; flex-wrap:wrap; gap:16px; justify-content:space-between; align-items:end; margin-bottom:24px; }
    .hero h1 { margin:0 0 8px; font-size:clamp(2rem,4vw,3rem); }
    .hero p, .meta { margin:0; color:var(--muted); }
    .search { display:flex; gap:12px; flex-wrap:wrap; }
    .search input { min-width:260px; border:1px solid var(--line); background:var(--panel); border-radius:999px; padding:12px 16px; font:inherit; }
    .search button { border:0; border-radius:999px; background:var(--accent); color:#fff; padding:12px 18px; font:inherit; cursor:pointer; }
    .layout { display:grid; grid-template-columns:minmax(220px,260px) 1fr; gap:24px; }
    .panel { background:var(--panel); border:1px solid var(--line); border-radius:24px; box-shadow:0 16px 40px rgba(15, 23, 42, 0.06); }
    .albums { padding:20px; position:sticky; top:16px; }
    .albums ul { list-style:none; margin:16px 0 0; padding:0; }
    .albums li + li { margin-top:8px; }
    .albums a { display:flex; justify-content:space-between; align-items:center; padding:10px 12px; border-radius:14px; color:var(--muted); }
    .albums a.active, .albums a:hover { background:#eef4ff; color:var(--accent); }
    .grid { display:grid; grid-template-columns:repeat(auto-fit,minmax(260px,1fr)); gap:18px; }
    .card { overflow:hidden; }
    .card img { width:100%; aspect-ratio:4 / 3; object-fit:cover; display:block; background:#dfe6f4; }
    .card-body { padding:16px; }
    .card h2 { margin:0 0 6px; font-size:1.05rem; }
    .card p { margin:0; color:var(--muted); }
    .tags { display:flex; flex-wrap:wrap; gap:8px; margin-top:14px; }
    .tag { background:#edf2ff; color:#3656a8; border-radius:999px; padding:6px 10px; font-size:.85rem; }
    .empty { padding:40px; text-align:center; color:var(--muted); }
    @media (max-width: 900px) { .layout { grid-template-columns:1fr; } .albums { position:static; } }
  </style>
</head>
<body>
  <div class="shell">
    <div class="hero">
      <div>
        <p class="meta">Public, read-only photo archive</p>
        <h1>Photo Gallery</h1>
        <p>Newest JPEGs first, grouped by directory-backed albums, with search across album names and embedded tags.</p>
      </div>
      <form class="search" method="get" action="{{.ActionURL}}">
        <input type="search" name="q" placeholder="Search albums or tags" value="{{.Query}}">
        <button type="submit">Search</button>
      </form>
    </div>

    <div class="layout">
      <aside class="panel albums">
        <p class="meta">Albums</p>
        <ul>
          <li><a href="/" class="{{if eq .CurrentAlbum ""}}active{{end}}"><span>All photos</span></a></li>
          {{range .Albums}}
          <li>
            <a href="{{albumURL .Path}}" class="{{if .Active}}active{{end}}">
              <span>{{.Name}}</span>
              <span>{{.Count}}</span>
            </a>
          </li>
          {{end}}
        </ul>
        <p class="meta" style="margin-top:18px;">Indexed {{.IndexedAt}}</p>
      </aside>

      <main>
        {{if .Photos}}
        <div class="grid">
          {{range .Photos}}
          <article class="panel card">
            <a href="{{.MediaURL}}">
              <img src="{{.MediaURL}}" alt="{{.Title}}" loading="lazy">
            </a>
            <div class="card-body">
              <h2>{{.Title}}</h2>
              <p>{{.AlbumName}} · {{.TakenAtUTC}}</p>
              {{if .Tags}}
              <div class="tags">
                {{range .Tags}}<span class="tag">{{.}}</span>{{end}}
              </div>
              {{end}}
            </div>
          </article>
          {{end}}
        </div>
        {{else}}
        <div class="panel empty">
          <p>No photos matched this album or search yet.</p>
        </div>
        {{end}}
      </main>
    </div>
  </div>
</body>
</html>
`
