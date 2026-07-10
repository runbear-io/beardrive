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
- **GCS**: pay per GB stored + egress. Cheap for text; consider a lifecycle
  policy later.

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
