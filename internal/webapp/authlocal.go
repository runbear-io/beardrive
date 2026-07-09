package webapp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// BuiltinAuth is the open-source identity provider: email + password + name
// accounts and long-lived device tokens, persisted in one JSON file (loaded
// at open, rewritten atomically on every change — same discipline as the
// project registry). It owns the /auth/* pages the browser sees and the
// /api/auth/* endpoints the CLI uses.
type BuiltinAuth struct {
	AllowSignup bool
	Mail        *Mailer // nil → reset links go to the server log

	// Public-URL signup gating (all optional; set after Open). A hub reachable
	// from the internet should use at least one of these.
	AllowedDomains      []string        // if non-empty, signup email domain must match one
	RequireVerification bool            // new accounts must click an email link before activation
	RequireApproval     bool            // new accounts wait for an admin to approve them
	Admins              map[string]bool // hub admins (lowercase emails): approve users, govern shares
	Brand               string          // optional name shown on the sign-in page

	path string

	mu     sync.Mutex
	users  map[string]*authUser // by id
	tokens map[string]authToken // by sha256(token)

	// Ephemeral single-use state; a server restart just cancels pending
	// logins and resets.
	pending map[string]pendingGrant // auth codes, device codes, reset tokens
}

type authUser struct {
	ID      string    `json:"id"`
	Email   string    `json:"email"`
	Name    string    `json:"name"`
	Pass    string    `json:"pass"` // bcrypt hash
	Status  string    `json:"status,omitempty"`
	Created time.Time `json:"created"`
}

// Account status. Empty is treated as active so accounts created before
// gating existed keep working.
const (
	statusActive     = "active"
	statusUnverified = "unverified" // awaiting email verification
	statusPending    = "pending"    // verified (or verification off) but awaiting admin approval
)

func (u *authUser) active() bool { return u.Status == "" || u.Status == statusActive }

type authToken struct {
	Hash    string    `json:"hash"` // sha256 of the token; plaintext is never stored
	User    string    `json:"user"`
	Device  string    `json:"device"`
	Created time.Time `json:"created"`
}

type pendingGrant struct {
	kind    string // "code" (CLI callback), "device" (poll flow), "reset"
	user    string // set once granted
	device  string // device flow: requested device name
	granted bool
	expires time.Time
}

// OpenBuiltinAuth loads (or starts) the account registry at path.
func OpenBuiltinAuth(path string, allowSignup bool, mail *Mailer) (*BuiltinAuth, error) {
	a := &BuiltinAuth{
		AllowSignup: allowSignup, Mail: mail, path: path,
		users:   make(map[string]*authUser),
		tokens:  make(map[string]authToken),
		pending: make(map[string]pendingGrant),
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return a, nil
		}
		return nil, err
	}
	var file struct {
		Users  []*authUser `json:"users"`
		Tokens []authToken `json:"tokens"`
		Policy *authPolicy `json:"policy,omitempty"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, u := range file.Users {
		a.users[u.ID] = u
	}
	for _, t := range file.Tokens {
		a.tokens[t.Hash] = t
	}
	// A UI-saved policy is the persisted operational default; the server
	// config can still override it at startup (see web.go), so a sysadmin who
	// pins a value in the config file always wins over a browser toggle.
	if file.Policy != nil {
		a.RequireVerification = file.Policy.RequireVerification
		a.RequireApproval = file.Policy.RequireApproval
	}
	return a, nil
}

// authPolicy is the UI-tunable slice of gating (persisted in auth.json).
// Domain allowlist and the admin list are intentionally NOT here — they are
// security-critical identity config owned by whoever controls the server,
// not something a browser session should be able to widen.
type authPolicy struct {
	RequireVerification bool `json:"require_verification"`
	RequireApproval     bool `json:"require_approval"`
}

// SetPolicy updates the tunable gating toggles and persists them.
func (a *BuiltinAuth) SetPolicy(requireVerification, requireApproval bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.RequireVerification = requireVerification
	a.RequireApproval = requireApproval
	return a.save()
}

// save persists users and tokens. Callers hold mu.
func (a *BuiltinAuth) save() error {
	var file struct {
		Users  []*authUser `json:"users"`
		Tokens []authToken `json:"tokens"`
		Policy *authPolicy `json:"policy,omitempty"`
	}
	for _, u := range a.users {
		file.Users = append(file.Users, u)
	}
	for _, t := range a.tokens {
		file.Tokens = append(file.Tokens, t)
	}
	file.Policy = &authPolicy{RequireVerification: a.RequireVerification, RequireApproval: a.RequireApproval}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(a.path), ".bdrive-tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil { // holds password hashes
		return err
	}
	return os.Rename(tmp.Name(), a.path)
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// ---- account + token operations ----

func (a *BuiltinAuth) findByEmail(email string) *authUser {
	for _, u := range a.users {
		if strings.EqualFold(u.Email, email) {
			return u
		}
	}
	return nil
}

func (a *BuiltinAuth) signup(email, name, password string) (*authUser, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	name = strings.TrimSpace(name)
	if email == "" || !strings.Contains(email, "@") {
		return nil, fmt.Errorf("a valid email is required")
	}
	if !a.domainAllowed(email) {
		return nil, fmt.Errorf("this server only accepts %s email addresses", a.domainList())
	}
	if name == "" {
		return nil, fmt.Errorf("a name is required")
	}
	if len(password) < 8 {
		return nil, fmt.Errorf("password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.findByEmail(email) != nil {
		return nil, fmt.Errorf("an account with this email already exists")
	}
	u := &authUser{
		ID: "u-" + randHex(4), Email: email, Name: name,
		Pass: string(hash), Status: a.initialStatus(), Created: time.Now().UTC(),
	}
	a.users[u.ID] = u
	if err := a.save(); err != nil {
		delete(a.users, u.ID)
		return nil, err
	}
	return u, nil
}

// initialStatus is the account state a new signup starts in, given the
// server's gating config: verify first, else approve first, else active.
func (a *BuiltinAuth) initialStatus() string {
	switch {
	case a.RequireVerification:
		return statusUnverified
	case a.RequireApproval:
		return statusPending
	default:
		return statusActive
	}
}

// afterVerify is the state a just-verified account moves to.
func (a *BuiltinAuth) afterVerify() string {
	if a.RequireApproval {
		return statusPending
	}
	return statusActive
}

func emailDomain(email string) string {
	if i := strings.LastIndex(email, "@"); i >= 0 {
		return strings.ToLower(email[i+1:])
	}
	return ""
}

func (a *BuiltinAuth) domainAllowed(email string) bool {
	if len(a.AllowedDomains) == 0 {
		return true
	}
	d := emailDomain(email)
	for _, allowed := range a.AllowedDomains {
		if strings.EqualFold(strings.TrimPrefix(strings.TrimSpace(allowed), "@"), d) {
			return true
		}
	}
	return false
}

func (a *BuiltinAuth) domainList() string {
	parts := make([]string, len(a.AllowedDomains))
	for i, d := range a.AllowedDomains {
		parts[i] = "@" + strings.TrimPrefix(strings.TrimSpace(d), "@")
	}
	return strings.Join(parts, ", ")
}

func (a *BuiltinAuth) isAdmin(email string) bool {
	return a.Admins != nil && a.Admins[normEmail(email)]
}

func (a *BuiltinAuth) verifyPassword(email, password string) *authUser {
	a.mu.Lock()
	u := a.findByEmail(email)
	a.mu.Unlock()
	if u == nil {
		// burn comparable time so missing accounts aren't detectable
		bcrypt.CompareHashAndPassword([]byte("$2a$10$0000000000000000000000000000000000000000000000000000"), []byte(password))
		return nil
	}
	if bcrypt.CompareHashAndPassword([]byte(u.Pass), []byte(password)) != nil {
		return nil
	}
	return u
}

// issueToken mints a device token for the user and persists its hash. The
// plaintext is returned exactly once.
func (a *BuiltinAuth) issueToken(userID, device string) (string, error) {
	tok := "bdt_" + randHex(20)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tokens[hashToken(tok)] = authToken{
		Hash: hashToken(tok), User: userID, Device: device, Created: time.Now().UTC(),
	}
	if err := a.save(); err != nil {
		delete(a.tokens, hashToken(tok))
		return "", err
	}
	return tok, nil
}

func (a *BuiltinAuth) revokeToken(tok string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.tokens[hashToken(tok)]; ok {
		delete(a.tokens, hashToken(tok))
		a.save()
	}
}

func (a *BuiltinAuth) userForToken(tok string) (User, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.tokens[hashToken(tok)]
	if !ok {
		return User{}, false
	}
	u, ok := a.users[t.User]
	if !ok || !u.active() {
		return User{}, false
	}
	return User{ID: u.ID, Email: u.Email, Name: u.Name, Admin: a.isAdmin(u.Email)}, true
}

// sendVerification emails (or logs) a verification link for the account.
func (a *BuiltinAuth) sendVerification(r *http.Request, u *authUser) {
	tok := a.newGrant("verify", u.ID, "", true, 24*time.Hour)
	link := requestBaseURL(r) + "/auth/verify?token=" + tok
	subject := "Verify your BearDrive account"
	body := "Confirm your email to activate your BearDrive account:\n\n  " + link +
		"\n\nThis link is valid for 24 hours. If you didn't sign up, ignore this email."
	if a.Mail == nil {
		fmt.Printf("verification link for %s:\n  %s\n", u.Email, link)
		return
	}
	if err := a.Mail.Send(u.Email, subject, body); err != nil {
		fmt.Printf("verification link for %s (email not sent: %v):\n  %s\n", u.Email, err, link)
	}
}

// grant helpers: single-use codes with expiry.
func (a *BuiltinAuth) newGrant(kind, user, device string, granted bool, ttl time.Duration) string {
	id := randHex(16)
	a.mu.Lock()
	a.pending[id] = pendingGrant{kind: kind, user: user, device: device, granted: granted, expires: time.Now().Add(ttl)}
	a.mu.Unlock()
	return id
}

func (a *BuiltinAuth) takeGrant(kind, id string) (pendingGrant, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	g, ok := a.pending[id]
	if !ok || g.kind != kind || time.Now().After(g.expires) {
		delete(a.pending, id)
		return pendingGrant{}, false
	}
	delete(a.pending, id)
	return g, true
}

// peekGrant reads without consuming (device polling).
func (a *BuiltinAuth) peekGrant(kind, id string) (pendingGrant, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	g, ok := a.pending[id]
	if !ok || g.kind != kind || time.Now().After(g.expires) {
		return pendingGrant{}, false
	}
	return g, true
}

func (a *BuiltinAuth) grantDevice(id, userID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	g, ok := a.pending[id]
	if !ok || g.kind != "device" || time.Now().After(g.expires) {
		return false
	}
	g.user, g.granted = userID, true
	a.pending[id] = g
	return true
}

// PendingUsers lists accounts awaiting admin approval, oldest first.
func (a *BuiltinAuth) PendingUsers() []User {
	a.mu.Lock()
	defer a.mu.Unlock()
	var us []*authUser
	for _, u := range a.users {
		if u.Status == statusPending {
			us = append(us, u)
		}
	}
	sort.Slice(us, func(i, j int) bool { return us[i].Created.Before(us[j].Created) })
	out := make([]User, len(us))
	for i, u := range us {
		out[i] = User{ID: u.ID, Email: u.Email, Name: u.Name}
	}
	return out
}

// Approve activates a pending account.
func (a *BuiltinAuth) Approve(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok := a.users[id]
	if !ok {
		return fmt.Errorf("no such account")
	}
	u.Status = statusActive
	return a.save()
}

// Deny removes a pending account.
func (a *BuiltinAuth) Deny(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.users[id]; !ok {
		return fmt.Errorf("no such account")
	}
	delete(a.users, id)
	return a.save()
}

// Accounts returns every account, oldest first (used by the org migration
// to pick the default org's owner).
func (a *BuiltinAuth) Accounts() []User {
	a.mu.Lock()
	defer a.mu.Unlock()
	users := make([]*authUser, 0, len(a.users))
	for _, u := range a.users {
		if u.active() {
			users = append(users, u)
		}
	}
	sort.Slice(users, func(i, j int) bool { return users[i].Created.Before(users[j].Created) })
	out := make([]User, len(users))
	for i, u := range users {
		out[i] = User{ID: u.ID, Email: u.Email, Name: u.Name}
	}
	return out
}

// ---- AuthProvider ----

const sessionCookie = "bdrive_session"

func (a *BuiltinAuth) CLILoginPath() string { return "/auth/cli" }

func (a *BuiltinAuth) Authenticate(r *http.Request) (User, bool) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return a.userForToken(strings.TrimPrefix(h, "Bearer "))
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		return a.userForToken(c.Value)
	}
	return User{}, false
}

func (a *BuiltinAuth) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", a.pageLogin)
	mux.HandleFunc("POST /auth/login", a.pageLogin)
	mux.HandleFunc("GET /auth/signup", a.pageSignup)
	mux.HandleFunc("POST /auth/signup", a.pageSignup)
	mux.HandleFunc("GET /auth/logout", a.pageLogout)
	mux.HandleFunc("GET /auth/cli", a.pageCLI)
	mux.HandleFunc("GET /auth/device", a.pageDevice)
	mux.HandleFunc("POST /auth/device", a.pageDevice)
	mux.HandleFunc("GET /auth/verify", a.pageVerify)
	mux.HandleFunc("GET /auth/reset", a.pageReset)
	mux.HandleFunc("POST /auth/reset", a.pageReset)
	mux.HandleFunc("GET /auth/reset/confirm", a.pageResetConfirm)
	mux.HandleFunc("POST /auth/reset/confirm", a.pageResetConfirm)
	mux.HandleFunc("POST /api/auth/exchange", a.apiExchange)
	mux.HandleFunc("POST /api/auth/device/start", a.apiDeviceStart)
	mux.HandleFunc("POST /api/auth/device/poll", a.apiDevicePoll)
	mux.HandleFunc("GET /api/auth/me", a.apiMe)
}

// sessionUser resolves the browser session (cookie only, not Bearer).
func (a *BuiltinAuth) sessionUser(r *http.Request) (User, bool) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		return a.userForToken(c.Value)
	}
	return User{}, false
}

func (a *BuiltinAuth) startSession(w http.ResponseWriter, userID string) error {
	tok, err := a.issueToken(userID, "web-session")
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// inviteBanner shows an invitation cue when the post-login destination is a
// join link, so a visitor who clicked an invite knows why they're here.
func inviteBanner(next string) string {
	if !strings.Contains(next, "join/") && !strings.Contains(next, "join%2F") {
		return ""
	}
	return `<p class="msg" style="margin:0 0 14px">You've been invited to a team. Sign in (or sign up) to accept.</p>`
}

// safeNext keeps post-login redirects on this site.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}

// ---- pages ----

func authPage(w http.ResponseWriter, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><title>%s — BearDrive</title>
<style>
/* Shares the app's token values so sign-in and the app read as one product. */
body{font:15px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:#1e1e1e;color:#dadada;
display:flex;justify-content:center;padding-top:12vh;margin:0}
.card{background:#262626;border:1px solid #363636;border-radius:12px;padding:28px 32px;width:340px}
h1{font-size:17px;margin:0 0 16px}
label{display:block;font-size:12.5px;color:#a6a6a6;margin:12px 0 4px}
input{width:100%%;box-sizing:border-box;padding:9px 11px;border-radius:6px;border:1px solid #363636;
background:#1e1e1e;color:#dadada;font:inherit}
input:focus-visible{outline:2px solid #a882ff;outline-offset:1px;border-color:#a882ff}
button{margin-top:18px;width:100%%;padding:10px;border:none;border-radius:6px;background:#6a48e0;
color:#fff;font:inherit;font-weight:600;cursor:pointer}
button:hover{background:#7c5cd6}
button:focus-visible{outline:2px solid #c9b3ff;outline-offset:2px}
.err{color:#ff9b91;font-size:13px;margin:10px 0 0}
.msg{color:#8ce59a;font-size:13px;margin:10px 0 0}
.alt{margin-top:16px;font-size:12.5px;color:#8a8a8a}
.alt a{color:#c9b3ff;text-decoration:none}
.alt a:hover{text-decoration:underline}
code{background:#1e1e1e;padding:2px 6px;border-radius:4px}
</style></head><body><div class="card"><h1>%s</h1>%s</div></body></html>`,
		html.EscapeString(title), html.EscapeString(title), body)
}

func field(label, name, typ, value string) string {
	return fmt.Sprintf(`<label>%s</label><input name=%q type=%q value=%q required>`,
		html.EscapeString(label), name, typ, html.EscapeString(value))
}

func (a *BuiltinAuth) pageLogin(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.FormValue("next"))
	var errMsg string
	if r.Method == http.MethodPost {
		if u := a.verifyPassword(r.FormValue("email"), r.FormValue("password")); u != nil {
			switch u.Status {
			case statusUnverified:
				a.sendVerification(r, u)
				errMsg = `<p class="err">Please verify your email first — we've re-sent the link.</p>`
			case statusPending:
				errMsg = `<p class="err">Your account is still awaiting administrator approval.</p>`
			default:
				if err := a.startSession(w, u.ID); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				http.Redirect(w, r, next, http.StatusSeeOther)
				return
			}
		} else {
			errMsg = `<p class="err">Wrong email or password.</p>`
		}
	}
	signup := ""
	if a.AllowSignup {
		note := ""
		if len(a.AllowedDomains) > 0 {
			note = ` <span style="color:#888">(` + html.EscapeString(a.domainList()) + ` only)</span>`
		}
		signup = fmt.Sprintf(`<p class="alt">No account? <a href="/auth/signup?next=%s">Sign up</a>%s</p>`, url.QueryEscape(next), note)
	}
	brand := ""
	if a.Brand != "" {
		brand = `<p class="alt" style="margin:0 0 14px;color:#aaa">` + html.EscapeString(a.Brand) + `</p>`
	}
	authPage(w, "Sign in", brand+inviteBanner(next)+fmt.Sprintf(`<form method="post" action="/auth/login?next=%s">%s%s%s<button>Sign in</button></form>
%s<p class="alt"><a href="/auth/reset">Forgot password?</a></p>`,
		url.QueryEscape(next),
		field("Email", "email", "email", r.FormValue("email")),
		field("Password", "password", "password", ""),
		errMsg, signup))
}

func (a *BuiltinAuth) pageSignup(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.FormValue("next"))
	if !a.AllowSignup {
		authPage(w, "Sign up disabled", `<p>This server does not allow self-signup. Ask the server admin for an account.</p>
<p class="alt"><a href="/auth/login">Back to sign in</a></p>`)
		return
	}
	var errMsg string
	if r.Method == http.MethodPost {
		u, err := a.signup(r.FormValue("email"), r.FormValue("name"), r.FormValue("password"))
		if err == nil {
			switch u.Status {
			case statusUnverified:
				a.sendVerification(r, u)
				authPage(w, "Verify your email", `<p class="msg">Almost there — we sent a verification link to <b>`+
					html.EscapeString(u.Email)+`</b>.</p><p class="alt">Click it to activate your account. No email on this server? The link is in the server log.</p>`)
				return
			case statusPending:
				authPage(w, "Awaiting approval", `<p class="msg">Thanks — your account was created and is waiting for an administrator to approve it.</p>
<p class="alt">You'll be able to sign in once it's approved.</p>`)
				return
			}
			if err := a.startSession(w, u.ID); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			http.Redirect(w, r, next, http.StatusSeeOther)
			return
		}
		errMsg = `<p class="err">` + html.EscapeString(err.Error()) + `</p>`
	}
	// State the domain restriction up front, where the stranger types their
	// email — not only after a rejected submit.
	domainNote := ""
	if len(a.AllowedDomains) > 0 {
		domainNote = `<p class="alt" style="margin:2px 0 0">Only ` + html.EscapeString(a.domainList()) + ` email addresses can sign up here.</p>`
	}
	brand := ""
	if a.Brand != "" {
		brand = `<p class="alt" style="margin:0 0 14px;color:#aaa">` + html.EscapeString(a.Brand) + `</p>`
	}
	authPage(w, "Create account", brand+inviteBanner(next)+fmt.Sprintf(`<form method="post" action="/auth/signup?next=%s">%s%s%s%s%s<button>Sign up</button></form>
<p class="alt">Have an account? <a href="/auth/login?next=%s">Sign in</a></p>`,
		url.QueryEscape(next),
		field("Name", "name", "text", r.FormValue("name")),
		field("Email", "email", "email", r.FormValue("email")),
		domainNote,
		field("Password (min 8 chars)", "password", "password", ""),
		errMsg, url.QueryEscape(next)))
}

func (a *BuiltinAuth) pageLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.revokeToken(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
}

// pageCLI completes `bdrive login`: once the browser has a session, mint a
// one-time code and bounce it to the CLI's loopback listener. Redirects are
// restricted to loopback addresses so the code can't be sent anywhere else.
func (a *BuiltinAuth) pageCLI(w http.ResponseWriter, r *http.Request) {
	redirect := r.URL.Query().Get("redirect")
	state := r.URL.Query().Get("state")
	u, err := url.Parse(redirect)
	if err != nil || (u.Scheme != "http") || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") {
		http.Error(w, "invalid redirect (must be a loopback URL)", http.StatusBadRequest)
		return
	}
	user, ok := a.sessionUser(r)
	if !ok {
		http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.String()), http.StatusSeeOther)
		return
	}
	code := a.newGrant("code", user.ID, "", true, time.Minute)
	q := u.Query()
	q.Set("code", code)
	q.Set("state", state)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

// pageDevice is the headless-login approval page: the user types the code
// `bdrive login` printed.
func (a *BuiltinAuth) pageDevice(w http.ResponseWriter, r *http.Request) {
	user, ok := a.sessionUser(r)
	if !ok {
		http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	var msg string
	if r.Method == http.MethodPost {
		code := strings.ToLower(strings.TrimSpace(r.FormValue("code")))
		if a.grantDevice(code, user.ID) {
			authPage(w, "Device connected", `<p class="msg">Done — you can close this tab. The terminal will finish logging in.</p>`)
			return
		}
		msg = `<p class="err">Unknown or expired code.</p>`
	}
	code := r.URL.Query().Get("code")
	authPage(w, "Connect a device", fmt.Sprintf(`<p>Enter the code shown by <code>bdrive login</code>:</p>
<form method="post">%s%s<button>Approve</button></form>`,
		field("Code", "code", "text", code), msg))
}

// pageVerify activates an account from an email link, then either starts a
// session (or explains it's now awaiting approval).
func (a *BuiltinAuth) pageVerify(w http.ResponseWriter, r *http.Request) {
	g, ok := a.takeGrant("verify", r.URL.Query().Get("token"))
	if !ok {
		authPage(w, "Link expired", `<p class="err">This verification link is invalid or expired.</p>
<p class="alt"><a href="/auth/login">Back to sign in</a></p>`)
		return
	}
	a.mu.Lock()
	u := a.users[g.user]
	next := a.afterVerify()
	if u != nil && u.Status == statusUnverified {
		u.Status = next
		a.save()
	}
	a.mu.Unlock()
	if u != nil && u.Status == statusPending {
		authPage(w, "Email verified", `<p class="msg">Your email is verified. Your account is now waiting for an administrator to approve it.</p>`)
		return
	}
	if u != nil {
		if err := a.startSession(w, u.ID); err == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}
	authPage(w, "Email verified", `<p class="msg">Your email is verified.</p><p class="alt"><a href="/auth/login">Sign in</a></p>`)
}

func (a *BuiltinAuth) pageReset(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		email := strings.TrimSpace(strings.ToLower(r.FormValue("email")))
		a.mu.Lock()
		u := a.findByEmail(email)
		a.mu.Unlock()
		if u != nil {
			tok := a.newGrant("reset", u.ID, "", true, time.Hour)
			link := requestBaseURL(r) + "/auth/reset/confirm?token=" + tok
			subject := "Reset your BearDrive password"
			body := "Someone (hopefully you) asked to reset the BearDrive password for " + u.Email +
				".\n\nReset it here (valid for 1 hour):\n\n  " + link + "\n\nIf this wasn't you, ignore this email."
			if err := a.Mail.Send(u.Email, subject, body); err != nil {
				// Never break reset: the admin can hand over the logged link.
				fmt.Printf("password reset for %s (email not sent: %v):\n  %s\n", u.Email, err, link)
			}
		}
		authPage(w, "Check your email", `<p class="msg">If that account exists, a reset link is on its way.</p>
<p class="alt">No email configured on this server? The link is in the server log.</p>`)
		return
	}
	authPage(w, "Reset password", fmt.Sprintf(`<form method="post">%s<button>Send reset link</button></form>
<p class="alt"><a href="/auth/login">Back to sign in</a></p>`,
		field("Email", "email", "email", "")))
}

func (a *BuiltinAuth) pageResetConfirm(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		tok, password := r.FormValue("token"), r.FormValue("password")
		if len(password) < 8 {
			authPage(w, "Set a new password", resetForm(tok, `<p class="err">Password must be at least 8 characters.</p>`))
			return
		}
		g, ok := a.takeGrant("reset", tok)
		if !ok {
			authPage(w, "Link expired", `<p class="err">This reset link is invalid or expired.</p>
<p class="alt"><a href="/auth/reset">Request a new one</a></p>`)
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		a.mu.Lock()
		if u := a.users[g.user]; u != nil {
			u.Pass = string(hash)
			a.save()
		}
		a.mu.Unlock()
		authPage(w, "Password updated", `<p class="msg">Your password is updated.</p>
<p class="alt"><a href="/auth/login">Sign in</a></p>`)
		return
	}
	authPage(w, "Set a new password", resetForm(r.URL.Query().Get("token"), ""))
}

func resetForm(token, msg string) string {
	return fmt.Sprintf(`<form method="post"><input type="hidden" name="token" value=%q>%s%s<button>Set password</button></form>`,
		html.EscapeString(token), field("New password (min 8 chars)", "password", "password", ""), msg)
}

func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// ---- CLI API ----

// apiExchange trades the one-time code from the browser redirect for a
// long-lived device token.
func (a *BuiltinAuth) apiExchange(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code   string `json:"code"`
		Device string `json:"device"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	g, ok := a.takeGrant("code", req.Code)
	if !ok || !g.granted {
		http.Error(w, "invalid or expired code", http.StatusUnauthorized)
		return
	}
	a.finishLogin(w, g.user, req.Device)
}

// apiDeviceStart begins the headless flow: the CLI shows the code, the user
// approves it at /auth/device, the CLI polls.
func (a *BuiltinAuth) apiDeviceStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Device string `json:"device"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	code := randHex(4) // short enough to type
	a.mu.Lock()
	a.pending[code] = pendingGrant{kind: "device", device: req.Device, expires: time.Now().Add(10 * time.Minute)}
	a.mu.Unlock()
	writeJSON(w, map[string]any{
		"code":       code,
		"verify_url": requestBaseURL(r) + "/auth/device?code=" + code,
		"interval":   2,
	})
}

func (a *BuiltinAuth) apiDevicePoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code   string `json:"code"`
		Device string `json:"device"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	g, ok := a.peekGrant("device", req.Code)
	if !ok {
		http.Error(w, "invalid or expired code", http.StatusUnauthorized)
		return
	}
	if !g.granted {
		writeJSON(w, map[string]any{"pending": true})
		return
	}
	a.takeGrant("device", req.Code)
	device := req.Device
	if device == "" {
		device = g.device
	}
	a.finishLogin(w, g.user, device)
}

func (a *BuiltinAuth) finishLogin(w http.ResponseWriter, userID, device string) {
	if device == "" {
		device = "cli"
	}
	tok, err := a.issueToken(userID, device)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.mu.Lock()
	u := a.users[userID]
	a.mu.Unlock()
	if u == nil {
		http.Error(w, "unknown user", http.StatusUnauthorized)
		return
	}
	writeJSON(w, map[string]any{
		"token": tok,
		"user":  User{ID: u.ID, Email: u.Email, Name: u.Name},
	})
}

func (a *BuiltinAuth) apiMe(w http.ResponseWriter, r *http.Request) {
	u, ok := a.Authenticate(r)
	if !ok {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}
	writeJSON(w, u)
}
