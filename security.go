package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

/* ================================ hashing =============================== */

// hashEmail produces the stored identity for a reviewer.
//
// Peppered because a bare SHA-256 of an email address is not anonymous: a
// university's address space is small and highly guessable ("firstlast@umd.edu"),
// so an unpeppered digest can be reversed by enumeration in minutes. The pepper
// lives in Secret Manager rather than in the database, so a database
// disclosure alone does not enable that.
//
// The address is lowercased and trimmed first so that the same person
// submitting as "Student@umd.edu" and "student@umd.edu " is deduplicated.
func hashEmail(email, pepper string) string {
	normalized := strings.ToLower(strings.TrimSpace(email))
	sum := sha256.Sum256([]byte(normalized + pepper))
	return hex.EncodeToString(sum[:])
}

// hashToken hashes a bearer token for storage.
//
// No pepper: these are 256-bit random values, so there is no dictionary to
// defend against, and the lookup has to work from the token alone.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// hashOpaque hashes abuse-forensics values (IP, user agent) with the pepper.
func hashOpaque(value, pepper string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value + pepper))
	return hex.EncodeToString(sum[:])
}

// newToken mints a 256-bit URL-safe token.
func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// constantTimeEqual compares two secrets without leaking their contents
// through timing. Used for every key comparison on /v1.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

/* ============================== middleware =============================== */

// AdminAuth guards the full moderation surface.
//
// Sets two things on the context: `actor`, which is "human" or "ai" and drives
// the shadow-mode gates, and `moderator`, which names who it was. With a single
// shared key those were the same fact and `moderator` was always "human"; a
// named key makes the audit trail able to answer "who approved this".
func AdminAuth(cfg *Config) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		token := bearerToken(ctx)
		if name, ok := moderatorFor(cfg, token); ok {
			ctx.Set("actor", "human")
			ctx.Set("moderator", name)
			ctx.Next()
			return
		}
		ctx.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
	}
}

// moderatorFor resolves a bearer token to the name recorded against its
// decisions. Every comparison is constant time, and every configured key is
// checked even after a match, so the time taken does not reveal which key
// matched or how many are configured.
func moderatorFor(cfg *Config, token string) (string, bool) {
	name := ""
	found := false

	if cfg.AdminKey != "" && constantTimeEqual(token, cfg.AdminKey) {
		name, found = "human", true
	}
	for moderator, key := range cfg.ModeratorKeys {
		if key != "" && constantTimeEqual(token, key) {
			name, found = moderator, true
		}
	}
	return name, found
}

// ModerationAuth accepts either the admin key or the scoped triage callback
// key, and records which one was used.
//
// The scoped key authorises exactly one operation on one route. If n8n is
// compromised, the blast radius is moderation decisions rather than the whole
// admin surface -- and because the decision is recorded with `decided_by`, a
// compromise is visible in the audit trail rather than indistinguishable from
// a human moderator's work.
func ModerationAuth(cfg *Config) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		token := bearerToken(ctx)
		if name, ok := moderatorFor(cfg, token); ok {
			ctx.Set("actor", "human")
			ctx.Set("moderator", name)
			ctx.Next()
			return
		}
		if cfg.TriageCallbackKey != "" && constantTimeEqual(token, cfg.TriageCallbackKey) {
			ctx.Set("actor", "ai")
			ctx.Set("moderator", "ai")
			ctx.Next()
			return
		}
		ctx.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
	}
}

func bearerToken(ctx *gin.Context) string {
	header := ctx.GetHeader("Authorization")
	if after, ok := strings.CutPrefix(header, "Bearer "); ok {
		return after
	}
	return ""
}

/* =============================== turnstile ============================== */

type turnstileResponse struct {
	Success    bool     `json:"success"`
	ErrorCodes []string `json:"error-codes"`
}

// verifyTurnstile checks a Cloudflare Turnstile token.
//
// Turnstile over reCAPTCHA because it sets no cookie and collects no personal
// data, which keeps it out of the privacy policy's consent section entirely.
//
// Returns true when no secret is configured, so that a development deployment
// works without one. Validate() warns loudly about that at boot rather than
// letting it pass unnoticed into production.
func verifyTurnstile(cfg *Config, token, remoteIP string) (bool, error) {
	if cfg.TurnstileKey == "" {
		return true, nil
	}
	if token == "" {
		return false, nil
	}

	form := url.Values{}
	form.Set("secret", cfg.TurnstileKey)
	form.Set("response", token)
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	res, err := client.PostForm("https://challenges.cloudflare.com/turnstile/v0/siteverify", form)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()

	var parsed turnstileResponse
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		return false, err
	}
	return parsed.Success, nil
}

/* ============================= rate limiting ============================ */

// RateLimit describes one bucket's allowance.
type RateLimit struct {
	Action string
	Window time.Duration
	Max    int
}

var (
	// Per IP, per hour. Generous enough that a shared campus NAT does not lock
	// out a lecture hall, tight enough to make scripted submission tedious.
	limitPerIP = RateLimit{Action: "submit_ip", Window: time.Hour, Max: 5}
	// Per email, per day.
	limitPerEmail = RateLimit{Action: "submit_email", Window: 24 * time.Hour, Max: 3}
	// Per instructor, per hour, across all submitters. This is the one that
	// catches brigading: the per-person limits do nothing against thirty
	// people arriving at once to bury the same professor.
	limitPerInstructor = RateLimit{Action: "submit_instructor", Window: time.Hour, Max: 20}
	// Manage-key attempts, to make brute force against a 256-bit key even less
	// attractive than the arithmetic already does.
	limitPerManageKey = RateLimit{Action: "manage", Window: time.Hour, Max: 30}
)

// checkRateLimit increments a counter and reports whether it is still within
// the allowance.
//
// The increment happens in the database, in the same statement that reads the
// new value, so two concurrent submissions cannot both observe a count below
// the limit and both proceed.
func checkRateLimit(w *WriteClient, bucket string, limit RateLimit) (bool, error) {
	// A pointer, not a slice and not a bare int.
	//
	// `bump_rate_limit` returns `integer` and is not set-returning, so
	// PostgREST answers with a bare JSON scalar (`5`), not an array. Decoding
	// that into []int fails with "cannot unmarshal number into Go value of
	// type []int", which surfaced as every submission returning 503.
	//
	// A plain int would decode, but a SQL null would become 0 and 0 <= Max
	// allows the request -- the limiter would fail open on exactly the error
	// it exists to catch. A pointer keeps null distinguishable from zero.
	var count *int
	err := w.RPC("bump_rate_limit", map[string]any{
		"p_bucket": bucket,
		"p_action": limit.Action,
		"p_window": fmt.Sprintf("%d seconds", int(limit.Window.Seconds())),
	}, &count)
	if err != nil {
		return false, err
	}
	if count == nil {
		// A limiter that fails open is worse than one that fails closed here:
		// the endpoint it guards writes user content to a public site.
		return false, fmt.Errorf("rate limiter returned no count")
	}
	return *count <= limit.Max, nil
}

// clientIP extracts the caller's address behind Cloud Run's proxy.
//
// Read from the RIGHT of X-Forwarded-For, never the left.
//
// This used to take the leftmost entry, which is the one value in the header
// an attacker fully controls. Cloud Run preserves whatever X-Forwarded-For the
// client sent and appends the address it observed, so `X-Forwarded-For: 1.2.3.4`
// arrives as `1.2.3.4, <real client>` and the leftmost read returned "1.2.3.4".
//
// That is not a cosmetic difference. Three limiters key off this value --
// submissions per IP, manage-key attempts, and reports -- and all three were
// bypassable by varying one header per request. `submit_ip_hash` and the
// request log recorded the attacker's chosen string too, so the forensics that
// exist to investigate exactly this were being written by the person under
// investigation.
//
// Reading from the right instead means the value can only have been written by
// infrastructure we control: Cloud Run appends last, so the final entry is the
// address it observed. trustedProxyHops exists for the case where a proxy is
// added in front of it -- each additional hop appends one more entry, so the
// address to trust moves one position left.
const trustedProxyHops = 0

func clientIP(ctx *gin.Context) string {
	if forwarded := ctx.GetHeader("X-Forwarded-For"); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		idx := len(parts) - 1 - trustedProxyHops
		if idx < 0 {
			idx = 0
		}
		if candidate := strings.TrimSpace(parts[idx]); candidate != "" {
			return candidate
		}
	}
	host, _, err := net.SplitHostPort(ctx.Request.RemoteAddr)
	if err != nil {
		return ctx.Request.RemoteAddr
	}
	return host
}
