// Removal of dead shares (fork addition): shares that expired or used up their download limit
// are deleted by an hourly task, so the database only holds shares that still work. Until then
// the link tells why it stopped working and the owner can still extend the share; afterwards it
// says the link is no longer available. The previews and zip checksums kept for a removed share are cleared from the
// .cache folders, unless another live share still covers the same files.

package httpd

import (
	"context"
	"io/fs"
	"os"
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
			time.Sleep(time.Minute) // a first pass soon after the start
			_, _ = tasks.submit("share-cleanup", "periodic", laneBackground, 0, 30*time.Minute, cleanup)
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
	var dead []dataprovider.Share
	var livePaths []string
	for idx := range shares {
		if isDeadShare(&shares[idx]) {
			dead = append(dead, shares[idx])
		} else {
			livePaths = append(livePaths, shares[idx].Paths...)
		}
	}
	for _, share := range dead {
		reason := "expired"
		if share.MaxTokens > 0 && share.UsedTokens >= share.MaxTokens {
			reason = "download limit reached"
		}
		if err := dataprovider.DeleteShare(share.ShareID, share.Username, "", ""); err != nil {
			logger.Warn(logSender, "", "share cleanup: unable to remove share %q: %v", share.ShareID, err)
			continue
		}
		logger.Info(logSender, "", "share cleanup: removed share %q (%q) of %q: %s", share.ShareID, share.Name,
			share.Username, reason)
		clearShareCache(share, livePaths)
	}
}

// clearShareCacheLater clears the cache of a share deleted by its owner
func clearShareCacheLater(share dataprovider.Share) {
	_, _ = tasks.submit("share-cache-clear", share.ShareID, laneBackground, 0, 30*time.Minute,
		func(_ context.Context) error {
			shareRemovals.Lock()
			defer shareRemovals.Unlock()
			shares, err := userShares(share.Username)
			if err != nil {
				return err
			}
			var livePaths []string
			for idx := range shares {
				livePaths = append(livePaths, shares[idx].Paths...)
			}
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
}

func withShareKinds(user *dataprovider.User, shares []dataprovider.Share) []shareWithKind {
	result := make([]shareWithKind, 0, len(shares))
	for _, share := range shares {
		result = append(result, shareWithKind{Share: share, Kind: getShareKind(user, share.Paths)})
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
