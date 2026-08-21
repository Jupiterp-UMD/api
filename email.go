package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Transactional email, via Brevo.
//
// Two properties matter more than the sending itself:
//
//  1. The submit response never waits on the mail provider. A reviewer whose
//     email is slow still gets a 202; a submit that 500s because a third party
//     was having a bad minute loses the review entirely, and the reviewer has
//     no way to know whether to try again.
//
//  2. Hitting the provider's daily cap defers a send, it does not skip
//     verification. Everything queues in `email_outbox` first and is sent from
//     there, so a cap, an outage, or a bad API key delays delivery rather than
//     letting unverified reviews through. The alternative -- accepting
//     submissions unverified once the cap is hit -- would make exhausting the
//     cap a way to bypass the check that backs the per-email dedupe and the
//     UMD-affiliation claim.

const brevoEndpoint = "https://api.brevo.com/v3/smtp/email"

// Retry schedule for a queued message. Long tail because the failure this is
// most likely to be waiting out is a daily cap, which resets on a clock rather
// than after a delay.
var emailBackoff = []time.Duration{
	1 * time.Minute,
	10 * time.Minute,
	1 * time.Hour,
	6 * time.Hour,
	25 * time.Hour,
}

type EmailSender struct {
	cfg   *Config
	write *WriteClient
	http  *http.Client
}

func NewEmailSender(cfg *Config, write *WriteClient) *EmailSender {
	return &EmailSender{
		cfg:   cfg,
		write: write,
		http:  &http.Client{Timeout: 15 * time.Second},
	}
}

type outboxRow struct {
	ID        int64          `json:"id"`
	ReviewID  *string        `json:"review_id"`
	Recipient *string        `json:"recipient"`
	Template  string         `json:"template"`
	Payload   map[string]any `json:"payload"`
	Attempts  int            `json:"attempts"`
}

// Queue stores a message for delivery. Never sends inline.
func (e *EmailSender) Queue(reviewID, recipient, template string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	row := map[string]any{
		"recipient": recipient,
		"template":  template,
		"payload":   payload,
	}
	if reviewID != "" {
		row["review_id"] = reviewID
	}
	return e.write.Insert("email_outbox", []any{row}, nil)
}

// Flush delivers due messages. Called after a submit (best effort, in a
// goroutine) and by the scheduled sweep.
//
// Returns how many were sent. A daily-cap response is not an error here: it
// leaves the message queued with a later `next_attempt_at`, which is the whole
// point of the outbox.
func (e *EmailSender) Flush(limit int) (int, error) {
	params := url.Values{}
	params.Set("select", "*")
	params.Set("status", "eq.queued")
	// RFC3339Nano, not RFC3339.
	//
	// RFC3339 truncates to whole seconds, so a row queued at 18:18:06.573 was
	// compared against `lte 18:18:06` and excluded by its own flush. Submission
	// queues the verification email and immediately flushes, so the row it just
	// wrote was the one row it could not see: every submission sent the
	// *previous* user's email and left its own behind, waiting for the hourly
	// sweep. From the reviewer's side that is a confirmation link that simply
	// never arrives.
	params.Set("next_attempt_at", "lte."+time.Now().UTC().Format(time.RFC3339Nano))
	params.Set("order", "next_attempt_at.asc")
	params.Set("limit", fmt.Sprintf("%d", limit))

	var due []outboxRow
	if err := e.write.Select("email_outbox", params, &due); err != nil {
		return 0, err
	}
	return e.deliverAll(due), nil
}

// FlushFor delivers the queued messages for one review and nothing else.
//
// Flush takes the oldest `limit` rows across the whole outbox, so a caller that
// has just queued a message and needs it gone before it does something else --
// notifyRejection, which purges the address immediately afterwards -- cannot
// rely on it: with a backlog deeper than the limit, the row it just wrote is
// not in the batch. Selecting by review is the only version of that which is
// actually true.
//
// Deliberately ignores `next_attempt_at`: the caller is asking for this
// message now, and a row queued microseconds ago is due by construction.
func (e *EmailSender) FlushFor(reviewID string) (int, error) {
	params := url.Values{}
	params.Set("select", "*")
	params.Set("status", "eq.queued")
	params.Set("review_id", "eq."+reviewID)
	params.Set("order", "id.asc")

	var due []outboxRow
	if err := e.write.Select("email_outbox", params, &due); err != nil {
		return 0, err
	}
	return e.deliverAll(due), nil
}

// deliverAll returns how many messages actually left, not how many rows it
// looked at. A deferred message is still queued and its caller must not treat
// it as delivered.
func (e *EmailSender) deliverAll(due []outboxRow) int {
	sent := 0
	for _, row := range due {
		outcome, err := e.deliver(row)
		if err != nil {
			log.Printf("email %d (%s) failed: %v", row.ID, row.Template, err)
			continue
		}
		if outcome == deliverySent {
			sent++
		}
	}
	return sent
}

// deliveryOutcome distinguishes "gone" from "still queued".
//
// `deliver` used to answer with a bare error, and returned nil for a message it
// had merely rescheduled -- so a provider cap, which is the case the whole
// outbox exists to handle, counted as a successful send. The sweep's
// `emails_sent` figure was really "rows considered", and `notifyRejection`,
// which purges the reviewer's address once the mail is away, would have purged
// it on a deferral and abandoned the message on the next pass.
type deliveryOutcome int

const (
	// Accepted by the provider. This is the only outcome that means the
	// message has left.
	deliverySent deliveryOutcome = iota
	// Still queued, with a later next_attempt_at. Not a failure.
	deliveryDeferred
	// Will never be sent, and the row says why.
	deliveryAbandoned
)

func (e *EmailSender) deliver(row outboxRow) (deliveryOutcome, error) {
	if e.cfg.BrevoAPIKey == "" {
		return deliveryDeferred, e.reschedule(row, "BREVO_API_KEY not configured")
	}
	if row.Recipient == nil || *row.Recipient == "" {
		return deliveryAbandoned, e.abandon(row, "no recipient")
	}

	subject, html, text := renderTemplate(e.cfg, row)

	body := map[string]any{
		"sender":      map[string]string{"email": e.cfg.EmailFrom, "name": e.cfg.EmailFromName},
		"to":          []map[string]string{{"email": *row.Recipient}},
		"subject":     subject,
		"htmlContent": html,
		// Sent alongside the HTML, not instead of it. HTML-only mail scores
		// worse with spam filters than the same message with a text part, and
		// this is a transactional link people need to receive.
		"textContent": text,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return deliveryAbandoned, err
	}

	req, err := http.NewRequest(http.MethodPost, brevoEndpoint, bytes.NewReader(encoded))
	if err != nil {
		return deliveryAbandoned, err
	}
	req.Header.Set("api-key", e.cfg.BrevoAPIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	res, err := e.http.Do(req)
	if err != nil {
		return deliveryDeferred, e.reschedule(row, err.Error())
	}
	defer res.Body.Close()

	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		return deliverySent, e.markSent(row)

	case res.StatusCode == http.StatusTooManyRequests || res.StatusCode == 402:
		// Rate limited, or the plan's daily allowance is exhausted. Not a
		// failure: the message waits. This is the case the whole outbox exists
		// for, so it is logged distinctly rather than as a generic error.
		log.Printf("email %d deferred: provider cap or rate limit (HTTP %d)", row.ID, res.StatusCode)
		return deliveryDeferred, e.reschedule(row, fmt.Sprintf("provider cap or rate limit: HTTP %d", res.StatusCode))

	case res.StatusCode >= 400 && res.StatusCode < 500:
		// A malformed request or a rejected address will not become valid on
		// a retry, so retrying only burns allowance.
		return deliveryAbandoned, e.abandon(row, fmt.Sprintf("permanent HTTP %d", res.StatusCode))

	default:
		return deliveryDeferred, e.reschedule(row, fmt.Sprintf("HTTP %d", res.StatusCode))
	}
}

func (e *EmailSender) markSent(row outboxRow) error {
	// The payload is dropped: it carries the verification token, which is a
	// bearer credential and has no reason to outlive the send.
	//
	// The recipient is NOT dropped here, which is a deliberate reversal.
	// Nulling it on send made two features unreachable rather than private:
	// a review can only be verified, and can only be rejected, *after* its
	// verification mail has gone out -- so by the time either of those needed
	// an address, this function had already destroyed the only copy. The
	// manage key was returned as "" on every verification and no rejection
	// mail was ever sent, both silently.
	//
	// The address is instead purged by PurgeContact when the review reaches a
	// state that will never generate mail again. That keeps the retention
	// bounded by the review's own lifecycle, which is what the privacy policy
	// actually describes, rather than by an implementation detail of the queue.
	return e.write.Update("email_outbox", eq("id", fmt.Sprintf("%d", row.ID)), map[string]any{
		"status":  "sent",
		"sent_at": time.Now().UTC().Format(time.RFC3339),
		"payload": map[string]any{},
	}, nil)
}

// RecipientFor recovers the address a review's mail was sent to.
//
// Returns "" once PurgeContact has run, which is the normal state for any
// review that has reached a terminal status. Callers must treat that as
// "no longer contactable" rather than as an error.
func (e *EmailSender) RecipientFor(reviewID string) string {
	params := url.Values{}
	params.Set("select", "recipient")
	params.Set("review_id", "eq."+reviewID)
	params.Set("recipient", "not.is.null")
	params.Set("order", "id.asc")
	params.Set("limit", "1")

	var rows []outboxRow
	if err := e.write.Select("email_outbox", params, &rows); err != nil || len(rows) == 0 {
		return ""
	}
	if rows[0].Recipient == nil {
		return ""
	}
	return *rows[0].Recipient
}

// PurgeContact drops every stored address for a review.
//
// Called when the review reaches a state that generates no further mail:
// approved, rejected-and-notified, or withdrawn. This is the retention
// boundary -- after it, the service holds no way to contact the reviewer and
// no way to link the review back to a person.
func (e *EmailSender) PurgeContact(reviewID string) {
	if err := e.write.Update("email_outbox", eq("review_id", reviewID), map[string]any{
		"recipient": nil,
		"payload":   map[string]any{},
	}, nil); err != nil {
		log.Printf("purging contact for review %s failed: %v", reviewID, err)
	}
}

// PurgeSettledContacts drops addresses for reviews that have reached a
// terminal status and have no mail still waiting to go out.
//
// The per-decision purge cannot be the only one. A rejection notice is now
// purged only once it has actually been delivered, so a review whose mail was
// deferred by a provider cap keeps its address until the sweep sends it -- and
// without this pass, nothing would ever come back for it. Retention has to
// terminate on the review's own lifecycle rather than on whether one code path
// happened to run.
func (e *EmailSender) PurgeSettledContacts(limit int) (int, error) {
	held := url.Values{}
	held.Set("select", "review_id")
	held.Set("recipient", "not.is.null")
	held.Set("review_id", "not.is.null")
	held.Set("limit", strconv.Itoa(limit))

	var holding []struct {
		ReviewID *string `json:"review_id"`
	}
	if err := e.write.Select("email_outbox", held, &holding); err != nil {
		return 0, err
	}

	ids := map[string]struct{}{}
	for _, row := range holding {
		if row.ReviewID != nil && *row.ReviewID != "" {
			ids[*row.ReviewID] = struct{}{}
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	idList := make([]string, 0, len(ids))
	for id := range ids {
		idList = append(idList, id)
	}
	inList := "in.(" + strings.Join(idList, ",") + ")"

	// Of those, the ones that have finished.
	settled := url.Values{}
	settled.Set("select", "id")
	settled.Set("id", inList)
	settled.Set("status", "in.(approved,rejected,withdrawn)")

	var terminal []struct {
		ID string `json:"id"`
	}
	if err := e.write.Select("reviews", settled, &terminal); err != nil {
		return 0, err
	}
	if len(terminal) == 0 {
		return 0, nil
	}

	// Minus any whose mail has not gone out yet. Purging those would abandon
	// the message, which is the bug this whole pass exists to avoid repeating.
	pendingMail := url.Values{}
	pendingMail.Set("select", "review_id")
	pendingMail.Set("review_id", inList)
	pendingMail.Set("status", "eq.queued")

	var queued []struct {
		ReviewID *string `json:"review_id"`
	}
	if err := e.write.Select("email_outbox", pendingMail, &queued); err != nil {
		return 0, err
	}
	waiting := map[string]struct{}{}
	for _, row := range queued {
		if row.ReviewID != nil {
			waiting[*row.ReviewID] = struct{}{}
		}
	}

	purged := 0
	for _, review := range terminal {
		if _, stillWaiting := waiting[review.ID]; stillWaiting {
			continue
		}
		e.PurgeContact(review.ID)
		purged++
	}
	return purged, nil
}

func (e *EmailSender) reschedule(row outboxRow, reason string) error {
	attempts := row.Attempts + 1
	if attempts > len(emailBackoff) {
		return e.abandon(row, "retries exhausted: "+reason)
	}
	// `attempts-1`, so the first retry uses the first entry.
	//
	// This indexed by `attempts`, which skipped entry zero entirely: the
	// declared schedule read 1m/10m/1h/6h/25h and the delivered one was
	// 10m/1h/6h/25h. The one-minute step -- the only one that helps with a
	// blip rather than an outage -- never ran.
	next := time.Now().UTC().Add(emailBackoff[attempts-1])
	return e.write.Update("email_outbox", eq("id", fmt.Sprintf("%d", row.ID)), map[string]any{
		"attempts":        attempts,
		"next_attempt_at": next.Format(time.RFC3339),
		"last_error":      reason,
	}, nil)
}

func (e *EmailSender) abandon(row outboxRow, reason string) error {
	log.Printf("email %d abandoned: %s", row.ID, reason)
	return e.write.Update("email_outbox", eq("id", fmt.Sprintf("%d", row.ID)), map[string]any{
		"status":     "abandoned",
		"last_error": reason,
		"recipient":  nil,
		"payload":    map[string]any{},
	}, nil)
}

// renderTemplate builds the subject and body for one queued message.
//
// Brand tokens, mirrored from site/src/themes.css.
//
// Duplicated rather than imported because an email carries no stylesheet: every
// rule has to travel inside the message. If the site's palette changes, these
// are the values to change with it.
const (
	emailFont     = "'Cabin', Arial, Helvetica, sans-serif"
	colorOrange   = "#f5692e"
	colorBg       = "#ffffff"
	colorBgAlt    = "#ebebeb"
	colorText     = "#000000"
	colorTextSub  = "#667085"
	colorBorder   = "#f1f1f1"
	colorDarkBg   = "#151922"
	colorDarkAlt  = "#141721"
	colorDarkText = "#d9dfea"
	colorDarkBord = "#252e3e"
)

// emailShell wraps body content in the Jupiterp frame.
//
// Branded, but deliberately not marketing-shaped, which is the tension the
// previous plain version was avoiding: mail that looks like a campaign gets
// filtered like one, and a verification link in a spam folder is
// indistinguishable from a broken feature. So the things that actually drive
// that classification are avoided rather than decorated around --
//
//   - no images of any kind, so nothing is blocked by default, nothing leaks a
//     tracking pixel, and the message renders identically before and after the
//     "display images" prompt. The wordmark is text in the brand colour.
//   - one link, the one the reader asked for. No social icons, no footer menu.
//   - a plain-text alternative alongside the HTML (see deliver), because
//     HTML-only mail is one of the cheapest spam signals to trip.
//
// Tables and inline styles because email clients are not browsers: Outlook
// renders through Word, and flexbox, grid, and most positioning do not survive.
// Dark mode rides on a <style> block, which Apple Mail and iOS honour and the
// rest ignore -- so the inline light styles have to stand on their own.
func emailShell(preheader, heading, body string) string {
	return `<!doctype html><html><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<meta name="color-scheme" content="light dark">` +
		`<style>` +
		`@media (prefers-color-scheme:dark){` +
		`.jp-bg{background:` + colorDarkBg + ` !important}` +
		`.jp-card{background:` + colorDarkAlt + ` !important;border-color:` + colorDarkBord + ` !important}` +
		`.jp-text{color:` + colorDarkText + ` !important}` +
		`.jp-code{background:` + colorDarkBg + ` !important;border-color:` + colorDarkBord + ` !important;color:` + colorDarkText + ` !important}` +
		`}` +
		`</style></head>` +
		`<body class="jp-bg" style="margin:0;padding:0;background:` + colorBgAlt + `;">` +
		// Preheader: the grey line the inbox shows next to the subject. Hidden
		// in the body itself, then padded so the client does not pull the
		// following markup into the preview.
		`<div style="display:none;max-height:0;overflow:hidden;opacity:0;">` + preheader +
		`&#8203;&#8203;&#8203;&#8203;&#8203;&#8203;&#8203;&#8203;&#8203;&#8203;</div>` +
		`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" class="jp-bg" style="background:` + colorBgAlt + `;">` +
		`<tr><td align="center" style="padding:32px 16px;">` +
		`<table role="presentation" width="560" cellpadding="0" cellspacing="0" border="0" style="width:560px;max-width:100%;">` +
		// Wordmark.
		`<tr><td style="padding:0 4px 16px 4px;font-family:` + emailFont + `;font-size:20px;font-weight:700;letter-spacing:-0.01em;color:` + colorOrange + `;">` +
		`Jupiterp</td></tr>` +
		// Card.
		`<tr><td class="jp-card" style="background:` + colorBg + `;border:1px solid ` + colorBorder + `;border-radius:12px;padding:32px;">` +
		`<h1 class="jp-text" style="margin:0 0 16px 0;font-family:` + emailFont + `;font-size:22px;line-height:1.3;font-weight:700;color:` + colorText + `;">` +
		heading + `</h1>` + body +
		`</td></tr>` +
		// Footer.
		`<tr><td style="padding:20px 4px 0 4px;font-family:` + emailFont + `;font-size:12px;line-height:1.6;color:` + colorTextSub + `;">` +
		`Jupiterp &middot; course planning and professor reviews for UMD` +
		`</td></tr>` +
		`</table></td></tr></table></body></html>`
}

// para is body copy inside the card.
func para(html string) string {
	return `<p class="jp-text" style="margin:0 0 14px 0;font-family:` + emailFont +
		`;font-size:15px;line-height:1.6;color:` + colorText + `;">` + html + `</p>`
}

// note is the smaller, secondary copy: the privacy explanation and the
// "if this wasn't you" line. Secondary colour is identical in both themes, so
// it needs no dark override.
func note(html string) string {
	return `<p style="margin:16px 0 0 0;font-family:` + emailFont +
		`;font-size:13px;line-height:1.6;color:` + colorTextSub + `;">` + html + `</p>`
}

// button is a table-based call to action.
//
// An <a> with padding is dropped by Outlook, which is exactly the client where
// a missed verification link is least likely to be reported and most likely to
// be read as "the site is broken". The bare URL underneath covers whatever
// still fails to render it.
func button(label, href string) string {
	return `<table role="presentation" cellpadding="0" cellspacing="0" border="0" style="margin:24px 0;">` +
		`<tr><td align="center" bgcolor="` + colorOrange + `" style="border-radius:8px;">` +
		`<a href="` + href + `" style="display:inline-block;padding:13px 28px;font-family:` + emailFont +
		`;font-size:15px;font-weight:700;color:#ffffff;text-decoration:none;border-radius:8px;">` +
		label + `</a></td></tr></table>`
}

// renderTemplate returns the subject, the HTML body, and the plain-text
// alternative. The text part is not a fallback nobody reads: sending HTML with
// no text alternative is one of the cheapest ways to be scored as bulk mail.
func renderTemplate(cfg *Config, row outboxRow) (string, string, string) {
	str := func(key string) string {
		if v, ok := row.Payload[key].(string); ok {
			return v
		}
		return ""
	}

	switch row.Template {
	case "verify", "resend_verify":
		link := cfg.SiteBaseURL + "/review/verify?token=" + url.QueryEscape(str("token"))
		name := htmlEscape(str("instructor_name"))

		html := emailShell(
			"Confirm your review and it will go to a moderator.",
			"Confirm your review",
			para("Someone (hopefully you) wrote a review of <strong>"+name+"</strong> on Jupiterp.")+
				button("Confirm my review", link)+
				para(`The link expires in 48 hours. Your review will not appear until it has been read by a moderator.`)+
				note(`If the button does not work, paste this into your browser:<br>`+
					`<span style="word-break:break-all;color:`+colorTextSub+`;">`+htmlEscape(link)+`</span>`)+
				note(`If this wasn't you, ignore this email and nothing will be published. `+
					`We store your address only as an irreversible hash, to check you're at UMD `+
					`and to stop duplicate reviews. It is never shown to anyone, including the professor.`))

		text := "Someone (hopefully you) wrote a review of " + str("instructor_name") + " on Jupiterp.\n\n" +
			"Confirm it here (the link expires in 48 hours):\n" + link + "\n\n" +
			"Your review will not appear until it has been read by a moderator.\n\n" +
			"If this wasn't you, ignore this email and nothing will be published. " +
			"We store your address only as an irreversible hash, to check you're at UMD " +
			"and to stop duplicate reviews. It is never shown to anyone, including the professor.\n"

		return "Confirm your Jupiterp review", html, text

	case "manage_key":
		// "withdraw", not "edit or withdraw".
		//
		// Editing does not exist -- there is no route for it and the feature was
		// deliberately removed -- so the old copy promised a capability the site
		// has never had. Withdrawal does exist, and now has a page, which this
		// links to: a key with nowhere to use it is the same broken promise in a
		// different shape.
		name := htmlEscape(str("instructor_name"))
		key := htmlEscape(str("manage_key"))
		withdrawLink := cfg.SiteBaseURL + "/review/withdraw"

		html := emailShell(
			"Keep this key. It is the only way to withdraw your review later.",
			"Your review is awaiting moderation",
			para("Thanks for confirming your review of <strong>"+name+"</strong>.")+
				para("Keep this key. It is the only way to withdraw your review later:")+
				`<div class="jp-code" style="margin:0 0 14px 0;padding:14px 16px;background:`+colorBgAlt+
				`;border:1px solid `+colorBorder+`;border-radius:8px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;`+
				`font-size:14px;line-height:1.5;color:`+colorText+`;word-break:break-all;">`+key+`</div>`+
				para(`To withdraw it, paste the key at <a href="`+withdrawLink+`" style="color:`+colorOrange+
					`;">`+htmlEscape(withdrawLink)+`</a>. You will be shown the review before anything is removed.`)+
				note("We cannot recover this key for you, because we have no way to link it back to you."))

		text := "Thanks for confirming your review of " + str("instructor_name") + ". It is now awaiting moderation.\n\n" +
			"Keep this key. It is the only way to withdraw your review later:\n\n" +
			"    " + str("manage_key") + "\n\n" +
			"To withdraw it, paste the key at:\n" + withdrawLink + "\n" +
			"You will be shown the review before anything is removed.\n\n" +
			"We cannot recover this key for you, because we have no way to link it back to you.\n"

		return "Your Jupiterp review management key", html, text

	case "rejected":
		name := htmlEscape(str("instructor_name"))
		reason := htmlEscape(str("reason"))

		html := emailShell(
			"Your review was not published.",
			"Your review was not published",
			para("Your review of <strong>"+name+"</strong> was not published.")+
				`<div class="jp-code" style="margin:0 0 14px 0;padding:14px 16px;background:`+colorBgAlt+
				`;border-left:3px solid `+colorOrange+`;border-radius:6px;font-family:`+emailFont+
				`;font-size:14px;line-height:1.6;color:`+colorText+`;">`+reason+`</div>`+
				para("If you think that was a mistake, reply to this email and a person will "+
					"look at it again. You can also submit a revised review."))

		text := "Your review of " + str("instructor_name") + " was not published.\n\n" +
			"Reason given: " + str("reason") + "\n\n" +
			"If you think that was a mistake, reply to this email and a person will look at it again. " +
			"You can also submit a revised review.\n"

		return "Your Jupiterp review was not published", html, text
	}

	return "Jupiterp",
		emailShell("This message was sent in error.", "Sent in error",
			para("This message was sent in error.")),
		"This message was sent in error.\n"
}

// htmlEscape escapes the few characters that matter in an email body.
//
// Instructor names and rejection reasons are the only interpolated values, and
// a rejection reason is written by a moderator, but escaping both is cheaper
// than reasoning about which one is trusted.
func htmlEscape(value string) string {
	replacer := map[rune]string{'&': "&amp;", '<': "&lt;", '>': "&gt;", '"': "&quot;", '\'': "&#39;"}
	var out bytes.Buffer
	for _, r := range value {
		if replacement, ok := replacer[r]; ok {
			out.WriteString(replacement)
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}
