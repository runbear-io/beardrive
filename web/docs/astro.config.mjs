// @ts-check
import { defineConfig } from "astro/config";
import starlight from "@astrojs/starlight";
import llmsTxt from "starlight-llms-txt";

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
export default defineConfig({
  site: "https://docs.beardrive.ai",
  // The docs were reorganized around the agent-first path; these URLs were
  // public and indexed. Astro emits meta-refresh pages for static output —
  // real 301s belong in the host config (see README, "Deploying").
  redirects: {
    "/start/install": "/manual/install/",
    "/start/quickstart": "/manual/setup-by-hand/",
    "/guides/connect-an-agent": "/start/setup/",
  },
  integrations: [
    starlight({
      title: "BearDrive",
      description:
        "Google Drive for AI agents. One shared folder your whole team's agents read and write — real files, synced in seconds, with history, provenance, and share links.",
      logo: { src: "./src/assets/bear.svg", alt: "BearDrive" },
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
          // Job-shaped titles, persona named in the description (which is also
          // the search snippet and the llms.txt line). These pages ROUTE — the
          // moment one starts teaching a feature, it links to the guide that
          // owns it instead.
          label: "Use cases",
          items: [
            { label: "Share work across your team's agents", slug: "use-cases/team-artifacts" },
            { label: "Keep a wiki your agents maintain", slug: "use-cases/team-wiki" },
            { label: "Turn a personal brain into a company brain", slug: "use-cases/company-brain" },
            { label: "Run a personal wiki, publish part of it", slug: "use-cases/personal-wiki" },
            { label: "Carry one context across agents and devices", slug: "use-cases/multi-device" },
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
            { label: "Skills and hooks in detail", slug: "manual/skills-and-hooks" },
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
          ],
        },
        {
          label: "Concepts",
          items: [{ label: "How sync works", slug: "concepts/how-it-works" }],
        },
      ],
    }),
  ],
});
