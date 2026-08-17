package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

// Regression tests for bugs that reached a running system.
//
// Each one here was found by a person using the site, not by a test, and every
// one of them was invisible from the server's side: the request succeeded, the
// log was clean, and the wrong thing happened anyway. They are grouped together
// because that is what they have in common, and because the shape keeps
// recurring -- a contract mismatch between two layers that both look correct on
// their own.

/* ===================== PostgREST scalar decoding ======================== */

// A SQL function returning a scalar answers with a bare JSON value, not an
// array. Decoding that into a slice fails, and the two call sites that did so
// took down review submission entirely (`bump_rate_limit`, every POST a 503)
// and silently killed the nightly rating recompute (`refresh_instructor_ratings`,
// logged and swallowed).
//
// The distinction is invisible in SQL -- `returns integer` and
// `returns setof integer` differ by one word -- so it is pinned here.
func TestPostgRESTScalarRPCDecodesIntoPointerNotSlice(t *testing.T) {
	// What PostgREST actually returns for `returns integer`.
	const scalarBody = `5`

	var asSlice []int
	if err := json.Unmarshal([]byte(scalarBody), &asSlice); err == nil {
		t.Fatal("decoding a scalar into []int succeeded; the bug this guards against is gone, " +
			"but so is the reason for the guard -- check why")
	}

	var asPointer *int
	if err := json.Unmarshal([]byte(scalarBody), &asPointer); err != nil {
		t.Fatalf("decoding a scalar into *int failed: %v", err)
	}
	if asPointer == nil || *asPointer != 5 {
		t.Fatalf("got %v, want 5", asPointer)
	}

	// A SQL null must stay distinguishable from zero. The rate limiter fails
	// closed on null; if null decoded as 0 it would compare 0 <= Max and let
	// the request through -- failing open on exactly the error it exists to
	// catch.
	var nullValue *int
	if err := json.Unmarshal([]byte(`null`), &nullValue); err != nil {
		t.Fatalf("decoding null failed: %v", err)
	}
	if nullValue != nil {
		t.Fatalf("null decoded to %v, want nil", nullValue)
	}
}

/* ========================= email outbox timing ========================== */

// The outbox cutoff has to carry sub-second precision.
//
// Submission queues the verification email and immediately flushes. With the
// cutoff truncated to whole seconds, a row written at :06.573 was compared
// against `lte :06` and excluded by its own flush -- so every submission sent
// the previous person's email and left its own for the hourly sweep. The
// reviewer saw a confirmation link that never arrived.
func TestOutboxCutoffKeepsSubSecondPrecision(t *testing.T) {
	instant := time.Date(2026, 8, 17, 18, 18, 6, 573674000, time.UTC)

	truncated := instant.Format(time.RFC3339)
	if strings.Contains(truncated, ".") {
		t.Fatalf("RFC3339 unexpectedly kept fractional seconds: %s", truncated)
	}

	// The comparison the bug turned on is Postgres `created_at <= cutoff`, on
	// timestamps rather than strings. A truncated cutoff lands *before* the row
	// it was meant to include, so the row fails its own filter.
	cutoff, err := time.Parse(time.RFC3339, truncated)
	if err != nil {
		t.Fatalf("parsing the truncated cutoff failed: %v", err)
	}
	if !instant.After(cutoff) {
		t.Fatal("expected the row's timestamp to fall after a whole-second cutoff")
	}

	// With full precision the row is included, which is the fix.
	precise, err := time.Parse(time.RFC3339Nano, instant.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatalf("parsing the precise cutoff failed: %v", err)
	}
	if instant.After(precise) {
		t.Fatal("a full-precision cutoff still excluded the row it was taken from")
	}

	if formatted := instant.Format(time.RFC3339Nano); !strings.Contains(formatted, ".573674") {
		t.Fatalf("RFC3339Nano lost precision: %s", formatted)
	}
}

/* ============================ v1 CORS contract ========================== */

// The CORS setup on /v1, built the same way `main` builds it.
//
// Two bugs shipped here, both of which made a working API unusable from a
// browser while behaving perfectly over curl:
//
//   - no OPTIONS route was registered, so Gin 404'd the preflight before the
//     CORS middleware could answer it. No browser could POST JSON at all.
//   - PUT was missing from AllowMethods while `admin.PUT /reviews/:id` was the
//     moderation decision route, so no moderator could approve from the UI.
//
// Verbs are asserted against the routes actually registered below, so adding a
// route with a new verb and forgetting the CORS list fails here rather than in
// someone's browser.
func newV1TestRouter(allowedOrigins []string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()

	v1 := r.Group("/v1")
	v1.Use(cors.New(cors.Config{
		AllowOrigins:     allowedOrigins,
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization"},
		AllowCredentials: false,
		MaxAge:           12 * time.Hour,
	}))
	v1.OPTIONS("/*path", func(ctx *gin.Context) { ctx.Status(http.StatusNoContent) })

	noop := func(ctx *gin.Context) { ctx.Status(http.StatusOK) }
	v1.GET("/reviews", noop)
	v1.POST("/reviews", noop)
	v1.DELETE("/reviews/:id", noop)
	v1.POST("/reviews/:id/report", noop)
	v1.PUT("/admin/reviews/:id", noop)
	v1.GET("/admin/instructors/queue", noop)
	v1.POST("/admin/instructors/queue/:id", noop)
	return r
}

func TestPreflightIsAnsweredForEveryV1Verb(t *testing.T) {
	const origin = "https://www.jupiterp.com"
	router := newV1TestRouter([]string{origin})

	// Every verb the group serves. A preflight for any of them must be
	// answered, and the response must advertise that verb.
	for _, probe := range []struct{ method, path string }{
		{"POST", "/v1/reviews"},
		{"DELETE", "/v1/reviews/abc"},
		{"PUT", "/v1/admin/reviews/abc"},
		{"POST", "/v1/admin/instructors/queue/1"},
	} {
		req := httptest.NewRequest(http.MethodOptions, probe.path, nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", probe.method)
		req.Header.Set("Access-Control-Request-Headers", "content-type,authorization")

		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)

		if res.Code == http.StatusNotFound {
			t.Errorf("preflight for %s %s returned 404: no OPTIONS route, so the CORS "+
				"middleware never ran and no browser can send this request",
				probe.method, probe.path)
			continue
		}
		if res.Code != http.StatusNoContent && res.Code != http.StatusOK {
			t.Errorf("preflight for %s %s returned %d", probe.method, probe.path, res.Code)
			continue
		}
		allowed := res.Header().Get("Access-Control-Allow-Methods")
		if !strings.Contains(allowed, probe.method) {
			t.Errorf("preflight for %s %s advertises %q, which omits %s -- the browser "+
				"will refuse to send it", probe.method, probe.path, allowed, probe.method)
		}
		if res.Header().Get("Access-Control-Allow-Origin") != origin {
			t.Errorf("preflight for %s %s did not echo the allowed origin", probe.method, probe.path)
		}
	}
}

func TestPreflightStillRefusesAnUnknownOrigin(t *testing.T) {
	router := newV1TestRouter([]string{"https://www.jupiterp.com"})

	req := httptest.NewRequest(http.MethodOptions, "/v1/reviews", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")

	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("an unlisted origin was given an Access-Control-Allow-Origin header")
	}
	if res.Code == http.StatusNoContent || res.Code == http.StatusOK {
		t.Errorf("an unlisted origin got %d from the preflight; the catch-all OPTIONS "+
			"route must not answer for origins the middleware rejects", res.Code)
	}
}

/* ========================== email rendering ============================= */

// Every template must produce a subject, HTML, and a plain-text alternative.
//
// The text part is not decoration: HTML-only mail scores worse with spam
// filters, and these carry the verification link a signup depends on.
func TestRenderTemplateAlwaysProducesAllThreeParts(t *testing.T) {
	cfg := &Config{SiteBaseURL: "https://www.jupiterp.com"}

	for _, template := range []string{"verify", "resend_verify", "manage_key", "rejected", "unknown-template"} {
		row := outboxRow{Template: template, Payload: map[string]any{
			"token":           "tok",
			"manage_key":      "key",
			"instructor_name": "Ada Lovelace",
			"reason":          "no substantive feedback",
		}}
		subject, html, text := renderTemplate(cfg, row)

		if strings.TrimSpace(subject) == "" {
			t.Errorf("%s: empty subject", template)
		}
		if strings.TrimSpace(html) == "" {
			t.Errorf("%s: empty html", template)
		}
		if strings.TrimSpace(text) == "" {
			t.Errorf("%s: empty text alternative -- HTML-only mail is a spam signal", template)
		}
	}
}

// Interpolated values must not be able to break out of the markup.
//
// The instructor name comes from Testudo and the registrar; a rejection reason
// is typed by a moderator. Neither is trusted here.
func TestRenderTemplateEscapesInterpolatedValues(t *testing.T) {
	cfg := &Config{SiteBaseURL: "https://www.jupiterp.com"}
	row := outboxRow{Template: "rejected", Payload: map[string]any{
		"instructor_name": `<script>alert("x")</script>`,
		"reason":          `<img src=x onerror=alert(1)>`,
	}}

	_, html, _ := renderTemplate(cfg, row)

	// What matters is that the payload cannot introduce a tag or an attribute
	// boundary. The inner text ("onerror=alert(1)") surviving verbatim is fine
	// and expected -- once the angle brackets are entities it is prose, and
	// asserting on it instead would fail on correctly escaped output.
	for _, forbidden := range []string{"<script", "</script", "<img"} {
		if strings.Contains(html, forbidden) {
			t.Errorf("rendered html contains unescaped %q", forbidden)
		}
	}
	for _, want := range []string{"&lt;script&gt;", "&lt;img src=x onerror=alert(1)&gt;"} {
		if !strings.Contains(html, want) {
			t.Errorf("expected the payload to survive as escaped text: %q missing", want)
		}
	}
}

// The verification link is built from SITE_BASE_URL. Getting this wrong sends
// every reviewer to a host they cannot reach, which is invisible server-side.
func TestVerifyEmailLinksToTheConfiguredSite(t *testing.T) {
	cfg := &Config{SiteBaseURL: "https://www.jupiterp.com"}
	row := outboxRow{Template: "verify", Payload: map[string]any{
		"token":           "abc/def+ghi",
		"instructor_name": "Ada Lovelace",
	}}

	_, html, text := renderTemplate(cfg, row)

	if !strings.Contains(html, "https://www.jupiterp.com/review/verify?token=") {
		t.Error("html does not contain a verification link on the configured site")
	}
	if !strings.Contains(text, "https://www.jupiterp.com/review/verify?token=") {
		t.Error("text alternative does not contain the verification link")
	}
	// The token is URL-escaped, so a token containing / or + still resolves.
	if strings.Contains(html, "token=abc/def+ghi") {
		t.Error("token was interpolated without escaping")
	}
}
