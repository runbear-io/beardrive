package webapp

import (
	"net/http"
	"strings"
)

// Authentication is opt-in (`auth` in the server config) and sits behind the
// AuthProvider interface. The open-source server ships exactly one
// implementation, BuiltinAuth (email + password accounts in a file-backed
// registry, server-owned /auth/* pages). A managed deployment can swap in a
// different provider (e.g. PropelAuth-backed) without touching the CLI or
// the API: the CLI learns the login page from /api/config and the callback
// flow is provider-agnostic.

// User is an authenticated account as the rest of the server sees it.
type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
	Admin bool   `json:"admin,omitempty"` // hub admin (approve users, govern shares)
}

// AuthProvider is the seam between the server and an identity system.
type AuthProvider interface {
	// CLILoginPath is the page `bdrive login` opens in a browser. The CLI
	// appends ?redirect=http://127.0.0.1:<port>/callback&state=<nonce>.
	CLILoginPath() string
	// Authenticate resolves the request's Bearer token or session cookie.
	Authenticate(r *http.Request) (User, bool)
	// Register mounts the provider's own pages and endpoints (/auth/*,
	// /api/auth/*) on the server mux.
	Register(mux *http.ServeMux)
	// Accounts lists every account the provider knows, oldest first. Startup
	// tasks (the org migration) need it, and both implementations already had
	// it — declaring it here stops callers reaching for a concrete type.
	Accounts() []User
	// UseDeviceBinder hands the provider the hub's device-binding hook. The
	// provider MUST call it at every point it mints a CLI token, before the
	// token is handed over, and MUST refuse the login if it returns an error.
	//
	// This is on the interface — a breaking change for an out-of-tree provider,
	// on purpose — because it is a PRECONDITION of a gate the hub enforces for
	// every provider. store.go's ownJournal refuses a journal write unless the
	// device id is bound to the caller's account, and DeviceRegistry.Bind is the
	// only thing that binds. That hook used to be a field on BuiltinAuth, wired
	// behind `if a, ok := s.Auth.(*BuiltinAuth); ok` — so on a hub running any
	// other provider nothing ever called it, no device was ever bound, and EVERY
	// journal push 403'd forever while login, permissions and blob uploads all
	// looked healthy. A gate whose enabler is wired to one concrete type is a
	// gate that is enforced further than it can be satisfied; declaring it here
	// is what makes a provider that ignores it fail to compile instead.
	//
	// The hub cannot do this for the provider. Binding must be reachable only
	// from a completed authentication — a device token that could reach a bind
	// would let a stolen credential squat a teammate's id — and Authenticate
	// reports only WHO a request is, never which credential class it presented.
	// Only the provider knows it is minting, so only the provider can bind.
	UseDeviceBinder(bind DeviceBinder)
}

// DeviceBinder records that the device identified by the request's
// X-Bdrive-Device header belongs to email. It returns an error when the id is
// already another account's, which a provider must surface as a failed login
// rather than a token that cannot push.
type DeviceBinder func(email string, r *http.Request) error

// AccountApprover is the optional half of account administration: signup
// policy and the approval queue behind /api/admin/*. A provider whose accounts
// live in an external identity system does not implement it, and those routes
// say so (503) rather than pretending the queue is empty.
type AccountApprover interface {
	PendingUsers() []User
	Approve(id string) error
	Deny(id string) error
	SetPolicy(requireVerification, requireApproval bool) error
	// Policy reports the signup gates as configured. The provider assembles
	// it, so the hub never reaches into provider fields to render the page.
	Policy() SignupPolicy
}

// Brander is the optional hub-name half: a provider that renders its own
// sign-in pages knows what to call this hub.
type Brander interface{ Branding() string }

// SignupPolicy is what /api/admin/policy reports: which gates are on, and
// which of them are server-config owned (read-only to a browser session, so
// that no one can widen access by clicking).
type SignupPolicy struct {
	RequireVerification bool     `json:"require_verification"`
	RequireApproval     bool     `json:"require_approval"`
	AllowSignup         bool     `json:"allow_signup"`
	AllowedDomains      []string `json:"allowed_domains"` // read-only
	Admins              []string `json:"admins"`          // read-only
	Mailer              bool     `json:"mailer"`          // SMTP configured?
}

// authGate wraps the API with authentication when a provider is configured.
// The static frontend and the provider's own surface stay reachable so a
// browser can get to the login page; everything else under /api/ needs a
// valid identity.
func (s *Server) authGate(next http.Handler) http.Handler {
	if s.Auth == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		open := strings.HasPrefix(p, "/auth/") ||
			strings.HasPrefix(p, "/api/auth/") ||
			p == "/api/config" ||
			!strings.HasPrefix(p, "/api/") // static frontend; its API calls are gated
		if !open {
			if _, ok := s.Auth.Authenticate(r); !ok {
				http.Error(w, "authentication required (bdrive login, or sign in at /auth/login)", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// requestUser returns the authenticated user, or a zero User when auth is
// disabled (everything then runs as an anonymous single user).
func (s *Server) requestUser(r *http.Request) User {
	if s.Auth == nil {
		return User{}
	}
	u, _ := s.Auth.Authenticate(r)
	return u
}
