package main

import (
	"bufio"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/electromage/photo-gallery/internal/gallery"
)

func main() {
	// Load a .env file (if present) before reading configuration. Real environment
	// variables always win over .env, so container/systemd settings override it.
	loadDotEnv(getenv("GALLERY_ENV_FILE", ".env"))

	photoRoot := getenv("PHOTO_ROOT", "./photos")
	addr := getenv("ADDR", ":8080")
	cachePath := getenv("GALLERY_CACHE", "gallery-cache.gob")
	thumbCache := getenv("THUMB_CACHE", "gallery-thumbs")
	refreshInterval := durationEnv("GALLERY_REFRESH", 2*time.Minute)

	downloadSizes, err := gallery.ParseDownloadSizes(getenv("DOWNLOAD_SIZES", ""))
	if err != nil {
		log.Printf("invalid DOWNLOAD_SIZES, using defaults: %v", err)
		downloadSizes = nil
	}

	app, err := gallery.New(gallery.Config{
		PhotoRoot:      photoRoot,
		CachePath:      cachePath,
		ThumbCache:     thumbCache,
		WarmCache:      boolEnv("WARM_CACHE", false),
		Title:          getenv("SITE_TITLE", "Photo Gallery"),
		Domain:         getenv("SITE_DOMAIN", ""),
		ThumbHeight:    intEnv("THUMB_HEIGHT", 0),
		ThumbQuality:   intEnv("THUMB_QUALITY", 0),
		PreviewMax:     intEnv("PREVIEW_MAX", 0),
		PreviewQuality: intEnv("PREVIEW_QUALITY", 0),
		DownloadSizes:  downloadSizes,
	})
	if err != nil {
		log.Fatalf("unable to index photos: %v", err)
	}

	if refreshInterval > 0 {
		go func() {
			ticker := time.NewTicker(refreshInterval)
			defer ticker.Stop()
			for range ticker.C {
				if err := app.Rescan(); err != nil {
					log.Printf("background rescan failed: %v", err)
				}
			}
		}()
	}

	mux := http.NewServeMux()
	mux.Handle("/media/", http.StripPrefix("/media/", http.FileServer(http.FS(os.DirFS(photoRoot)))))
	mux.HandleFunc("/thumb/", app.HandleThumb)
	mux.HandleFunc("/preview/", app.HandlePreview)
	mux.HandleFunc("/download/", app.HandleDownload)
	mux.HandleFunc("/albums/", app.HandleAlbum)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", app.HandleIndex)

	log.Printf("photo gallery listening on %s and serving %s", addr, photoRoot)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// loadDotEnv reads KEY=VALUE lines from a .env file into the environment. Missing
// files are ignored. Blank lines and lines starting with '#' are skipped, an
// optional leading "export " is allowed, and surrounding quotes are stripped.
// Existing environment variables are never overwritten.
func loadDotEnv(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if key == "" {
			continue
		}
		if _, ok := os.LookupEnv(key); ok {
			continue // real environment wins
		}
		os.Setenv(key, unquote(strings.TrimSpace(line[eq+1:])))
	}
}

// intEnv reads an integer environment variable, falling back on empty/invalid input.
func intEnv(key string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("invalid %s value %q, using default", key, v)
	}
	return fallback
}

// boolEnv reads a boolean environment variable (1/true/yes/on, case-insensitive).
func boolEnv(key string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

// unquote strips a single matching pair of surrounding single or double quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func durationEnv(key string, fallback time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		duration, err := time.ParseDuration(value)
		if err == nil {
			return duration
		}
		log.Printf("invalid %s value %q, using %s", key, value, fallback)
	}
	return fallback
}
