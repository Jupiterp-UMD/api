package main

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds everything the write path needs from the environment.
//
// Every value is read once at boot and validated there rather than at first
// use. A misconfigured moderation pipeline that only reveals itself when the
// first review arrives is a pipeline that is broken during exactly the window
// nobody is watching it.
type Config struct {
	// Existing, read path.
	DatabaseURL string
	DatabaseKey string
	Port        string

	// Write path. Empty ServiceKey disables /v1 entirely.
	ServiceKey     string
	EmailPepper    string
	AdminKey       string
	TurnstileKey   string
	BrevoAPIKey    string
	EmailFrom      string
	EmailFromName  string
	SiteBaseURL    string
	AllowedOrigins []string

	// Domains permitted to submit. Both terpmail and umd.edu, so that faculty
	// and staff can review too; `email_domain` is stored so those can be
	// identified or labelled later if that turns out to matter.
	AllowedEmailDomains []string

	// Automated triage. Everything here is optional: with TriageWebhookURL
	// empty, every verified review goes to the human queue and the system is
	// fully functional. That property is worth testing by actually running
	// with it empty rather than assuming.
	TriageWebhookURL    string
	TriageWebhookSecret string
	TriageCallbackKey   string
	TriageTimeout       time.Duration
	TriageRetryMax      time.Duration
	TriageMaxAttempts   int
	TriageDisabled      bool
	DiscordWebhookURL   string

	// Shadow mode gates. Both false means the classifier writes its opinion to
	// moderation_decisions with applied = false and a human decides
	// everything. That is stage one and it is the default.
	AutoReject         bool
	AutoApprove        bool
	AutoApproveMinConf float64
	AutoRejectMinConf  float64

	// If true, a submission is accepted without email verification when the
	// mail provider's daily cap is exhausted.
	//
	// OFF by default, and it should stay off. Turning it on makes exhausting
	// the cap a way to bypass verification, and verification is what backs the
	// "real UMD student" claim, the per-email dedupe, and most of the abuse
	// defences. The queue in email_outbox already prevents losing reviews when
	// the cap is hit -- it defers the send instead of dropping it -- which is
	// the actual problem this would otherwise be solving.
	AllowUnverifiedOnEmailCap bool
}

func LoadConfig() *Config {
	c := &Config{
		DatabaseURL: mustEnv("DATABASE_URL"),
		DatabaseKey: mustEnv("DATABASE_KEY"),
		Port:        envOr("PORT", "8080"),

		ServiceKey:    os.Getenv("DATABASE_SERVICE_KEY"),
		EmailPepper:   os.Getenv("REVIEW_EMAIL_PEPPER"),
		AdminKey:      os.Getenv("REVIEW_ADMIN_KEY"),
		TurnstileKey:  os.Getenv("TURNSTILE_SECRET_KEY"),
		BrevoAPIKey:   os.Getenv("BREVO_API_KEY"),
		EmailFrom:     os.Getenv("EMAIL_FROM_ADDRESS"),
		EmailFromName: envOr("EMAIL_FROM_NAME", "Jupiterp"),
		SiteBaseURL:   envOr("SITE_BASE_URL", "https://www.jupiterp.com"),

		AllowedOrigins:      splitList(envOr("V1_ALLOWED_ORIGINS", "https://www.jupiterp.com,https://jupiterp.com")),
		AllowedEmailDomains: splitList(envOr("REVIEW_EMAIL_DOMAINS", "terpmail.umd.edu,umd.edu")),

		TriageWebhookURL:    os.Getenv("REVIEW_TRIAGE_WEBHOOK_URL"),
		TriageWebhookSecret: os.Getenv("REVIEW_TRIAGE_WEBHOOK_SECRET"),
		TriageCallbackKey:   os.Getenv("REVIEW_TRIAGE_CALLBACK_KEY"),
		TriageTimeout:       envDuration("REVIEW_TRIAGE_TIMEOUT_SEC", 108000*time.Second),
		TriageRetryMax:      envDuration("REVIEW_TRIAGE_RETRY_MAX_SEC", 90000*time.Second),
		TriageMaxAttempts:   envInt("REVIEW_TRIAGE_MAX_ATTEMPTS", 3),
		TriageDisabled:      envBool("REVIEW_TRIAGE_DISABLED", false),
		DiscordWebhookURL:   os.Getenv("DISCORD_MODERATION_WEBHOOK_URL"),

		AutoReject:  envBool("REVIEW_TRIAGE_AUTO_REJECT", false),
		AutoApprove: envBool("REVIEW_TRIAGE_AUTO_APPROVE", false),
		// Asymmetric on purpose. A wrongly rejected review annoys one student
		// who can appeal or resubmit; a wrongly approved defamatory review is
		// the case that causes real harm to someone who never opted in. So the
		// bar to publish is higher than the bar to refuse.
		AutoApproveMinConf: envFloat("REVIEW_TRIAGE_AUTO_APPROVE_MIN_CONFIDENCE", 0.90),
		AutoRejectMinConf:  envFloat("REVIEW_TRIAGE_AUTO_REJECT_MIN_CONFIDENCE", 0.85),

		AllowUnverifiedOnEmailCap: envBool("REVIEW_ALLOW_UNVERIFIED_ON_EMAIL_CAP", false),
	}
	return c
}

// WriteEnabled reports whether the /v1 group can be served at all.
//
// The service key is the gate: without it there is no write path, and mounting
// routes that cannot work only produces confusing 500s.
func (c *Config) WriteEnabled() bool {
	return c.ServiceKey != "" && c.EmailPepper != ""
}

// Validate fails fast on configurations that are wrong in ways that would
// otherwise be silent.
func (c *Config) Validate() {
	if !c.WriteEnabled() {
		log.Printf("v1 write path DISABLED: DATABASE_SERVICE_KEY and REVIEW_EMAIL_PEPPER are both required")
		return
	}

	var fatal []string

	if c.AdminKey == "" {
		fatal = append(fatal, "REVIEW_ADMIN_KEY is required when the write path is enabled; "+
			"without it the moderation queue is unauthenticated")
	}
	if len(c.AdminKey) > 0 && len(c.AdminKey) < 32 {
		fatal = append(fatal, "REVIEW_ADMIN_KEY is shorter than 32 characters; it is the only "+
			"thing standing in front of the moderation surface")
	}

	// The single most important ordering constraint in the triage design.
	//
	// The sweeper escalates anything pending longer than TriageTimeout. The
	// retry parks quota-blocked reviews until the daily quota resets. If the
	// timeout is shorter than the parking window, every quota-blocked review
	// is escalated to a human before its retry ever fires, and the retry queue
	// is dead code that looks like it works.
	if c.TriageTimeout <= c.TriageRetryMax {
		fatal = append(fatal, "REVIEW_TRIAGE_TIMEOUT_SEC must exceed REVIEW_TRIAGE_RETRY_MAX_SEC "+
			"(got "+c.TriageTimeout.String()+" vs "+c.TriageRetryMax.String()+"); otherwise the "+
			"sweeper escalates every quota-blocked review before its retry runs")
	}

	if c.TriageWebhookURL != "" && c.TriageWebhookSecret == "" {
		fatal = append(fatal, "REVIEW_TRIAGE_WEBHOOK_SECRET is required when "+
			"REVIEW_TRIAGE_WEBHOOK_URL is set; the webhook endpoint is on the public "+
			"internet and the signature is what stops it being fed fabricated reviews")
	}
	if c.TriageCallbackKey != "" && len(c.TriageCallbackKey) < 32 {
		fatal = append(fatal, "REVIEW_TRIAGE_CALLBACK_KEY is shorter than 32 characters")
	}
	if c.TriageCallbackKey != "" && c.TriageCallbackKey == c.AdminKey {
		fatal = append(fatal, "REVIEW_TRIAGE_CALLBACK_KEY must not equal REVIEW_ADMIN_KEY; "+
			"the point of a scoped key is that a compromised n8n cannot reach the "+
			"rest of the admin surface")
	}

	if (c.AutoApprove || c.AutoReject) && c.TriageWebhookURL == "" {
		fatal = append(fatal, "auto-approve or auto-reject is enabled but "+
			"REVIEW_TRIAGE_WEBHOOK_URL is empty, so nothing will ever produce a decision")
	}

	for _, msg := range fatal {
		log.Printf("CONFIG ERROR: %s", msg)
	}
	if len(fatal) > 0 {
		log.Fatalf("refusing to start with %d configuration error(s)", len(fatal))
	}

	// Loud warnings for states that are valid but easy to be in by accident.
	if c.TurnstileKey == "" {
		log.Printf("WARNING: TURNSTILE_SECRET_KEY is empty; captcha verification is disabled")
	}
	if c.BrevoAPIKey == "" {
		log.Printf("WARNING: BREVO_API_KEY is empty; verification email will be queued but never sent")
	}
	if c.TriageWebhookURL == "" {
		log.Printf("Automated triage is off; every verified review goes to the human queue")
	}
	if c.AllowUnverifiedOnEmailCap {
		log.Printf("WARNING: REVIEW_ALLOW_UNVERIFIED_ON_EMAIL_CAP is on. Exhausting the mail " +
			"provider's daily cap now bypasses email verification, which is what backs the " +
			"per-email dedupe and the UMD-affiliation check.")
	}
	if c.AutoApprove {
		log.Printf("Auto-approve is ENABLED above confidence %.2f with zero flags", c.AutoApproveMinConf)
	}
}

func mustEnv(key string) string {
	val := os.Getenv(key)
	if val == "" {
		log.Fatalf("missing required env var: %s", key)
	}
	return val
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("WARNING: %s is not an integer; using %d", key, fallback)
	}
	return fallback
}

func envFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
		log.Printf("WARNING: %s is not a number; using %v", key, fallback)
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
		log.Printf("WARNING: %s is not a boolean; using %v", key, fallback)
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Second
		}
		log.Printf("WARNING: %s is not an integer number of seconds; using %s", key, fallback)
	}
	return fallback
}

func splitList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, strings.ToLower(trimmed))
		}
	}
	return out
}
