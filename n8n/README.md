# Automated review triage

Two n8n workflows and an error handler. Import the JSON in this directory, then
fill in the credentials and the four environment variables below.

Everything here is optional. With `REVIEW_TRIAGE_WEBHOOK_URL` unset, every
verified review goes to the human queue and the site works exactly as it does
now. **Test that by actually running with it empty**, rather than assuming — it
is the property the whole design leans on and the one nobody checks.

## Files

| file | what it is |
| :-- | :-- |
| `review-triage.json` | Webhook → signature check → classifier → decision callback |
| `triage-sweep.json` | Scheduled sweep: retries, timeouts, email flush, ratings |
| `error-alert.json` | Error Trigger → Discord, so a broken workflow is visible |

## Configuration

On the Jupiterp side (Secret Manager, read by the API at boot):

```
REVIEW_TRIAGE_WEBHOOK_URL     = https://<n8n>/webhook/jupiterp-review-triage
REVIEW_TRIAGE_WEBHOOK_SECRET  = <32+ random bytes>   # signs outbound payloads
REVIEW_TRIAGE_CALLBACK_KEY    = <32+ random bytes>   # n8n authenticates back with this
DISCORD_MODERATION_WEBHOOK_URL = https://discord.com/api/webhooks/...
```

On the n8n side, as workflow variables:

| name | value |
| :-- | :-- |
| `JUPITERP_API` | `https://api.jupiterp.com` |
| `TRIAGE_WEBHOOK_SECRET` | same as `REVIEW_TRIAGE_WEBHOOK_SECRET` |
| `TRIAGE_CALLBACK_KEY` | same as `REVIEW_TRIAGE_CALLBACK_KEY` |
| `ADMIN_KEY` | `REVIEW_ADMIN_KEY`, used only by the sweep workflow |

The callback key must differ from the admin key. The API refuses to start if
they match: the entire point of a scoped key is that a compromised n8n reaches
moderation decisions and nothing else.

## The classifier

Gemini Flash, free tier, pinned to an explicit model id. Pinning matters — a
provider silently swapping the model underneath a moderation pipeline is a
change to what gets published that nobody decided to make. The model id is sent
back with every decision and stored, so a decision can be traced to the model
that made it.

**The free tier's terms permit Google to use submitted content to improve their
products, including human review.** That is disclosed in the site's privacy
policy. If you move to a paid tier, nothing changes architecturally, but that
paragraph should be updated because it will no longer be true.

Structured output only: the response is constrained to a JSON schema whose
`decision` field is an enum of exactly `approve|reject|escalate`. The model
never emits a free-form action string that something downstream parses loosely,
and anything unparseable or out of range is treated as `escalate`.

## Rolling it out

Do not enable auto-apply on day one. There is no data yet on whether the
classifier agrees with your judgement, and launch volume is small enough to
moderate by hand.

1. **Shadow.** `REVIEW_TRIAGE_AUTO_REJECT=false`,
   `REVIEW_TRIAGE_AUTO_APPROVE=false`. The workflow runs on every review and
   writes to `moderation_decisions` with `applied = false`; humans decide
   everything. Run for at least four weeks or a hundred reviews.

2. **Compare.** Agreement between the classifier and the humans:

   ```sql
   select ai.decision as ai_said, human.decision as human_said, count(*)
   from moderation_decisions ai
   join moderation_decisions human
     on human.review_id = ai.review_id and human.decided_by = 'human'
   where ai.decided_by = 'ai'
   group by 1, 2
   order by 3 desc;
   ```

   The cell that matters is `ai_said = 'approve'` where `human_said = 'reject'`.
   Anything in it means auto-approve is not ready.

3. **Auto-reject.** Turn on `REVIEW_TRIAGE_AUTO_REJECT` first: a wrong
   rejection annoys one student who can appeal or resubmit.

4. **Auto-approve.** Last, and only once step 2 is clean. A wrong approval
   publishes something defamatory about a person who never opted in.

5. **Ongoing.** Spot-check ~10% of automated decisions forever.
   `moderation_decisions` makes that a query rather than a project.

## Thresholds

Defaults, set conservatively but not so tight that everything escalates:

```
REVIEW_TRIAGE_AUTO_APPROVE_MIN_CONFIDENCE = 0.90   # AND zero categories AND zero prefilter flags
REVIEW_TRIAGE_AUTO_REJECT_MIN_CONFIDENCE  = 0.85
```

Asymmetric on purpose. Tune the approve threshold from shadow data rather than
from taste — it is the one number that should come from evidence.

Note that the API enforces the "zero categories" part itself, so a workflow
change cannot loosen it by accident.

## What the API does regardless of this workflow

These are in Go, not in n8n, because a rule that lives in a prompt is a rule an
injection can argue with:

- Reviews containing links, email addresses, or phone numbers are rejected
  before any model call.
- Suspected prompt injection escalates to a human.
- Anything alleging misconduct about a named person escalates to a human,
  whatever the classifier concludes.
- A decision is only applied if the review is still `pending` or `escalated`,
  so a late retry cannot overturn a human.

## Failure modes

| what happens | result |
| :-- | :-- |
| Webhook URL unset | Everything goes to the human queue |
| n8n unreachable | Review stays `pending`; the sweep escalates it |
| Gemini per-minute 429 | Retry in-workflow, seconds not hours |
| Gemini daily quota | Park: set `next_triage_at` past the reset, leave `pending` |
| Gemini other error | Escalate |
| Malformed model output | Escalate |
| Bad signature | Rejected and logged on both sides |
| n8n never calls back | The sweep escalates after `REVIEW_TRIAGE_TIMEOUT_SEC` |

The sweep is not optional. Without it a silently broken workflow looks exactly
like "nobody submitted any reviews this week", while their authors have been
told they are awaiting moderation.

**Confirm when the free tier's daily counter actually resets.** It is a fixed
clock boundary in a specific timezone, not 24 hours after the first call, so
`next_triage_at` should target that boundary. Aiming at `now() + 24h` lands
before the reset and burns an attempt.

## Scheduling the sweep

`triage-sweep.json` runs hourly and calls `POST /v1/admin/sweep`, which does the
retries, the timeout escalations, the abandoned-submission purge, the email
outbox flush, and the nightly rating recompute.

If you would rather not depend on n8n for this, Cloud Scheduler hitting the same
endpoint works identically — and is the better choice, since it means an n8n
outage cannot also stop the email queue draining.
