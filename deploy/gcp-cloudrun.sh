#!/usr/bin/env bash
# Deploy the BearDrive hub to Google Cloud Run (single instance) with Cloud SQL
# Postgres for metadata and a GCS bucket for blobs/journals.
#
# The hub is single-process for now, so max-instances=1 (in-memory caches
# assume one writer). Run from the repo root: bash example/deploy/gcp-cloudrun.sh
set -euo pipefail

# gcloud needs Python >= 3.10 (3.9 crashes `gcloud builds`); point it at a
# newer interpreter if your system default is old:
#   export CLOUDSDK_PYTHON=$(command -v python3.11 || command -v python3.12)

# ---- fill these in -----------------------------------------------------------
PROJECT_ID="${PROJECT_ID:?set PROJECT_ID}"          # dedicated GCP project id
BILLING_ACCOUNT="${BILLING_ACCOUNT:-}"              # e.g. 0X0X0X-0X0X0X-0X0X0X (only needed if creating the project)
REGION="${REGION:-us-central1}"
ADMIN_EMAIL="${ADMIN_EMAIL:?set ADMIN_EMAIL}"        # first hub admin
ADMIN_DOMAIN="${ADMIN_DOMAIN:?set ADMIN_DOMAIN}"     # signup limited to this email domain for bootstrap, e.g. runbear.io
BRAND="${BRAND:-BearDrive}"
# ------------------------------------------------------------------------------
BUCKET="${BUCKET:-${PROJECT_ID}-bdrive}"
SQL_INSTANCE="${SQL_INSTANCE:-bdrive-pg}"
SQL_TIER="${SQL_TIER:-db-f1-micro}"
DB_NAME="bdrive"; DB_USER="bdrive"
SERVICE="${SERVICE:-bdrive}"
RUN_SA="bdrive-run@${PROJECT_ID}.iam.gserviceaccount.com"
CONN_NAME="${PROJECT_ID}:${REGION}:${SQL_INSTANCE}"

echo "== project =="
gcloud projects describe "$PROJECT_ID" >/dev/null 2>&1 || {
  echo "creating project $PROJECT_ID"; gcloud projects create "$PROJECT_ID"
  [ -n "$BILLING_ACCOUNT" ] && gcloud billing projects link "$PROJECT_ID" --billing-account "$BILLING_ACCOUNT"
}
gcloud config set project "$PROJECT_ID"

echo "== enable APIs =="
gcloud services enable run.googleapis.com sqladmin.googleapis.com storage.googleapis.com \
  secretmanager.googleapis.com artifactregistry.googleapis.com cloudbuild.googleapis.com

echo "== GCS bucket for blobs/journals =="
gcloud storage buckets describe "gs://$BUCKET" >/dev/null 2>&1 || \
  gcloud storage buckets create "gs://$BUCKET" --location "$REGION" --uniform-bucket-level-access

echo "== Cloud SQL Postgres (this takes several minutes) =="
gcloud sql instances describe "$SQL_INSTANCE" >/dev/null 2>&1 || \
  gcloud sql instances create "$SQL_INSTANCE" --database-version POSTGRES_16 \
    --edition ENTERPRISE --tier "$SQL_TIER" --region "$REGION" \
    --storage-size 10 --storage-auto-increase
gcloud sql databases describe "$DB_NAME" --instance "$SQL_INSTANCE" >/dev/null 2>&1 || \
  gcloud sql databases create "$DB_NAME" --instance "$SQL_INSTANCE"
DB_PASS="$(gcloud secrets versions access latest --secret bdrive-db-pass 2>/dev/null || true)"
if [ -z "$DB_PASS" ]; then
  DB_PASS="$(openssl rand -base64 24 | tr -d '/+=')"
  printf '%s' "$DB_PASS" | gcloud secrets create bdrive-db-pass --data-file=- 2>/dev/null || \
    printf '%s' "$DB_PASS" | gcloud secrets versions add bdrive-db-pass --data-file=-
fi
gcloud sql users create "$DB_USER" --instance "$SQL_INSTANCE" --password "$DB_PASS" 2>/dev/null || \
  gcloud sql users set-password "$DB_USER" --instance "$SQL_INSTANCE" --password "$DB_PASS"

echo "== runtime service account + IAM =="
gcloud iam service-accounts describe "$RUN_SA" >/dev/null 2>&1 || \
  gcloud iam service-accounts create bdrive-run --display-name "BearDrive Cloud Run"
gcloud storage buckets add-iam-policy-binding "gs://$BUCKET" \
  --member "serviceAccount:$RUN_SA" --role roles/storage.objectAdmin
gcloud projects add-iam-policy-binding "$PROJECT_ID" \
  --member "serviceAccount:$RUN_SA" --role roles/cloudsql.client >/dev/null

echo "== hub config secret (contains the DB DSN) =="
# Bootstrap posture: domain-gated self-signup so the first admin can create
# their account and become owner; tighten to invite-only afterwards.
CONFIG="$(cat <<JSON
{
  "remote": "gs://$BUCKET/hub",
  "addr": ":8080",
  "upload": true,
  "database": {
    "driver": "postgres",
    "dsn": "postgres://$DB_USER:$DB_PASS@/$DB_NAME?host=/cloudsql/$CONN_NAME&sslmode=disable"
  },
  "auth": {
    "allow_signup": true,
    "allowed_domains": ["$ADMIN_DOMAIN"],
    "admins": ["$ADMIN_EMAIL"],
    "brand": "$BRAND"
  }
}
JSON
)"
printf '%s' "$CONFIG" | gcloud secrets create bdrive-config --data-file=- 2>/dev/null || \
  printf '%s' "$CONFIG" | gcloud secrets versions add bdrive-config --data-file=-
gcloud secrets add-iam-policy-binding bdrive-config \
  --member "serviceAccount:$RUN_SA" --role roles/secretmanager.secretAccessor >/dev/null

echo "== build + deploy to Cloud Run (single instance) =="
gcloud run deploy "$SERVICE" \
  --source . \
  --region "$REGION" \
  --service-account "$RUN_SA" \
  --add-cloudsql-instances "$CONN_NAME" \
  --update-secrets "/config/config.json=bdrive-config:latest" \
  --args "web,-c,/config/config.json" \
  --min-instances 1 --max-instances 1 \
  --cpu 1 --memory 512Mi \
  --allow-unauthenticated

echo
echo "Deployed. URL:"
gcloud run services describe "$SERVICE" --region "$REGION" --format 'value(status.url)'
echo "Next: open the URL, Sign up as $ADMIN_EMAIL (domain-gated), then tighten"
echo "auth to invite-only by editing the bdrive-config secret + redeploying."
