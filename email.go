package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
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
	params.Set("next_attempt_at", "lte."+time.Now().UTC().Format(time.RFC3339))
	params.Set("order", "next_attempt_at.asc")
	params.Set("limit", fmt.Sprintf("%d", limit))

	var due []outboxRow
	if err := e.write.Select("email_outbox", params, &due); err != nil {
		return 0, err
	}

	sent := 0
	for _, row := range due {
		if err := e.deliver(row); err != nil {
			log.Printf("email %d (%s) failed: %v", row.ID, row.Template, err)
			continue
		}
		sent++
	}
	return sent, nil
}

func (e *EmailSender) deliver(row outboxRow) error {
	if e.cfg.BrevoAPIKey == "" {
		return e.reschedule(row, "BREVO_API_KEY not configured")
	}
	if row.Recipient == nil || *row.Recipient == "" {
		return e.abandon(row, "no recipient")
	}

	subject, html := renderTemplate(e.cfg, row)

	body := map[string]any{
		"sender":      map[string]string{"email": e.cfg.EmailFrom, "name": e.cfg.EmailFromName},
		"to":          []map[string]string{{"email": *row.Recipient}},
		"subject":     subject,
		"htmlContent": html,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, brevoEndpoint, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("api-key", e.cfg.BrevoAPIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	res, err := e.http.Do(req)
	if err != nil {
		return e.reschedule(row, err.Error())
	}
	defer res.Body.Close()

	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		return e.markSent(row)

	case res.StatusCode == http.StatusTooManyRequests || res.StatusCode == 402:
		// Rate limited, or the plan's daily allowance is exhausted. Not a
		// failure: the message waits. This is the case the whole outbox exists
		// for, so it is logged distinctly rather than as a generic error.
		log.Printf("email %d deferred: provider cap or rate limit (HTTP %d)", row.ID, res.StatusCode)
		return e.reschedule(row, fmt.Sprintf("provider cap or rate limit: HTTP %d", res.StatusCode))

	case res.StatusCode >= 400 && res.StatusCode < 500:
		// A malformed request or a rejected address will not become valid on
		// a retry, so retrying only burns allowance.
		return e.abandon(row, fmt.Sprintf("permanent HTTP %d", res.StatusCode))

	default:
		return e.reschedule(row, fmt.Sprintf("HTTP %d", res.StatusCode))
	}
}

func (e *EmailSender) markSent(row outboxRow) error {
	// The recipient address is dropped once it is no longer needed. Holding
	// raw addresses in a queue table would quietly undo the decision not to
	// store them on `reviews`.
	return e.write.Update("email_outbox", eq("id", fmt.Sprintf("%d", row.ID)), map[string]any{
		"status":    "sent",
		"sent_at":   time.Now().UTC().Format(time.RFC3339),
		"recipient": nil,
		"payload":   map[string]any{},
	}, nil)
}

func (e *EmailSender) reschedule(row outboxRow, reason string) error {
	attempts := row.Attempts + 1
	if attempts >= len(emailBackoff) {
		return e.abandon(row, "retries exhausted: "+reason)
	}
	next := time.Now().UTC().Add(emailBackoff[attempts])
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
// Plain, unbranded HTML on purpose: mail that looks like marketing is filtered
// like marketing, and a verification link in a spam folder is indistinguishable
// from a broken feature.
func renderTemplate(cfg *Config, row outboxRow) (string, string) {
	str := func(key string) string {
		if v, ok := row.Payload[key].(string); ok {
			return v
		}
		return ""
	}

	switch row.Template {
	case "verify", "resend_verify":
		link := cfg.SiteBaseURL + "/review/verify?token=" + url.QueryEscape(str("token"))
		return "Confirm your Jupiterp review", fmt.Sprintf(
			`<p>Someone (hopefully you) wrote a review of %s on Jupiterp.</p>`+
				`<p><a href="%s">Confirm it here</a>. The link expires in 48 hours.</p>`+
				`<p>Your review will not appear until it has been read by a moderator.</p>`+
				`<p>If this wasn't you, ignore this email and nothing will be published. `+
				`We store your address only as an irreversible hash, to check you're at UMD `+
				`and to stop duplicate reviews. It is never shown to anyone, including the `+
				`professor.</p>`,
			htmlEscape(str("instructor_name")), link)

	case "manage_key":
		return "Your Jupiterp review management key", fmt.Sprintf(
			`<p>Thanks for confirming your review of %s. It is now awaiting moderation.</p>`+
				`<p>Keep this key if you want to edit or withdraw it later:</p>`+
				`<p><code>%s</code></p>`+
				`<p>We cannot recover it for you, because we have no way to link it back to `+
				`you.</p>`,
			htmlEscape(str("instructor_name")), htmlEscape(str("manage_key")))

	case "rejected":
		return "Your Jupiterp review was not published", fmt.Sprintf(
			`<p>Your review of %s was not published.</p>`+
				`<p>Reason given: %s</p>`+
				`<p>If you think that was a mistake, reply to this email and a person will `+
				`look at it again. You can also submit a revised review.</p>`,
			htmlEscape(str("instructor_name")), htmlEscape(str("reason")))
	}

	return "Jupiterp", "<p>This message was sent in error.</p>"
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
