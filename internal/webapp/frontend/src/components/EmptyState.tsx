import { GuideCode, INSTALL_DOC } from "./ConnectGuide";

// Onboarding: a signed-in account with no projects shouldn't hit a blank
// sidebar. One path in — paste the canonical install prompt into a coding
// agent — with the by-hand route a docs link away.

export function EmptyState() {
  return (
    <div className="onboard">
      <h1>Welcome to BearDrive</h1>
      <p>You're signed in, but you're not part of any project yet.</p>
      <div className="ob-card">
        <h3>Connect a new drive to your project</h3>
        <p>
          Paste into your coding agent — Claude Code, Cowork, Codex, Gemini CLI, Hermes — in the
          folder where you want the files. It creates the project and starts syncing:
        </p>
        <GuideCode
          code={
            "Follow " +
            INSTALL_DOC +
            "\nto set up a new BearDrive project on " +
            window.location.origin +
            ". Ask me which folder to sync."
          }
        />
        <p className="ob-alt">
          <a href="https://docs.beardrive.ai/manual/setup-by-hand/" target="_blank" rel="noreferrer">
            Or start a project manually →
          </a>
        </p>
      </div>
    </div>
  );
}
