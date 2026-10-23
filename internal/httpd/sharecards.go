// Link previews for direct links and robots.txt (fork addition).
//
// A direct download link pasted in a chat or a post is fetched by the platform's link preview
// bot: it gets the share page with its Open Graph tags, the card link staying the direct link,
// while people opening the link get the file as before. Such fetches never count as share uses.
// robots.txt keeps search engines and scrapers out and lets only link preview bots read the
// share pages and their pictures.

package httpd

import (
	"context"
	"net/http"
	"strings"

	"github.com/drakkan/sftpgo/v2/internal/dataprovider"
)

// linkPreviewBots are user agent fragments of link preview fetchers, lower case
var linkPreviewBots = []string{
	"facebookexternalhit", "facebot", "twitterbot", "discordbot", "slackbot", "slack-imgproxy",
	"telegrambot", "whatsapp", "linkedinbot", "pinterest", "redditbot", "tumblr", "embedly", "iframely",
	"skypeuripreview", "mastodon", "bluesky", "cardyb", "vkshare", "xing-contenttabreceiver",
}

func isLinkPreviewBot(r *http.Request) bool {
	ua := strings.ToLower(r.UserAgent())
	if ua == "" {
		return false
	}
	for _, bot := range linkPreviewBots {
		if strings.Contains(ua, bot) {
			return true
		}
	}
	return false
}

type ogURLKey struct{}

// withOGURL makes the share page tell link previews to open this address when the card is
// clicked: the direct link the bot was given
func withOGURL(ctx context.Context, link string) context.Context {
	return context.WithValue(ctx, ogURLKey{}, link)
}

func ogURLFrom(ctx context.Context) string {
	link, _ := ctx.Value(ogURLKey{}).(string)
	return link
}

func requestAbsoluteURL(r *http.Request) string {
	return getRequestBaseURL(r) + r.URL.RequestURI()
}

const robotsTxt = `# Shares are private links: no listing in search engines or by scrapers.
# Link preview bots may read share pages and their pictures to show link cards.
User-agent: facebookexternalhit
User-agent: Facebot
User-agent: Twitterbot
User-agent: Discordbot
User-agent: Slackbot
User-agent: Slackbot-LinkExpanding
User-agent: TelegramBot
User-agent: WhatsApp
User-agent: LinkedInBot
User-agent: Pinterestbot
User-agent: redditbot
User-agent: Tumblr
User-agent: Embedly
User-agent: Iframely
User-agent: SkypeUriPreview
User-agent: Mastodon
User-agent: Bluesky
Allow: /web/client/pubshares/
Disallow: /

User-agent: *
Disallow: /
`

func handleRobotsTxt(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write([]byte(robotsTxt))
}

// handleSharePreviewCard serves the share card, the picture of zip and folder share previews
func (s *httpdServer) handleSharePreviewCard(w http.ResponseWriter, r *http.Request) {
	validScopes := []dataprovider.ShareScope{dataprovider.ShareScopeRead, dataprovider.ShareScopeReadWrite}
	_, connection, err := s.checkPublicShare(w, r, validScopes)
	if err != nil {
		return
	}
	connection.CloseFS()
	s.serveShareCard(w, r, "folder")
}
