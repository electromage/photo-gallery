package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/electromage/photo-gallery/internal/gallery"
)

func main() {
	photoRoot := getenv("PHOTO_ROOT", "./photos")
	addr := getenv("ADDR", ":8080")
	cachePath := getenv("GALLERY_CACHE", "gallery-cache.gob")
	refreshInterval := durationEnv("GALLERY_REFRESH", 2*time.Minute)

	app, err := gallery.New(photoRoot, cachePath)
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
