// @ts-check
import { defineConfig } from "astro/config";
import starlight from "@astrojs/starlight";
import sitemap from "@astrojs/sitemap";
import llmsTxt from "starlight-llms-txt";
import { execFileSync } from "node:child_process";

// docs.beardrive.ai — the public product documentation.
//
// This is a standalone static site, deliberately NOT embedded into the Go
// binary the way the hub frontend (internal/webapp/static) and the cloud
// landing page (cloud/internal/landing/dist) are. Docs change far more often
// than the binary does, and a Pagefind search index has no business shipping
// inside every self-hoster's install. It deploys on its own, on push.
//
// It lives in the OSS repo because that's what it documents: the CLI, the sync
// model, self-hosting. "Edit this page" resolves to something an outside
// contributor can actually open a PR against.

// `lastmod` for the sitemap, from the commit that last touched each page.
//
// The tempting shortcut — stamp every URL with the build time — is worse than
// emitting nothing: a sitemap that reports the whole site changed on every
// deploy teaches Google to disregard its lastmod entirely, and freshness is
// most of what a sitemap is for on a docs site.
//
// Which is also why the shallow check exists. A CI checkout is depth-1 by
// default, and there `git log` attributes every file to the one commit it has
// — the same uniform lie in a different costume. No history, no lastmod.
// (A host that wants these dates must clone with full history:
// actions/checkout needs `fetch-depth: 0`.)
const lastmodBySource = (() => {
  const git = (...args) =>
    execFileSync("git", args, { cwd: import.meta.dirname, encoding: "utf8" }).trim();
  try {
    if (git("rev-parse", "--is-shallow-repository") === "true") return new Map();
    // One `git log` for every page rather than one per page. Newest commit
    // first, so the first time a path appears is its last modification.
    const log = git("log", "--format=%cI", "--name-only", "--relative", "--", "src/content/docs");
    const map = new Map();
    let when = "";
    for (const line of log.split("\n")) {
      if (!line) continue;
      else if (/^\d{4}-\d\d-\d\dT/.test(line)) when = line;
      else if (!map.has(line)) map.set(line, when);
    }
    return map;
  } catch {
    return new Map(); // built from a tarball, or no git installed — not fatal
  }
})();

/** `/reference/cli/` -> the date on `src/content/docs/reference/cli.md`. */
function lastmodFor(url) {
  const slug = new URL(url).pathname.replace(/^\/|\/$/g, "") || "index";
  for (const ext of [".md", ".mdx"]) {
    const at = lastmodBySource.get(`src/content/docs/${slug}${ext}`);
    if (at) return at;
  }
}

export default defineConfig({
  site: "https://docs.beardrive.ai",
  // The docs were reorganized around the agent-first path; these URLs were
  // public and indexed. Astro emits meta-refresh pages for static output —
  // real 301s belong in the host config (see README, "Deploying").
  redirects: {
    "/start/install": "/manual/install/",
    "/start/quickstart": "/manual/setup-by-hand/",
    "/guides/connect-an-agent": "/start/setup/",
    "/manual/skills-and-hooks": "/manual/hooks/",
    // Use cases moved to the marketing site, which already published the same
    // six pages at the same slugs. They were live and indexed here for a
    // month, so every one of them keeps a redirect — off-site, which is
    // exactly what a page that is no longer documentation should do.
    "/use-cases/team-artifacts": "https://beardrive.ai/use-cases/team-artifacts",
    "/use-cases/team-wiki": "https://beardrive.ai/use-cases/team-wiki",
    "/use-cases/business-context": "https://beardrive.ai/use-cases/business-context",
    "/use-cases/company-brain": "https://beardrive.ai/use-cases/company-brain",
    "/use-cases/personal-wiki": "https://beardrive.ai/use-cases/personal-wiki",
    "/use-cases/multi-device": "https://beardrive.ai/use-cases/multi-device",
    // The one with no counterpart over there yet — land on the index rather
    // than a 404 someone else's deploy has to fix.
    "/use-cases/shared-skills": "https://beardrive.ai/use-cases/",
  },
  integrations: [
    // Starlight adds @astrojs/sitemap itself, but only when the config hasn't
    // already — declaring it here replaces that default rather than doubling
    // it, which is the supported way to reach these options. (Starlight's own
    // version only sets `i18n`, and this site is single-language.)
    sitemap({ serialize: (item) => ({ ...item, lastmod: lastmodFor(item.url) }) }),
    starlight({
      title: "BearDrive",
      description:
        "Google Drive for AI agents. One shared folder your whole team's agents read and write — real files, synced in seconds, with history, provenance, and share links.",
      logo: { src: "./src/assets/bear.svg", alt: "BearDrive" },
      // The logo is the way back to beardrive.ai. Starlight always points it
      // at the docs root, so the link lives in a component override.
      components: { SiteTitle: "./src/components/SiteTitle.astro" },
      customCss: ["./src/styles/tokens.gen.css", "./src/styles/custom.css"],
      social: [
        {
          icon: "github",
          label: "GitHub",
          href: "https://github.com/runbear-io/beardrive",
        },
      ],
      editLink: {
        baseUrl:
          "https://github.com/runbear-io/beardrive/edit/main/web/docs/",
      },
      // Docs are the top AI-citation surface for a dev tool, so ship the
      // machine-readable index too: /llms.txt and /llms-full.txt.
      //
      // Convention puts llms.txt at the ROOT domain, not a docs subdomain.
      // beardrive.ai/llms.txt should redirect (or proxy) here — that lives in
      // the cloud landing, and is the one cross-repo coordination point this
      // split introduces.
      plugins: [llmsTxt()],
      sidebar: [
        {
          // The reading order IS the recommended path, and the recommended path
          // is agent-first: nobody should meet `brew install` before they meet
          // /beardrive:install. Everything CLI lives under "Manual setup",
          // one click away and never on the critical path.
          label: "Start here",
          items: [
            { label: "What is BearDrive?", slug: "" },
            { label: "Set up with your agent", slug: "start/setup" },
            { label: "Your first hour", slug: "start/first-hour" },
          ],
        },
        {
          // Guides are about working with agents — that's what the product is
          // for. Command-by-command CLI detail belongs in Reference.
          label: "Working with agents",
          items: [
            { label: "Shared agent memory", slug: "guides/shared-agent-memory" },
            { label: "Artifacts and links", slug: "guides/agent-artifacts" },
            { label: "What agents read", slug: "guides/what-agents-read" },
            { label: "Write files over HTTP", slug: "guides/http-api" },
            { label: "Scoping the folder", slug: "guides/scoping" },
          ],
        },
        {
          // For people who would rather type it, and for machines with no agent
          // on them. Same destination, more steps.
          label: "Manual setup (optional)",
          items: [
            { label: "Install the CLI", slug: "manual/install" },
            { label: "Set up by hand", slug: "manual/setup-by-hand" },
            { label: "Hooks in detail", slug: "manual/hooks" },
          ],
        },
        {
          label: "Self-hosting",
          items: [
            { label: "Run a hub", slug: "self-hosting/run-a-hub" },
            { label: "Authentication", slug: "self-hosting/authentication" },
            { label: "Database", slug: "self-hosting/database" },
          ],
        },
        {
          label: "Reference",
          items: [
            { label: "CLI", slug: "reference/cli" },
            { label: "Project files", slug: "reference/project-files" },
            { label: "Hub config", slug: "reference/hub-config" },
            { label: "Migrate between hubs", slug: "reference/migration" },
          ],
        },
        {
          label: "Concepts",
          items: [
            { label: "How sync works", slug: "concepts/how-it-works" },
            { label: "Project permissions", slug: "concepts/permissions" },
          ],
        },
        // Off-site, and last on purpose: the sidebar order is the recommended
        // path, so nothing that leaves the docs belongs above the docs.
        {
          label: "More",
          items: [
            { label: "Use cases", link: "https://beardrive.ai/use-cases/" },
            { label: "Blog", link: "https://beardrive.ai/blog/" },
            { label: "GitHub", link: "https://github.com/runbear-io/beardrive" },
          ],
        },
      ],
    }),
  ],
});
