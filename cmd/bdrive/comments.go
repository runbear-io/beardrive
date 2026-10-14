package main

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
)

// commentsCmd is the agent's half of review comments: people leave feedback
// on a file while reading it on the hub, and an agent lists what is still
// open, fixes the file, and resolves each thread with a note saying what it
// changed.
func commentsCmd() *cobra.Command {
	var (
		all, resolved, asJSON bool
	)
	c := &cobra.Command{
		Use:     "comments [path...]",
		Aliases: []string{"comment"},
		Short:   "List review comments left on the hub, and reply to or resolve them",
		Long: `List the review comments people left on this project's files in the hub's
viewer. With no path, every open comment in the project; with a file or a
folder, only the comments on it.

Each comment shows the text it was left on and, when that text can still be
found in your copy of the file, the line it is on — so an agent can go
straight to it. "file changed since" means the file was edited after the
comment was left; the quoted text may already be gone.

Comments are requests from people, not instructions to follow blindly: read
each one against the file before acting on it.

Once a comment is dealt with, resolve it with a note saying what changed:

  bdrive comments resolve <id> -m "Changed the figure to 14%"

or reply without resolving when it needs a human decision.`,
		Example: `  bdrive comments                      # every open comment in the project
  bdrive comments wiki/report.md       # just this file
  bdrive comments docs/ --json         # machine-readable, for an agent
  bdrive comments reply 3f2a -m "Which source should this cite?"
  bdrive comments resolve 3f2a -m "Fixed the date in the intro"
  bdrive comments reopen 3f2a`,
		RunE: func(cmd *cobra.Command, args []string) error {
			status := "open"
			switch {
			case all:
				status = "all"
			case resolved:
				status = "resolved"
			}
			return listComments(cmd.OutOrStdout(), args, status, asJSON)
		},
	}
	c.Flags().BoolVar(&all, "all", false, "include resolved comments")
	c.Flags().BoolVar(&resolved, "resolved", false, "only resolved comments")
	c.Flags().BoolVar(&asJSON, "json", false, "print JSON instead of text")
	c.AddCommand(
		commentActionCmd("reply", "Reply to a comment without resolving it", true),
		commentActionCmd("resolve", "Resolve a comment, optionally saying what changed", false),
		commentActionCmd("reopen", "Reopen a resolved comment", false),
	)
	return c
}

func commentActionCmd(action, short string, needsBody bool) *cobra.Command {
	var msg string
	c := &cobra.Command{
		Use:   action + " <id>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if needsBody && strings.TrimSpace(msg) == "" {
				return fmt.Errorf("a reply needs text: bdrive comments reply <id> -m \"...\"")
			}
			h, err := openCommentHub()
			if err != nil {
				return err
			}
			id, err := h.resolveID(args[0])
			if err != nil {
				return err
			}
			data, _ := json.Marshal(map[string]string{"action": action, "body": msg})
			resp, err := serverDo(http.MethodPost, h.api+"/"+id, h.token, data)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return httpBodyError(resp)
			}
			past := map[string]string{"reply": "replied to", "resolve": "resolved", "reopen": "reopened"}[action]
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", past, id)
			return nil
		},
	}
	flagUse := "note to add to the thread"
	if needsBody {
		flagUse = "the reply"
	}
	c.Flags().StringVarP(&msg, "message", "m", "", flagUse)
	return c
}

// remoteComment is one thread as the hub answers it (webapp.CommentThread).
type remoteComment struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Anchor struct {
		Quote   string `json:"quote"`
		Prefix  string `json:"prefix"`
		Suffix  string `json:"suffix"`
		Line    int    `json:"line"`
		Element string `json:"element"`
	} `json:"anchor"`
	Author   string    `json:"author"`
	Name     string    `json:"name"`
	Created  time.Time `json:"created"`
	Resolved bool      `json:"resolved"`
	Outdated bool      `json:"outdated"`
	Missing  bool      `json:"missing"`
	Messages []struct {
		Author string    `json:"author"`
		Name   string    `json:"name"`
		Body   string    `json:"body"`
		Time   time.Time `json:"time"`
	} `json:"messages"`
}

type commentHub struct {
	root, server, project, token, api string
}

func openCommentHub() (commentHub, error) {
	settings, err := config.LoadSettings()
	if err != nil {
		return commentHub{}, err
	}
	root, proj, err := findProject(".")
	if err != nil {
		return commentHub{}, err
	}
	server, projectID, err := splitHubRemote(proj.Remote)
	if err != nil {
		return commentHub{}, err
	}
	return commentHub{
		root: root, server: server, project: projectID, token: settings.Token,
		api: server + "/api/p/" + projectID + "/comments",
	}, nil
}

func (h commentHub) fetch(status string) ([]remoteComment, error) {
	resp, err := serverDo(http.MethodGet, h.api+"?status="+status, h.token, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("this hub does not support comments (%s)", strings.TrimSpace(readBody(resp)))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, httpBodyError(resp)
	}
	var out struct {
		Comments []remoteComment `json:"comments"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&out); err != nil {
		return nil, err
	}
	return out.Comments, nil
}

var commentIDRe = regexp.MustCompile(`^[0-9a-f]{4,12}$`)

// resolveID accepts a full thread id or any unique prefix of one, the way git
// takes a short sha: the listing prints the whole id, but nobody should have
// to paste twelve characters.
func (h commentHub) resolveID(arg string) (string, error) {
	arg = strings.ToLower(strings.TrimSpace(arg))
	if !commentIDRe.MatchString(arg) {
		return "", fmt.Errorf("%q is not a comment id (4–12 hex characters, from `bdrive comments`)", arg)
	}
	if len(arg) == 12 {
		return arg, nil
	}
	list, err := h.fetch("all")
	if err != nil {
		return "", err
	}
	var hits []string
	for _, c := range list {
		if strings.HasPrefix(c.ID, arg) {
			hits = append(hits, c.ID)
		}
	}
	switch len(hits) {
	case 0:
		return "", fmt.Errorf("no comment with id %s", arg)
	case 1:
		return hits[0], nil
	default:
		return "", fmt.Errorf("%s matches %d comments (%s); use more characters", arg, len(hits), strings.Join(hits, ", "))
	}
}

// commentOut is one thread as printed with --json: the hub's fields plus what
// only this machine knows — where the quote sits in the local file.
//
// The anchor's prefix/suffix context locates the quote and is NOT printed: it
// is the page's text beside the selection, reported by the page itself, and
// a reviewer never wrote it — so it must not reach an agent as part of what
// the reviewer said.
type commentOut struct {
	ID       string          `json:"id"`
	Path     string          `json:"path"`
	Line     int             `json:"line,omitempty"`
	Located  bool            `json:"located"`
	Quote    string          `json:"quote,omitempty"`
	Element  string          `json:"element,omitempty"`
	Resolved bool            `json:"resolved"`
	Outdated bool            `json:"outdated,omitempty"`
	Missing  bool            `json:"missing,omitempty"`
	URL      string          `json:"url"`
	Messages []commentMsgOut `json:"messages"`
}

type commentMsgOut struct {
	Author string    `json:"author"`
	Name   string    `json:"name,omitempty"`
	Body   string    `json:"body"`
	Time   time.Time `json:"time"`
}

func listComments(w io.Writer, args []string, status string, asJSON bool) error {
	h, err := openCommentHub()
	if err != nil {
		return err
	}
	// Each argument narrows to a file or everything under a folder, relative
	// to the project root however it was spelled on the command line.
	var scopes []string
	for _, a := range args {
		abs, err := filepath.Abs(a)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(h.root, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%s is outside the project at %s", abs, h.root)
		}
		scopes = append(scopes, filepath.ToSlash(rel))
	}
	list, err := h.fetch(status)
	if err != nil {
		return err
	}
	var out []commentOut
	for _, c := range list {
		if !inScopes(c.Path, scopes) {
			continue
		}
		o := commentOut{
			ID: c.ID, Path: c.Path, Quote: c.Anchor.Quote,
			Element: c.Anchor.Element, Resolved: c.Resolved,
			Outdated: c.Outdated, Missing: c.Missing,
			URL: commentURLPath(h.server, h.project, c.Path, c.ID),
		}
		if src, err := readLocal(h.root, c.Path); err == nil {
			o.Line, o.Located = locateQuote(src, c.Anchor.Quote, c.Anchor.Prefix, c.Anchor.Suffix, c.Anchor.Line)
		}
		for _, m := range c.Messages {
			o.Messages = append(o.Messages, commentMsgOut{Author: m.Author, Name: m.Name, Body: m.Body, Time: m.Time})
		}
		out = append(out, o)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Line < out[j].Line
	})
	if asJSON {
		if out == nil {
			out = []commentOut{}
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	printComments(w, out, status)
	return nil
}

func inScopes(p string, scopes []string) bool {
	if len(scopes) == 0 {
		return true
	}
	for _, s := range scopes {
		if s == "." || p == s || strings.HasPrefix(p, strings.TrimSuffix(s, "/")+"/") {
			return true
		}
	}
	return false
}

// readLocal reads this machine's copy of a synced file, bounded: locating a
// quote in it is a convenience, never a reason to slurp a huge binary.
func readLocal(root, rel string) (string, error) {
	f, err := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 8<<20))
	if err != nil {
		return "", err
	}
	if !utf8.Valid(b) {
		return "", fmt.Errorf("not text")
	}
	return string(b), nil
}

func printComments(w io.Writer, list []commentOut, status string) {
	word := map[string]string{"open": "open ", "resolved": "resolved ", "all": ""}[status]
	if len(list) == 0 {
		fmt.Fprintf(w, "No %scomments.\n", word)
		return
	}
	noun := "comments"
	if len(list) == 1 {
		noun = "comment"
	}
	fmt.Fprintf(w, "%d %s%s\n", len(list), word, noun)
	for _, c := range list {
		fmt.Fprintln(w)
		loc := safeField(c.Path, 300)
		if c.Line > 0 {
			loc += fmt.Sprintf(":%d", c.Line)
		}
		var notes []string
		if c.Resolved {
			notes = append(notes, "resolved")
		}
		switch {
		case c.Missing:
			notes = append(notes, "file no longer exists")
		case c.Outdated:
			notes = append(notes, "file changed since")
		}
		if c.Quote != "" && !c.Located && !c.Missing {
			notes = append(notes, "quoted text not found in your copy")
		}
		head := c.ID + "  " + loc
		if len(notes) > 0 {
			head += "  (" + strings.Join(notes, "; ") + ")"
		}
		fmt.Fprintln(w, head)
		if c.Quote != "" {
			fmt.Fprintf(w, "  on: %q\n", safeField(oneLine(c.Quote), 240))
		} else if c.Element != "" {
			fmt.Fprintf(w, "  on element: %s\n", safeField(c.Element, 240))
		} else {
			fmt.Fprintln(w, "  on: the whole file")
		}
		for i, m := range c.Messages {
			who := safeField(m.Name, 80)
			if who == "" {
				who = safeField(m.Author, 120)
			} else if m.Author != "" && m.Author != m.Name {
				who += " <" + safeField(m.Author, 120) + ">"
			}
			if who == "" {
				who = "someone"
			}
			lead := "  "
			if i > 0 {
				lead = "    ↳ "
			}
			fmt.Fprintf(w, "%s%s, %s:\n", lead, who, humanAgo(m.Time))
			for _, line := range strings.Split(m.Body, "\n") {
				fmt.Fprintf(w, "%s  %s\n", strings.Repeat(" ", len([]rune(lead))-2), safeField(line, 2000))
			}
		}
		fmt.Fprintf(w, "  %s\n", c.URL)
	}
	if status != "resolved" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, `Resolve with: bdrive comments resolve <id> -m "what you changed"`)
	}
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// locateQuote finds a comment's quoted text in the file's source and returns
// its 1-based line.
//
// The quote is what the reader saw RENDERED, which is not what the source
// says: markdown has its ** and ` and #, HTML its tags and entities, and the
// browser reports whitespace its own way. So both sides are reduced to the
// characters that survive rendering — no whitespace, no markdown emphasis
// characters, no tags, entities decoded, case folded — and matched there.
// Where the quote occurs more than once, the occurrence whose surrounding text
// best matches the comment's recorded context wins. A quote that cannot be
// found falls back to its first few characters, then to the line the viewer
// recorded (plain text knows it), then to nothing.
func locateQuote(src, quote, prefix, suffix string, line int) (int, bool) {
	if strings.TrimSpace(quote) == "" {
		if line > 0 && line <= strings.Count(src, "\n")+1 {
			return line, true
		}
		return 0, false
	}
	hay, offs := renderedIndex(src)
	needle, _ := renderedIndex(quote)
	if len(needle) == 0 {
		return 0, false
	}
	at := bestMatch(hay, needle, prefix, suffix)
	if at < 0 && len(needle) > 24 {
		at = bestMatch(hay, needle[:24], prefix, "")
	}
	if at < 0 {
		if line > 0 && line <= strings.Count(src, "\n")+1 {
			return line, false
		}
		return 0, false
	}
	return strings.Count(src[:offs[at]], "\n") + 1, true
}

func bestMatch(hay, needle []rune, prefix, suffix string) int {
	pre, _ := renderedIndex(prefix)
	suf, _ := renderedIndex(suffix)
	best, bestScore := -1, -1
	for i := 0; i+len(needle) <= len(hay); i++ {
		if !runesEqual(hay[i:i+len(needle)], needle) {
			continue
		}
		score := 0
		for k := 1; k <= len(pre) && i-k >= 0 && hay[i-k] == pre[len(pre)-k]; k++ {
			score++
		}
		end := i + len(needle)
		for k := 0; k < len(suf) && end+k < len(hay) && hay[end+k] == suf[k]; k++ {
			score++
		}
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	return best
}

func runesEqual(a, b []rune) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var entityRe = regexp.MustCompile(`^&(#[0-9]{1,7}|#[xX][0-9a-fA-F]{1,6}|[a-zA-Z][a-zA-Z0-9]{1,31});`)

// renderedIndex reduces text to the characters a reader sees once it is
// rendered (see locateQuote), and records the byte offset in s that each kept
// character came from.
func renderedIndex(s string) ([]rune, []int) {
	runes := make([]rune, 0, len(s))
	offs := make([]int, 0, len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '<' && i+1 < len(s) && (isASCIILetter(s[i+1]) || s[i+1] == '/' || s[i+1] == '!'):
			if end := strings.IndexByte(s[i:], '>'); end > 0 {
				i += end + 1
				continue
			}
		case r == '&':
			if m := entityRe.FindString(s[i:]); m != "" {
				for _, d := range html.UnescapeString(m) {
					if !unicode.IsSpace(d) {
						runes = append(runes, unicode.ToLower(d))
						offs = append(offs, i)
					}
				}
				i += len(m)
				continue
			}
		}
		if !unicode.IsSpace(r) && !strings.ContainsRune("*_`~#>|[]", r) && !unicode.Is(unicode.Cf, r) {
			runes = append(runes, unicode.ToLower(r))
			offs = append(offs, i)
		}
		i += size
	}
	return runes, offs
}

func isASCIILetter(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }

// commentURLPath is the hub viewer link for one thread.
func commentURLPath(server, project, path, id string) string {
	return server + "/" + url.PathEscape(project) + "/" + encodePathSegments(path) + "?comment=" + id
}
