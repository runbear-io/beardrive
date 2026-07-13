import { defineConfig } from "@playwright/test";

// The suite runs against the committed Go e2e harness (e2e_serve_test.go):
// a deterministic seeded hub on :8993, freshly wiped on every start. It
// exercises the BUILT frontend served by the Go binary — run `npm run
// build` before `npm run e2e`, or test against stale assets.
export default defineConfig({
  testDir: ".",
  timeout: 30_000,
  retries: 0,
  workers: 1, // specs share one hub with mutable state (uploads, shares)
  use: {
    baseURL: "http://localhost:8993",
  },
  webServer: {
    command:
      "cd ../../../.. && BDRIVE_E2E_SERVE=1 go test -count=1 -timeout 3h -run TestE2EServe ./internal/webapp",
    url: "http://localhost:8993/",
    reuseExistingServer: true,
    timeout: 60_000,
  },
});
