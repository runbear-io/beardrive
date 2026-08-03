# Deploy BearDrive to Google Cloud (Cloud Run)

Single-instance hub on **Cloud Run**, metadata in **Cloud SQL Postgres**,
blobs/journals in a **GCS bucket**. Matches Phase 0 of the managed PRD.

> The current build is single-process: Cloud Run is pinned to
> `max-instances=1` because the in-memory caches assume one writer. Do **not**
> raise it until the "stateless app" work (PRD §5.1) lands.

## Architecture

```
      browser / bdrive CLI
              │  https
        ┌─────▼─────┐   metadata   ┌──────────────┐
        │ Cloud Run │◀────────────▶│  Cloud SQL   │  (accounts, orgs,
        │  bdrive   │  unix socket │  Postgres    │   projects, invites…)
        │ (1 inst.) │              └──────────────┘
        └─────┬─────┘
              │ ADC (runtime SA)
        ┌─────▼─────┐
        │    GCS    │  blobs/ + journal/  (file content + sync log)
        └───────────┘
```

## Prerequisites

- `gcloud` installed and logged in (`gcloud auth login`).
- A **billing account** id (`gcloud billing accounts list`) if the script
  creates the project.
- Values: `PROJECT_ID`, `ADMIN_EMAIL`, `ADMIN_DOMAIN` (the rest have defaults).

## Run it

From the repo root:

```sh
PROJECT_ID=beardrive-prod \
BILLING_ACCOUNT=0X0X0X-0X0X0X-0X0X0X \
ADMIN_EMAIL=you@runbear.io \
ADMIN_DOMAIN=runbear.io \
REGION=us-central1 \
bash example/deploy/gcp-cloudrun.sh
```

The script: creates/links the project → enables APIs → creates the GCS bucket
and Cloud SQL instance → generates a DB password (stored in Secret Manager) →
writes the hub config to a secret → builds the image from the repo `Dockerfile`
via Cloud Build → deploys Cloud Run with the Cloud SQL socket, the config
secret mounted at `/config/config.json`, and a dedicated runtime service
account granted GCS + Cloud SQL access. It prints the service URL.

## First-run: bootstrap the admin, then lock down

The hub ships **invite-only by default**, but a brand-new hub has no accounts,
so the deploy config temporarily allows **domain-gated self-signup**
(`allowed_domains: [ADMIN_DOMAIN]`). Steps:

1. Open the printed URL → **Sign up** as `ADMIN_EMAIL` (must be on
   `ADMIN_DOMAIN`). The account is active immediately and is a hub admin.
2. Create your org/projects and invite teammates from the UI.
3. **Tighten to invite-only:** edit the config secret to `"allow_signup": false`
   and redeploy:
   ```sh
   gcloud secrets versions access latest --secret bdrive-config > /tmp/c.json
   #   …set "allow_signup": false …
   gcloud secrets versions add bdrive-config --data-file=/tmp/c.json
   gcloud run services update bdrive --region "$REGION"   # picks up latest secret
   ```

## Rough cost

- **Cloud Run**: scales to ~zero when idle (min-instances=1 keeps one warm;
  set `--min-instances 0` to save more, at the cost of cold-start journal
  folding on first hit). ~$5–15/mo warm.
- **Cloud SQL** `db-f1-micro`: ~$8–15/mo (smallest shared-core tier).
- **GCS**: pay per GB stored + egress. Cheap for text. `gcp-cloudrun.sh`
  installs a Nearline-at-30-days lifecycle rule by default (`LIFECYCLE=0`
  skips it) — see below for why it stops there.

## Storage tiering

Every version of every file is retained forever, so stored bytes only ever
grow while per-seat revenue stays flat. Aging objects down is the lever.

Prices below are us-central1 regional, verified 2026-08-03 — re-check before
relying on them, GCP moves them.

| Class | Storage $/GB/mo | Retrieval $/GB | Min duration |
|---|---|---|---|
| Standard | 0.020 | — | none |
| Nearline | 0.010 | 0.01 | 30 days |
| Coldline | 0.004 | 0.02 | 90 days |
| Archive | 0.0012 | 0.05 | 365 days |

Archive really is ~6% of Standard, and GCS serves every class at the same
millisecond latency — there is no restore job to wait on. The catch is the
retrieval fee, so the break-even is entirely about **how often a given object
is read**. Writing `r` for reads per GB per month:

- Nearline beats Standard while `r < 1.0/mo`
- Coldline beats Nearline while `r < 0.6/mo`
- Archive beats Coldline while `r < 0.09/mo` (about once a year)

**This is why the script stops at Nearline.** The tempting assumption is that
old blobs are cold because old versions are rarely opened. That is not true
here: a device syncing a project for the first time downloads a blob for
*every put op in every peer journal* — the entire history, not just the
current file tree (`internal/syncer` `pull`). Measured on a 10-version file
whose working tree is 1 KB, a fresh device pulls 10 KB.

So the read rate on old blobs tracks **how often anyone adds a device**, not
how often anyone opens an old version. A team that adds or replaces roughly
one device a month drives `r ≈ 1`, which makes Coldline a wash and Archive a
straight bill increase.

Two consequences worth acting on, in this order:

1. **The real lever is not the lifecycle policy.** Making a first sync fetch
   only current-state blobs (history stays available on demand through the
   existing `/blob?sha=` route) cuts onboarding egress from "all history" to
   "the working tree" *and* makes old blobs genuinely cold — which is what
   makes Coldline and Archive safe to turn on afterwards. Until then the
   ladder is priced against a read pattern the sync engine does not have.
2. **Egress scales with devices × total history**, not with change volume,
   and every byte is relayed: blob reads have no presigned path (only
   `remote.PutSigner` exists — uploads can go direct to storage, downloads
   cannot), so they stream GCS → Cloud Run → client. Same-region GCS→Cloud Run
   transfer is free, so this is one egress charge, not two — but it does
   occupy the single `max-instances=1` container for the whole transfer.

The lifecycle rule is applied bucket-wide rather than to `blobs/` alone
because journals live under the same per-project prefixes and a GCS
lifecycle `matchesPrefix` cannot express `*/blobs/`. That is safe: a peer
journal is re-fetched only when the listing shows it grew, and one that grew
was just rewritten, so it is Standard again.

## Notes / limits (single-instance build)

- `max-instances=1` is required. Metadata correctness depends on one writer.
- `BDRIVE_HOME=/tmp` is ephemeral on Cloud Run → the server's own device id
  regenerates on cold start (cosmetic in history). Mount a volume later to
  persist it.
- Large **downloads/sync** stream through Cloud Run (bounded by the request
  timeout, up to 60 min). Uploads go direct to storage when the backend can
  presign. See PRD §5.2 for offloading these at scale.
- Put a custom domain on the service via `gcloud run domain-mappings` (gives
  managed TLS).
