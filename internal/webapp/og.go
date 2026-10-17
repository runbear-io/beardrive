package webapp

import (
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// Open Graph tags: what a hub link looks like when it is pasted somewhere
// that unfurls it (Slack, Discord, iMessage, LinkedIn). Unfurlers do not run
// JavaScript, so everything here is emitted server-side — the SPA shell is
// templated in server.go's frontend(), the markdown share page in shares.go.
//
// The split of what each surface may say is deliberate: the shell is served
// UNAUTHENTICATED to anyone holding a URL, so it gets name-derived tags only
// (a basename and a project name). Content-derived tags (description, image)
// appear on share pages alone, where the content is already public by
// construction.

// ogMeta builds the meta block for one page. Values are escaped for an
// attribute; description and image are emitted only when they have something
// to say — an empty og:image renders as a broken card, so its absence is the
// correct output rather than an empty tag.
func ogMeta(title, desc, image, ogType, pageURL string) string {
	var b strings.Builder
	tag := func(prop, val string) {
		if val == "" {
			return
		}
		b.WriteString(`<meta property="` + prop + `" content="` + html.EscapeString(val) + `">`)
	}
	tag("og:title", title)
	tag("og:site_name", "BearDrive")
	tag("og:type", ogType)
	tag("og:url", pageURL)
	tag("og:description", desc)
	// Only an absolute http(s) URL is fetchable by an unfurler. A relative
	// src in a shared document is already broken on the live share page
	// (RenderMarkdown does no rewriting), so advertising it would only ever
	// produce a card with a missing image.
	if u, err := url.Parse(image); err == nil && u.Host != "" &&
		(u.Scheme == "http" || u.Scheme == "https") {
		tag("og:image", image)
	}
	return b.String()
}

// titleTag is the <title> element, escaped — kept here so server.go needs no
// new import to template the shell.
func titleTag(s string) string {
	return "<title>" + html.EscapeString(s) + "</title>"
}

// shellViews mirrors VIEW_ROUTES in frontend/src/router.ts (plus the legacy
// "insights" alias from LEGACY_VIEWS). A view added there and not here is
// cosmetic — the URL still names itself, just uncapitalized.
var shellViews = map[string]string{
	"dashboard": "Dashboard",
	"history":   "History",
	"install":   "Install",
	"settings":  "Settings",
	"insights":  "Dashboard", // renamed; see LEGACY_VIEWS
}

// shellTitle names one SPA URL, or returns "" meaning "serve the shell
// exactly as it has always been". Costs no storage access: the basename comes
// from the URL and the project name from an in-memory registry read, so this
// stays cheap on the hub's hottest handler. Reserved prefixes (join/, orgs/,
// billing/) and anything whose first segment isn't shaped like a project id
// never reach the registry at all.
func (s *Server) shellTitle(upath string) string {
	if upath == "" || upath == "index.html" {
		return ""
	}
	if s.Root == nil { // volume mode: the whole path is a file, no project name
		return path.Base(upath)
	}
	id, rest, _ := strings.Cut(upath, "/")
	if !projectIDRe.MatchString(id) {
		return "" // join/<token>, orgs/…, billing/…, or a typo
	}
	p, ok := s.Projects.Get(id)
	if !ok {
		// Unresolvable id: the generic shell, with no hint either way about
		// whether the project exists.
		return ""
	}
	switch {
	case rest == "":
		return p.Name
	case !strings.Contains(rest, "/"):
		if view, ok := shellViews[rest]; ok {
			return view + " — " + p.Name
		}
	}
	return path.Base(rest) + " — " + p.Name
}

var (
	ogTagRe   = regexp.MustCompile(`<[^>]*>`)
	ogParaRe  = regexp.MustCompile(`(?s)<p>(.*?)</p>`)
	ogImageRe = regexp.MustCompile(`<img[^>]*\ssrc="(https?://[^"]*)"`)
)

// shareDescription is what a share card says under its title: the
// frontmatter's own description when the author wrote one, otherwise the
// document's opening prose. Headings, code and images are skipped — only a
// <p> counts — so a document that starts with a title still describes itself.
func shareDescription(pairs []FrontmatterPair, body string) string {
	for _, p := range pairs {
		if strings.EqualFold(p.Key, "description") {
			if v := strings.TrimSpace(p.Value); v != "" {
				return truncateWords(v, 200)
			}
		}
	}
	m := ogParaRe.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	text := html.UnescapeString(ogTagRe.ReplaceAllString(m[1], ""))
	return truncateWords(strings.Join(strings.Fields(text), " "), 200)
}

// shareImage is the first image an unfurler could actually fetch. goldmark
// runs without WithUnsafe, so a raw <img> in the source never survives to be
// matched here — every hit came from markdown image syntax.
func shareImage(body string) string {
	m := ogImageRe.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return html.UnescapeString(m[1])
}

// truncateWords cuts to at most max runes, on a word boundary, with an
// ellipsis standing in for what was dropped.
func truncateWords(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	cut := string(r[:max-1])
	if i := strings.LastIndexAny(cut, " \t\n"); i > 0 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " \t\n") + "…"
}

// unfurlerUAs are the link expanders that fetch a pasted URL to build a card.
// Matching is by substring because each one appends version and contact text.
var unfurlerUAs = []string{
	"Slackbot-LinkExpanding",
	"Slack-ImgProxy",
	"Discordbot",
	"Twitterbot",
	"facebookexternalhit",
	"LinkedInBot",
	"TelegramBot",
	"WhatsApp",
}

func isUnfurler(ua string) bool {
	for _, u := range unfurlerUAs {
		if strings.Contains(ua, u) {
			return true
		}
	}
	return false
}

// needsCard reports whether a shared file gets an Open Graph card when an
// unfurler asks for it. Markdown already renders its own; images unfurl as
// themselves, and swapping one for a card would also swap the og:image fetch
// for a card, leaving nothing to show.
func needsCard(p string) bool {
	ext := strings.ToLower(path.Ext(p))
	if ext == ".md" || ext == ".markdown" {
		return false
	}
	return !strings.HasPrefix(contentType(p), "image/")
}

// cardHeadLimit bounds how much of a shared HTML file is read to find its
// title and description; both belong in <head>.
const cardHeadLimit = 64 << 10

var (
	cardTitleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	cardMetaRe  = regexp.MustCompile(`(?is)<meta\s[^>]*>`)
	cardAttrRe  = regexp.MustCompile(`(?is)\b(name|property|content)\s*=\s*("[^"]*"|'[^']*')`)
)

// htmlHead pulls the title, description and image an HTML document declares
// about itself, its own og:* winning over <title> and name="description".
func htmlHead(doc string) (title, desc, image string) {
	if m := cardTitleRe.FindStringSubmatch(doc); m != nil {
		title = m[1]
	}
	for _, tag := range cardMetaRe.FindAllString(doc, -1) {
		var key, content string
		for _, a := range cardAttrRe.FindAllStringSubmatch(tag, -1) {
			v := a[2][1 : len(a[2])-1]
			if strings.EqualFold(a[1], "content") {
				content = v
			} else {
				key = strings.ToLower(v)
			}
		}
		switch key {
		case "og:title":
			title = content
		case "og:description":
			desc = content
		case "description":
			if desc == "" {
				desc = content
			}
		case "og:image":
			image = content
		}
	}
	clean := func(s string) string {
		return strings.Join(strings.Fields(html.UnescapeString(ogTagRe.ReplaceAllString(s, ""))), " ")
	}
	return truncateWords(clean(title), 200), truncateWords(clean(desc), 200), html.UnescapeString(image)
}

// serveShareCard answers an unfurler with an Open Graph page for a shared
// file that is not markdown or an image. HTML is described by its own head;
// anything else by its name, kind, size and age. Content stays inside the
// share's public scope: the link already serves these bytes to anyone.
func (s *Server) serveShareCard(w http.ResponseWriter, r *http.Request, v *volume, sp string, fi FileInfo) {
	title, desc, image := path.Base(sp), "", ""
	ext := strings.ToLower(path.Ext(sp))
	if ext == ".html" || ext == ".htm" {
		if rc, err := v.source.Open(r.Context(), sp, fi); err == nil {
			head, _ := io.ReadAll(io.LimitReader(rc, cardHeadLimit))
			rc.Close()
			t, d, i := htmlHead(string(head))
			if t != "" {
				title = t
			}
			desc, image = d, i
		}
	}
	if desc == "" {
		kind := strings.TrimPrefix(strings.ToUpper(ext), ".")
		if kind == "" {
			kind = "File"
		}
		desc = kind + " · " + humanSize(fi.Size)
		if !fi.Time.IsZero() {
			desc += " · Updated " + fi.Time.UTC().Format("2 Jan 2006")
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8">%s%s</head><body></body></html>`,
		titleTag(title), ogMeta(title, desc, image, "article", requestBaseURL(r)+r.URL.EscapedPath()))
}
