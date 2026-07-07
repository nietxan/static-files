# static-files

Minimal single-bucket S3 explorer: **list**, **upload**, **copy public link**. One bucket, nothing else.

No auth of its own — it runs behind the oauth2-proxy sidecar.

## Why this exists

Replaces Filestash, which was overkill for "browse one bucket + upload + get link"
and had reverse-proxy quirks.

## Config (env)

- **`BUCKET`** (required) — bucket name, e.g. `example-static`.
- **`AWS_REGION`** — defaults to `us-east-1`.
- **`PUBLIC_BASE_URL`** — base for public links. Defaults to `https://<bucket>.s3.<region>.amazonaws.com`; override if fronted by CloudFront.
- **`LISTEN_ADDR`** — listen address, defaults to `:8334`.
- **`ROOT_PREFIX`** — confine the UI to a sub-path (users can't escape it). Defaults to empty = whole bucket.
- **`MAX_UPLOAD_MB`** — per-request upload cap, defaults to `512`.

## API

- `GET /api/list?prefix=foo/` → `{prefix, folders[], objects[{key,name,size,modified,url}]}`
- `POST /api/upload?prefix=foo/` (multipart, field `files`) → `{uploaded[]}`
- `GET /healthz` → `ok`
- `GET /` → the UI

## Run locally

```bash
BUCKET=example-static AWS_REGION=us-east-1 go run .
# open http://localhost:8334  (uses your local AWS creds)
```
