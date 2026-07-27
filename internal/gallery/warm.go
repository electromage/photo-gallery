package gallery

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// warmCache pre-generates the thumbnail and preview for every indexed photo so the
// first browse is instant instead of paying the decode+resize cost on demand. It
// runs in the background at low concurrency to avoid starving request handling, is
// safe to call after every rescan (already-cached renditions are skipped), and only
// one run happens at a time.
func (g *Gallery) warmCache() {
	if !g.warm || g.thumbDir == "" {
		return
	}
	if !g.warmMu.TryLock() {
		return // a warm pass is already running
	}
	defer g.warmMu.Unlock()

	g.mu.RLock()
	photos := g.state.Photos
	g.mu.RUnlock()
	if len(photos) == 0 {
		return
	}

	// Keep the warm pass modest and, crucially, below the render limit so live
	// requests always have a free render slot (the shared limiter caps total
	// memory; this just stops warming from monopolizing it).
	workers := runtime.NumCPU() / 2
	if workers < 1 {
		workers = 1
	}
	if workers > 2 {
		workers = 2
	}
	if sc := cap(g.renderSem); sc > 0 && workers >= sc {
		workers = sc - 1
		if workers < 1 {
			workers = 1
		}
	}

	start := time.Now()
	var generated, done int64
	var mu sync.Mutex

	queue := make(chan Photo)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range queue {
				n := g.warmPhoto(p)
				mu.Lock()
				generated += int64(n)
				done++
				mu.Unlock()
			}
		}()
	}
	for _, p := range photos {
		queue <- p
	}
	close(queue)
	wg.Wait()

	if generated > 0 {
		log.Printf("warmed %d renditions for %d photos in %s", generated, len(photos), time.Since(start).Round(time.Millisecond))
	}
}

// warmPhoto ensures both renditions exist for one photo, decoding the original at
// most once. Returns how many renditions it generated (0 if all were cached).
func (g *Gallery) warmPhoto(p Photo) int {
	abs := filepath.Join(g.root, filepath.FromSlash(p.MediaPath))
	info, err := os.Stat(abs)
	if err != nil {
		return 0
	}

	ext := g.imageExt()
	var missing []variant
	for _, v := range []variant{g.thumbV, g.previewV} {
		cached := filepath.Join(g.thumbDir, v.cacheName(p.MediaPath, ext))
		if ci, err := os.Stat(cached); err != nil || ci.ModTime().Before(info.ModTime()) {
			missing = append(missing, v)
		}
	}
	if len(missing) == 0 {
		return 0
	}

	// Share the render limiter with live requests so warming can never push the
	// total number of in-flight decodes past the memory ceiling.
	g.acquireRender()
	defer g.releaseRender()

	img, err := loadImageOriented(abs, p.Orient) // single decode for all missing variants
	if err != nil {
		return 0
	}
	count := 0
	for _, v := range missing {
		data, gotExt, err := g.encodeImage(fitInside(img, v.maxW, v.maxH), v.quality)
		if err != nil {
			continue
		}
		writeCacheFile(g.thumbDir, v.cacheName(p.MediaPath, gotExt), data)
		count++
	}
	return count
}
