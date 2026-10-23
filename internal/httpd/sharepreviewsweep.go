// Cleanup of the preview caches (fork addition): folders seen while browsing or through shares
// are queued, and a background worker removes the cached previews whose original file was
// deleted, renamed or changed, so the ".cache" folders do not grow forever.

package httpd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/drakkan/sftpgo/v2/internal/logger"
	"github.com/drakkan/sftpgo/v2/internal/vfs"
)

const (
	// a folder is checked at most once in this interval
	previewSweepInterval = 10 * time.Minute
	// leftovers of interrupted jobs older than this are removed
	previewSweepTempAge = time.Hour
)

// enqueueCacheSweep asks for a check of the preview cache of a local folder: a maintenance
// task, at most once every 10 minutes per folder
func enqueueCacheSweep(dir string) {
	if sharePreviews == nil || dir == "" {
		return
	}
	_, _ = tasks.submit("cache-sweep", dir, priorityMaintenance, previewSweepInterval, 5*time.Minute,
		func(_ context.Context) error {
			if removed := sweepPreviewCache(dir); removed > 0 {
				logger.Debug(logSender, "", "preview cache of %q: removed %d stale files", dir, removed)
			}
			return nil
		})
}

// enqueuePreviewSweep queues the check of the preview cache of a listed folder
func enqueuePreviewSweep(connection *Connection, virtualDir string) {
	if sharePreviews == nil {
		return
	}
	fs, resolved, err := connection.GetFsAndResolvedPath(virtualDir)
	if err == nil && vfs.IsLocalOsFs(fs) {
		enqueueCacheSweep(resolved)
	}
}

// sweepPreviewCache removes, in <dir>/.cache, the previews whose source file is gone or has
// changed since, and old leftovers of interrupted jobs. Only names written by the preview code
// are considered: ".cache" is a common name and other tools may keep files there.
// It returns the number of removed files
func sweepPreviewCache(dir string) int {
	cacheDir := filepath.Join(dir, sharePreviewCacheDir)
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return 0
	}
	removed := 0
	remove := func(name string) {
		if err := os.Remove(filepath.Join(cacheDir, name)); err == nil {
			removed++
		}
	}
	// decide for every source from its probe before removing anything: removing a stale probe
	// first would leave its other assets without the signature to check them against
	probes := make(map[string]*sharePreviewProbe)
	for _, entry := range entries {
		if source, suffix, temp := splitPreviewAssetName(entry.Name()); source != "" && !temp &&
			suffix == sharePreviewProbeSuffix {
			var probe sharePreviewProbe
			data, err := os.ReadFile(filepath.Join(cacheDir, entry.Name()))
			if err == nil && json.Unmarshal(data, &probe) == nil {
				probes[source] = &probe
			} else {
				probes[source] = nil // unreadable: the probe goes, the other assets are judged by time
			}
		}
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		name := entry.Name()
		info, err := entry.Info()
		if err != nil {
			continue
		}
		source, suffix, temp := splitPreviewAssetName(name)
		if source == "" {
			continue
		}
		if temp {
			if time.Since(info.ModTime()) > previewSweepTempAge {
				remove(name)
			}
			continue
		}
		sourceInfo, err := os.Stat(filepath.Join(dir, source))
		if err != nil {
			if os.IsNotExist(err) {
				remove(name)
			}
			continue
		}
		if suffix == zipChecksumSuffix {
			// zip checksums carry their own source signature and survive preview version changes
			if !zipChecksumsAreCurrent(filepath.Join(cacheDir, name), sourceInfo) {
				remove(name)
			}
			continue
		}
		probe, hasProbe := probes[source]
		if !previewAssetIsCurrent(probe, hasProbe, suffix, info, sourceInfo) {
			remove(name)
		}
	}
	// gone when nothing is left in it
	_ = os.Remove(cacheDir)
	return removed
}

// previewAssetIsCurrent compares an asset with its source: through the source signature kept
// in the probe when there is one, by modification time otherwise
func previewAssetIsCurrent(probe *sharePreviewProbe, hasProbe bool, suffix string, asset, sourceInfo os.FileInfo) bool {
	if hasProbe {
		if probe == nil {
			return suffix != sharePreviewProbeSuffix && !sourceInfo.ModTime().After(asset.ModTime())
		}
		return probe.SourceSize == sourceInfo.Size() && probe.SourceMtime == sourceInfo.ModTime().UnixNano() &&
			probe.Version == sharePreviewAssetsVersion
	}
	return !sourceInfo.ModTime().After(asset.ModTime())
}

// splitPreviewAssetName splits "<source><suffix>" (or a temporary file of it) and returns an
// empty source for names the preview code does not write
func zipChecksumsAreCurrent(file string, sourceInfo os.FileInfo) bool {
	data, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	var sums zipChecksums
	return json.Unmarshal(data, &sums) == nil && sums.Size == sourceInfo.Size() &&
		sums.Mtime == sourceInfo.ModTime().UnixNano()
}

func splitPreviewAssetName(name string) (source, suffix string, temp bool) {
	for _, s := range append(slices.Clone(sharePreviewSuffixes), zipChecksumSuffix) {
		for _, tempEnd := range []string{".tmp", ".tmp.jpg"} {
			if strings.HasSuffix(name, s+tempEnd) {
				return strings.TrimSuffix(name, s+tempEnd), s, true
			}
		}
		if strings.HasSuffix(name, s) && len(name) > len(s) {
			return strings.TrimSuffix(name, s), s, false
		}
	}
	// the audio preview is written as "<source>.preview.tmp.mp3" before the rename
	if strings.HasSuffix(name, ".preview.tmp.mp3") {
		return strings.TrimSuffix(name, ".preview.tmp.mp3"), sharePreviewAudioSuffix, true
	}
	return "", "", false
}
