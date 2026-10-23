// Hidden entries in public shares (fork addition): names starting with a dot (".cache",
// ".DS_Store", "._" resource forks and so on) are not listed, not added to zip downloads
// and cannot be downloaded from a public share, also when requested by path. The owner
// still sees them through SFTP, WebDAV and the web client.

package httpd

import (
	"slices"
	"strings"

	"github.com/sftpgo/sdk"
)

const shareHiddenPattern = ".*"

// hideDotEntriesInShares returns the file pattern rules for a share connection: every rule
// hides dot names, since only the most specific rule applies to a path, and a rule on "/"
// covers the paths without one. Hidden directories hide everything below them.
func hideDotEntriesInShares(patterns []sdk.PatternsFilter) []sdk.PatternsFilter {
	result := make([]sdk.PatternsFilter, 0, len(patterns)+1)
	hasRoot := false
	for _, p := range patterns {
		p.AllowedPatterns = excludeDotNames(p.AllowedPatterns)
		p.DeniedPatterns = append(slices.Clone(p.DeniedPatterns), shareHiddenPattern)
		// a share viewer gets nothing out of seeing entries it cannot download
		p.DenyPolicy = sdk.DenyPolicyHide
		hasRoot = hasRoot || p.Path == "/"
		result = append(result, p)
	}
	if !hasRoot {
		result = append(result, sdk.PatternsFilter{
			Path:           "/",
			DeniedPatterns: []string{shareHiddenPattern},
			DenyPolicy:     sdk.DenyPolicyHide,
		})
	}
	return result
}

// excludeDotNames rewrites allowed patterns so they cannot match dot names: allowed patterns
// are checked before the denied ones, "*.wav" would otherwise let ".x.wav" through
func excludeDotNames(allowed []string) []string {
	if len(allowed) == 0 {
		return nil
	}
	result := make([]string, 0, len(allowed))
	for _, pattern := range allowed {
		switch {
		case strings.HasPrefix(pattern, "."):
			// explicitly allowed dot names stay hidden in shares
			continue
		case strings.HasPrefix(pattern, "*"):
			pattern = "[^.]" + pattern
		case strings.HasPrefix(pattern, "?"):
			pattern = "[^.]" + pattern[1:]
		}
		result = append(result, pattern)
	}
	if len(result) == 0 {
		// every allowed pattern was a dot name: an empty pattern matches no name
		return []string{""}
	}
	return result
}
