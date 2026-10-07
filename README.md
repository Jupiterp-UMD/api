# Jupiterp API

Welcome to the Jupiterp API.

## Deploy

To deploy this API on GCP:

```
PROJECT_ID=(insert GCP project ID here)
RUNTIME_SA="run-sa@${PROJECT_ID}.iam.gserviceaccount.com"
gcloud run deploy go-api \
  --source=. \
  --region="us-east4" \
  --service-account="${RUNTIME_SA}" \
  --cpu-throttling \
  --min-instances=0 \
  --update-secrets="DATABASE_URL=DATABASE_URL:latest,DATABASE_KEY=DATABASE_KEY:latest"
```

`--cpu-throttling` selects request-based billing: the service is billed only
while it is handling a request, not for the idle time an instance stays warm.
Pass it explicitly. `gcloud run deploy` keeps any setting a command leaves out,
so a service previously deployed with `--no-cpu-throttling` stays on
instance-based billing until a deploy turns it back.

Nothing in the service runs after its response is written, which is what makes
throttling safe. Keep it that way: a goroutine started after the response may
be suspended indefinitely and lost.

## Cloudflare

`api.jupiterp.com` is meant to sit behind Cloudflare's proxy, so that read
responses -- which already carry `Cache-Control: public` -- are served from
Cloudflare's cache instead of waking Cloud Run and querying Supabase.

1. **Edge secret first.** Generate one (`openssl rand -hex 32`), store it in
   Secret Manager as `EDGE_PROXY_SECRET`, and deploy with
   `--update-secrets=...,EDGE_PROXY_SECRET=EDGE_PROXY_SECRET:latest`.

   Do this *before* turning on the proxy. Without it, every request appears to
   come from a Cloudflare edge server, and the per-IP rate limits on review
   submission, manage keys, and reports are shared by everyone routed through
   that edge. See `TrustEdgeProxy` in `security.go`.

2. **Transform Rule** (Rules → Transform Rules → Modify Request Header), on
   `http.host eq "api.jupiterp.com"`: *Set static* `X-Jupiterp-Edge-Secret` to
   the secret from step 1.

3. **SSL/TLS.** Cloud Run's domain mapping holds a Google-managed certificate
   that Google may not be able to renew once the domain resolves to Cloudflare.
   Use a Configuration Rule on `http.host eq "api.jupiterp.com"` setting SSL to
   **Full** (not Full (strict)), so the edge keeps connecting if that
   certificate lapses. If the domain mapping itself ever stops answering, the
   fallback is a small Worker on `api.jupiterp.com/*` that forwards to the
   service's `*.run.app` URL.

4. **Cache Rule** (Caching → Cache Rules). Cloudflare does not cache JSON by
   default, so mark the read surface eligible:

   ```
   (http.host eq "api.jupiterp.com"
     and not starts_with(http.request.uri.path, "/v1/reviews")
     and not starts_with(http.request.uri.path, "/v1/admin"))
   ```

   *Eligible for cache*, Edge TTL **use cache-control header if present**,
   Browser TTL **respect origin**. The reviews and admin routes are excluded
   because they answer with a per-origin CORS policy and, for admin, per-caller
   data; nothing there should be shared from a cache.

5. **DNS.** Proxy (orange-cloud) the `api` CNAME to `ghs.googlehosted.com`.

To confirm: a second request for the same URL should come back with
`cf-cache-status: HIT`.

```
curl -sI 'https://api.jupiterp.com/v1/deptList' | grep -i cf-cache-status
```
