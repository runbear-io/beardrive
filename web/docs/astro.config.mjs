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
          label: "Start here",
          items: [
            { label: "What is BearDrive?", slug: "" },
            { label: "Install", slug: "start/install" },
            { label: "Quickstart", slug: "start/quickstart" },
          ],
        },
        {
          // Guides are about working with agents — that's what the product is
          // for. Command-by-command CLI detail belongs in Reference.
          label: "Working with agents",
          items: [
            { label: "Connect an agent", slug: "guides/connect-an-agent" },
            { label: "Shared agent memory", slug: "guides/shared-agent-memory" },
            { label: "Artifacts and links", slug: "guides/agent-artifacts" },
            { label: "What agents read", slug: "guides/what-agents-read" },
            { label: "Scoping the folder", slug: "guides/scoping" },
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
