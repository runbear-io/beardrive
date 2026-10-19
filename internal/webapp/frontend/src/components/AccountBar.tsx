const REPO_URL = "https://github.com/runbear-io/beardrive";

// The GitHub mark, inline: lucide dropped its brand icons in v1, and one
// glyph is not worth a second icon dependency. Filled (not stroked) like
// every other rendering of this logo, so it sits outside the `.ico` sizing
// rules that the lucide `<Icon>` components share.
function GithubMark() {
  return (
    <svg viewBox="0 0 16 16" className="gh-mark" fill="currentColor" aria-hidden="true">
      <path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27s1.36.09 2 .27c1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0 0 16 8c0-4.42-3.58-8-8-8z" />
    </svg>
  );
}

/* The star ask is a plain link, never a banner: dim, one click, and
   identical on every render, so it reads as social proof rather than as a
   request. */
function StarOnGitHub() {
  return (
    <a className="gh-star" href={REPO_URL} target="_blank" rel="noreferrer">
      <GithubMark />
      <span>Star on GitHub</span>
      <span className="ext" aria-hidden="true">↗</span>
      <span className="sr-only"> (opens in a new tab)</span>
    </a>
  );
}

// SignedOutBar is the desktop app's sidebar footer while no device sign-in
// exists: same GitHub row, and the account slot becomes the sign-in action
// (which opens the hub's login page in the system browser via the sidecar).
export function SignedOutBar({ onSignIn }: { onSignIn: () => void }) {
  return (
    <footer id="accountbar">
      <StarOnGitHub />
      <button id="account-btn" onClick={onSignIn} aria-label="Sign in">
        <span className="avatar" style={{ background: "var(--hover)" }} aria-hidden="true">?</span>
        <span className="acct">
          <b>Sign in…</b>
          <small>connect to your hub</small>
        </span>
      </button>
    </footer>
  );
}
