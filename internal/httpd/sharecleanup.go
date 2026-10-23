// Removal of dead shares (fork addition): shares that expired, used up their download limit or
// point to files that no longer exist are deleted by an hourly task, so the database only holds shares that still work. Until then
// the link tells why it stopped working and the owner can still extend the share; afterwards it
// says the link is no longer available. The previews and zip checksums kept for a removed share are cleared from the
// .cache folders, unless another live share still covers the same files.

package httpd

import (
	"context"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/drakkan/sftpgo/v2/internal/dataprovider"
	"github.com/drakkan/sftpgo/v2/internal/logger"
	"github.com/drakkan/sftpgo/v2/internal/util"
	"github.com/drakkan/sftpgo/v2/internal/vfs"
)

const (
	shareCleanupInterval = time.Hour
	// the message for links to shares that are gone, they are not remembered
	i18nShareUnavailable = "share.unavailable"
)

var (
	shareCleanupOnce sync.Once
	// one removal at a time: they walk folders and must not race each other
	shareRemovals sync.Mutex
)

func startShareCleanup() {
	shareCleanupOnce.Do(func() {
		cleanup := func(_ context.Context) error {
			removeDeadShares()
			return nil
		}
		tasks.every("share-cleanup", shareCleanupInterval, 30*time.Minute, cleanup)
		go func() {
			// once after the start: the previews of the live shares, for link previews, at share
			// priority; new and edited shares are queued when they are saved
			time.Sleep(30 * time.Second)
			warmLiveShares()
			time.Sleep(30 * time.Second) // then a first cleanup pass
			_, _ = tasks.submit("share-cleanup", "periodic", nil, priorityMaintenance, 0, 30*time.Minute, cleanup)
		}()
	})
}

func isDeadShare(share *dataprovider.Share) bool {
	if share.MaxTokens > 0 && share.UsedTokens >= share.MaxTokens {
		return true
	}
	return share.ExpiresAt > 0 && share.ExpiresAt < util.GetTimeAsMsSinceEpoch(time.Now())
}

// removeDeadShares deletes every dead share of every user
func removeDeadShares() {
	for offset := 0; ; offset += 100 {
		users, err := dataprovider.GetUsers(100, offset, dataprovider.OrderASC, "")
		if err != nil {
			logger.Warn(logSender, "", "share cleanup: unable to list users: %v", err)
			return
		}
		for _, user := range users {
			removeDeadSharesOf(user.Username)
		}
		if len(users) < 100 {
			return
		}
	}
}

// userShares lists all the shares of a user: deleting while paging would shift the pages
func userShares(username string) ([]dataprovider.Share, error) {
	var all []dataprovider.Share
	for offset := 0; ; offset += 100 {
		shares, err := dataprovider.GetShares(100, offset, dataprovider.OrderASC, username)
		if err != nil {
			return nil, err
		}
		all = append(all, shares...)
		if len(shares) < 100 {
			return all, nil
		}
	}
}

func removeDeadSharesOf(username string) {
	shareRemovals.Lock()
	defer shareRemovals.Unlock()
	shares, err := userShares(username)
	if err != nil {
		logger.Warn(logSender, "", "share cleanup: unable to list the shares of %q: %v", username, err)
		return
	}
	user, userErr := dataprovider.GetUserWithGroupSettings(username, "")
	var dead, missing []dataprovider.Share
	var livePaths []string
	for idx := range shares {
		switch {
		case isDeadShare(&shares[idx]):
			dead = append(dead, shares[idx])
		case userErr == nil && isMissingShare(&user, shares[idx].Paths):
			missing = append(missing, shares[idx])
		default:
			livePaths = append(livePaths, shares[idx].Paths...)
		}
	}
	// many missing paths at once means unreachable storage, not deleted files: keep them all
	if len(missing) > 5 && len(missing)*4 > len(shares) {
		logger.Warn(logSender, "", "share cleanup: %d of %d shares of %q point to missing paths, "+
			"the storage may be unreachable, none removed", len(missing), len(shares), username)
		for _, share := range missing {
			livePaths = append(livePaths, share.Paths...)
		}
		missing = nil
	}
	for _, share := range append(dead, missing...) {
		reason := "expired"
		switch {
		case share.MaxTokens > 0 && share.UsedTokens >= share.MaxTokens:
			reason = "download limit reached"
		case !isDeadShare(&share):
			reason = "the shared path no longer exists"
		}
		if err := dataprovider.DeleteShare(share.ShareID, share.Username, "", ""); err != nil {
			logger.Warn(logSender, "", "share cleanup: unable to remove share %q: %v", share.ShareID, err)
			continue
		}
		logger.Info(logSender, "", "share cleanup: removed share %q (%q) of %q: %s", share.ShareID, share.Name,
			share.Username, reason)
		queueShareCacheClear(share, livePaths)
	}
}

// clearShareCacheLater clears the cache of a share deleted by its owner
func clearShareCacheLater(share dataprovider.Share) {
	shares, err := userShares(share.Username)
	if err != nil {
		return
	}
	var livePaths []string
	for idx := range shares {
		livePaths = append(livePaths, shares[idx].Paths...)
	}
	queueShareCacheClear(share, livePaths)
}

// queueShareCacheClear queues the cache clearing of a removed share as a maintenance task that
// holds the shared files and folders, so no preview of them is made meanwhile
func queueShareCacheClear(share dataprovider.Share, livePaths []string) {
	if sharePreviews == nil {
		return
	}
	user, err := dataprovider.GetUserWithGroupSettings(share.Username, "")
	if err != nil {
		return
	}
	var items []string
	for _, sharedPath := range share.Paths {
		fsys, err := user.GetFilesystemForPath(sharedPath, "")
		if err != nil || !vfs.IsLocalOsFs(fsys) {
			continue
		}
		resolved, err := fsys.ResolvePath(sharedPath)
		if err != nil {
			continue
		}
		if info, err := os.Stat(resolved); err == nil && info.IsDir() {
			resolved = strings.TrimSuffix(resolved, "/") + "/"
		}
		items = append(items, resolved)
	}
	if len(items) == 0 {
		return
	}
	_, _ = tasks.submit("share-cache-clear", share.ShareID, items, priorityMaintenance, 0, 30*time.Minute,
		func(_ context.Context) error {
			clearShareCache(share, livePaths)
			return nil
		})
}

// sharedPathsOverlap: one path contains the other
func sharedPathsOverlap(a, b string) bool {
	if a == b || a == "/" || b == "/" {
		return true
	}
	return strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// clearShareCache removes the cached previews and zip checksums of the shared paths that no
// live share covers anymore
func clearShareCache(share dataprovider.Share, livePaths []string) {
	if sharePreviews == nil {
		return
	}
	user, err := dataprovider.GetUserWithGroupSettings(share.Username, "")
	if err != nil {
		return
	}
	for _, sharedPath := range share.Paths {
		inUse := false
		for _, live := range livePaths {
			if sharedPathsOverlap(sharedPath, live) {
				inUse = true
				break
			}
		}
		if inUse {
			continue
		}
		fsys, err := user.GetFilesystemForPath(sharedPath, "")
		if err != nil || !vfs.IsLocalOsFs(fsys) {
			continue
		}
		resolved, err := fsys.ResolvePath(sharedPath)
		if err != nil {
			continue
		}
		removed := clearCacheFor(resolved)
		if removed > 0 {
			logger.Debug(logSender, "", "share cleanup: removed %d cached files for %q", removed, sharedPath)
		}
	}
}

// clearCacheFor removes the cache of a file, or every cache inside a folder
func clearCacheFor(resolved string) int {
	info, err := os.Stat(resolved)
	if err != nil {
		return 0
	}
	if !info.IsDir() {
		return clearCacheDir(filepath.Join(filepath.Dir(resolved), sharePreviewCacheDir), filepath.Base(resolved))
	}
	removed := 0
	_ = filepath.WalkDir(resolved, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable folders are skipped, the rest is still cleared
		}
		if d.IsDir() && d.Name() == sharePreviewCacheDir {
			removed += clearCacheDir(p, "")
			return filepath.SkipDir
		}
		return nil
	})
	return removed
}

// clearCacheDir removes the assets of one source, or all of them, from a .cache folder, and the
// folder once empty. Only names written by the preview and zip code are touched
func clearCacheDir(cacheDir, source string) int {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, entry := range entries {
		owner, _, _ := splitPreviewAssetName(entry.Name())
		if owner == "" || (source != "" && owner != source) {
			continue
		}
		if os.Remove(filepath.Join(cacheDir, entry.Name())) == nil {
			removed++
		}
	}
	_ = os.Remove(cacheDir)
	return removed
}

// shareWithKind is a share as listed to its owner, with what it points at: "file", "folder",
// "multiple" (several paths) or "missing" (the shared path no longer exists)
type shareWithKind struct {
	dataprovider.Share
	Kind string `json:"kind"`
	// ThumbURL is the small preview of a single-file share, shown in the shares list
	ThumbURL string `json:"thumb_url,omitempty"`
}

func withShareKinds(user *dataprovider.User, shares []dataprovider.Share) []shareWithKind {
	result := make([]shareWithKind, 0, len(shares))
	for _, share := range shares {
		item := shareWithKind{Share: share, Kind: getShareKind(user, share.Paths)}
		if item.Kind == "file" && sharePreviews != nil {
			base := path.Join(webClientPubSharesPath, share.ShareID, "preview", "thumb")
			switch sharePreviewExts[strings.ToLower(path.Ext(share.Paths[0]))] {
			case sharePreviewImage, sharePreviewVideo:
				item.ThumbURL = base + "?size=small"
			case sharePreviewAudio:
				// the waveform card, cropped by the list
				item.ThumbURL = base
			}
		}
		result = append(result, item)
	}
	return result
}

func getShareKind(user *dataprovider.User, paths []string) string {
	if len(paths) != 1 {
		return "multiple"
	}
	fsys, err := user.GetFilesystemForPath(paths[0], "")
	if err != nil {
		return "missing"
	}
	resolved, err := fsys.ResolvePath(paths[0])
	if err != nil {
		return "missing"
	}
	info, err := fsys.Stat(resolved)
	switch {
	case err != nil:
		return "missing"
	case info.IsDir():
		return "folder"
	default:
		return "file"
	}
}

// warmLiveShares queues the previews of every live single-file share
func warmLiveShares() {
	for offset := 0; ; offset += 100 {
		users, err := dataprovider.GetUsers(100, offset, dataprovider.OrderASC, "")
		if err != nil {
			return
		}
		for _, user := range users {
			shares, err := userShares(user.Username)
			if err != nil {
				continue
			}
			for _, share := range shares {
				warmSharePreview(share)
			}
		}
		if len(users) < 100 {
			return
		}
	}
}

// isMissingShare: every shared path is gone while the folder that held it is still there, so the
// storage is reachable and the files were deleted or renamed
func isMissingShare(user *dataprovider.User, paths []string) bool {
	if len(paths) == 0 {
		return false
	}
	for _, sharedPath := range paths {
		if sharedPath == "/" {
			return false
		}
		fsys, err := user.GetFilesystemForPath(sharedPath, "")
		if err != nil {
			return false
		}
		resolved, err := fsys.ResolvePath(sharedPath)
		if err != nil {
			return false
		}
		if _, err := fsys.Stat(resolved); err == nil || !fsys.IsNotExist(err) {
			return false
		}
		parentResolved, err := fsys.ResolvePath(path.Dir(sharedPath))
		if err != nil {
			return false
		}
		if info, err := fsys.Stat(parentResolved); err != nil || !info.IsDir() {
			return false
		}
	}
	return true
}
