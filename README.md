# static-files

Minimal single-bucket S3 explorer: **list**, **upload**, **copy public link**. One bucket, nothing else.

No auth of its own — it runs behind the oauth2-proxy sidecar.

## Why this exists

Replaces Filestash, which was overkill for "browse one bucket + upload + get link"
and had reverse-proxy quirks.

## Config (env)

| Var | Default | Notes |
|-----|---------|-------|
| `BUCKET` | — (required) | bucket name, e.g. `example-static` |
| `AWS_REGION` | `us-east-1` | |
| `PUBLIC_BASE_URL` | `https://<bucket>.s3.<region>.amazonaws.com` | override if fronted by CloudFront |
| `LISTEN_ADDR` | `:8334` | |
| `ROOT_PREFIX` | `` (whole bucket) | confine the UI to a sub-path; users can't escape it |
| `MAX_UPLOAD_MB` | `512` | per-request upload cap |

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
