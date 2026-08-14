# Installing the triage workflows on your n8n server

Three files to import, four variables to set, one credential to create. Fifteen
minutes if the secrets are already generated.

Nothing here is required for Jupiterp to work. Leave `REVIEW_TRIAGE_WEBHOOK_URL`
unset and every verified review goes to the human moderation queue instead —
which is where you should start regardless, since the classifier has to run in
shadow mode before it is allowed to act.

---

## 1. Generate the secrets

Three, all distinct:

```sh
# Signs Jupiterp's outbound webhook payloads; n8n verifies it.
openssl rand -hex 32

# n8n presents this when calling back with a decision. Scoped to one route.
openssl rand -hex 32

# The moderation queue key, if you have not made one yet.
openssl rand -hex 32
```

The callback key **must differ** from the admin key. The API refuses to start if
they match: the point of a scoped key is that a compromised n8n reaches
moderation decisions and nothing else.

## 2. Create the Discord webhook

Server Settings → Integrations → Webhooks → New Webhook. Point it at a private
moderation channel and copy the URL.

Treat the URL as a secret — anyone holding it can post into that channel.

## 3. Import the workflows

**Through the UI**, which is easiest for three files:

Workflows → the `…` menu → *Import from File*. Do all three:

| file | what it does |
| :-- | :-- |
| `review-triage.json` | The webhook that classifies a review |
| `triage-sweep.json` | Hourly maintenance |
| `error-alert.json` | Alerts Discord when a workflow fails |

**Or through the CLI**, if you have shell access to the n8n host:

```sh
# Docker
docker cp review-triage.json n8n:/tmp/
docker exec -u node n8n n8n import:workflow --input=/tmp/review-triage.json

# npm install
n8n import:workflow --separate --input=./api/n8n/
```

CLI imports do not activate workflows — do that in the UI afterwards, or the
webhook stays unregistered and every call 404s.

## 4. Set the variables

Settings → Variables (n8n 1.x; on Community edition use environment variables
on the host instead and swap `$vars.X` for `$env.X` in the workflow nodes).

| name | value |
| :-- | :-- |
| `JUPITERP_API` | `https://api.jupiterp.com` |
| `TRIAGE_WEBHOOK_SECRET` | the first secret from step 1 |
| `TRIAGE_CALLBACK_KEY` | the second |
| `ADMIN_KEY` | your moderation key |
| `DISCORD_WEBHOOK_URL` | from step 2 |

## 5. Add the Gemini credential

Credentials → New → *Google Gemini(PaLM) API*. Paste an API key from
[aistudio.google.com](https://aistudio.google.com/apikey). Open the **Classify**
node in `review-triage.json` and select it.

The model is pinned to `models/gemini-2.0-flash-001`. Leave it pinned. A
provider silently swapping the model underneath a moderation pipeline is a
change to what gets published that nobody decided to make, and the model id is
recorded with every decision so a decision can be traced back to what made it.

**The free tier's terms let Google use submitted content to improve their
products, including human review.** That is disclosed on Jupiterp's privacy
policy page. If you move to a paid tier, update that page — it will no longer
be true.

## 6. Activate and copy the webhook URL

Open `review-triage.json`, toggle **Active**, then open the Webhook node and
copy the *Production* URL. It looks like:

```
https://<your-n8n-host>/webhook/jupiterp-review-triage
```

Do the same for `triage-sweep.json` and `error-alert.json`.

## 7. Point Jupiterp at it

Set these on the Cloud Run service (Secret Manager, not plain env vars):

```
REVIEW_TRIAGE_WEBHOOK_URL      = https://<your-n8n-host>/webhook/jupiterp-review-triage
REVIEW_TRIAGE_WEBHOOK_SECRET   = <first secret>
REVIEW_TRIAGE_CALLBACK_KEY     = <second secret>
DISCORD_MODERATION_WEBHOOK_URL = <discord url>
```

Leave `REVIEW_TRIAGE_AUTO_REJECT` and `REVIEW_TRIAGE_AUTO_APPROVE` at `false`.
That is shadow mode: the classifier records an opinion, a human still decides.

Redeploy. The API validates all of this at boot and refuses to start if the
callback key matches the admin key, if the webhook URL is set without a signing
secret, or if the sweep timeout is shorter than the retry window.

## 8. Check it works

```sh
# Should fail the signature check. If it returns 200, the HMAC verification
# is not running and anyone can feed the workflow fabricated reviews.
curl -X POST https://<your-n8n-host>/webhook/jupiterp-review-triage \
  -H 'Content-Type: application/json' \
  -d '{"review_id":"test","body":"hello"}'
```

Then submit a real review through the site, confirm it, and check:

- the n8n execution list shows a run;
- the moderation queue at `/admin/reviews` shows the review with the
  classifier's opinion beside it, marked *recorded only*;
- Discord got nothing (it only fires on escalation).

## 9. Schedule the sweep

`triage-sweep.json` runs hourly. **Consider using Cloud Scheduler instead** —
it hits the same endpoint, and it means an n8n outage does not also stop the
email queue draining or the ratings recomputing:

```sh
gcloud scheduler jobs create http jupiterp-sweep \
  --schedule="0 * * * *" \
  --uri="https://api.jupiterp.com/v1/admin/sweep" \
  --http-method=POST \
  --headers="Authorization=Bearer $REVIEW_ADMIN_KEY" \
  --location=us-east4
```

If you use both, disable one. Running the sweep twice is harmless — it is
idempotent — but it doubles the load for nothing.

---

## Turning automation on

Do not skip shadow mode. Full rollout sequence, the agreement query, and the
threshold reasoning are in `README.md` in this directory. The short version:
run four weeks or a hundred reviews with both gates off, check how often the
classifier said *approve* where a human said *reject*, then enable auto-reject
first and auto-approve last.

## If something breaks

| symptom | cause |
| :-- | :-- |
| Webhook 404s | Workflow imported but not activated |
| Every call rejected as bad signature | `TRIAGE_WEBHOOK_SECRET` differs between the two sides |
| Callback 401s | `TRIAGE_CALLBACK_KEY` mismatch, or it equals the admin key |
| Callback 409s | A human already decided that review — working as intended |
| Reviews stuck `pending` | The sweep is not running; check step 9 |
| API will not boot | Read the log. Config validation names the exact variable |

Reviews are never lost by any of these. They stay `pending`, the sweep
escalates them after 30 hours, and a human picks them up.
