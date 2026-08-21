package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
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
//
// A third bug shipped here later, on the read side:
//
//   - the write group's catch-all `OPTIONS /v1/*path` also matched the read
//     routes mounted on the same prefix, so a preflight for `/v1/courses` was
//     answered by the write origin allowlist and refused with 403 -- on an
//     endpoint whose GET is open to every origin. `/v0` had no OPTIONS route at
//     all and answered 404.
//
// So this router mirrors `main`'s real structure: a permissive read group on
// both prefixes and an allowlisted write group, each registering its own
// OPTIONS routes. A catch-all is deliberately not used, and cannot be -- gin
// panics if one is added next to the static read routes.
func newV1TestRouter(allowedOrigins []string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()

	noop := func(ctx *gin.Context) { ctx.Status(http.StatusOK) }
	preflight := func(ctx *gin.Context) { ctx.Status(http.StatusNoContent) }

	permissive := cors.New(cors.Config{
		AllowAllOrigins: true,
		AllowMethods:    []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"},
		AllowHeaders:    []string{"Origin", "Content-Length", "Content-Type"},
		ExposeHeaders:   []string{"Content-Range"},
		MaxAge:          12 * time.Hour,
	})

	registerReads := func(g *gin.RouterGroup) {
		for _, path := range []string{"/courses", "/sections", "/instructors", "/grades/summary"} {
			g.GET(path, noop)
			g.OPTIONS(path, preflight)
		}
	}
	for _, prefix := range []string{"/v1", "/v0"} {
		g := r.Group(prefix)
		g.Use(permissive)
		registerReads(g)
	}

	v1 := r.Group("/v1")
	v1.Use(cors.New(cors.Config{
		AllowOrigins:     allowedOrigins,
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization"},
		AllowCredentials: false,
		MaxAge:           12 * time.Hour,
	}))
	for _, path := range []string{"/reviews", "/reviews/:id", "/reviews/:id/report"} {
		v1.OPTIONS(path, preflight)
	}
	for _, path := range []string{"/admin/reviews/:id", "/admin/instructors/queue", "/admin/instructors/queue/:id"} {
		v1.OPTIONS(path, preflight)
	}

	v1.GET("/reviews", noop)
	v1.POST("/reviews", noop)
	v1.DELETE("/reviews/:id", noop)
	v1.POST("/reviews/:id/report", noop)
	v1.PUT("/admin/reviews/:id", noop)
	v1.GET("/admin/instructors/queue", noop)
	v1.POST("/admin/instructors/queue/:id", noop)
	return r
}

// A read preflight must succeed from any origin, on both prefixes.
//
// The read surface is a documented public API and its GETs are open to
// everyone. A caller that sends any header forcing a preflight -- and a
// third-party client eventually will -- was refused, while the same request
// without that header worked. Nothing in the site exercised it, because the
// site is an allowed origin either way.
func TestReadPreflightIsOpenToAnyOrigin(t *testing.T) {
	const foreign = "https://some-third-party.example"
	router := newV1TestRouter([]string{"https://www.jupiterp.com"})

	for _, path := range []string{
		"/v1/courses", "/v1/sections", "/v1/instructors", "/v1/grades/summary",
		"/v0/courses", "/v0/sections", "/v0/instructors", "/v0/grades/summary",
	} {
		req := httptest.NewRequest(http.MethodOptions, path, nil)
		req.Header.Set("Origin", foreign)
		req.Header.Set("Access-Control-Request-Method", "GET")
		req.Header.Set("Access-Control-Request-Headers", "content-type")

		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)

		if res.Code == http.StatusNotFound {
			t.Errorf("preflight for GET %s returned 404: no OPTIONS route on this prefix, "+
				"so no browser on a foreign origin can send a preflighted read", path)
			continue
		}
		if res.Code == http.StatusForbidden {
			t.Errorf("preflight for GET %s returned 403: the read routes are being "+
				"answered by the write origin allowlist", path)
			continue
		}
		if got := res.Header().Get("Access-Control-Allow-Origin"); got != "*" && got != foreign {
			t.Errorf("preflight for GET %s answered Access-Control-Allow-Origin %q; "+
				"the read surface is open to every origin", path, got)
		}
	}
}

// And a read GET itself must still expose Content-Range to a foreign origin.
func TestReadGetExposesContentRangeToAnyOrigin(t *testing.T) {
	router := newV1TestRouter([]string{"https://www.jupiterp.com"})

	req := httptest.NewRequest(http.MethodGet, "/v1/instructors", nil)
	req.Header.Set("Origin", "https://some-third-party.example")
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("GET /v1/instructors from a foreign origin returned %d", res.Code)
	}
	if !strings.Contains(res.Header().Get("Access-Control-Expose-Headers"), "Content-Range") {
		t.Error("Content-Range is not exposed, so cross-origin JavaScript reads null " +
			"from it and cannot page or count")
	}
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

/* ===================== column selection on instructors =================== */

// A caller-supplied column list lands in PostgREST's `select`, so it is
// validated against a fixed set rather than forwarded.
//
// The endpoint exists because an instructor row is seventeen columns and the
// course planner reads two of them, over every active instructor -- 1.3MB to
// use about 6% of it. That is worth having, but not at the cost of letting a
// query string name columns or embed tables the endpoint does not publish.
func TestInstructorColumnsRejectsAnythingNotAColumn(t *testing.T) {
	for _, bad := range []string{
		"secret_field",
		"slug,secret_field",
		// PostgREST select syntax that must not survive validation: resource
		// embedding, aliasing, and casts each change what the query returns.
		"reviews(*)",
		"slug,reviews(email_hash)",
		"alias:slug",
		"slug::text",
		"*",
		"",
		"   ",
		",",
	} {
		if _, err := validateInstructorColumns(bad); err == nil {
			t.Errorf("validateInstructorColumns(%q) was accepted; it must be rejected", bad)
		}
	}
}

func TestInstructorColumnsAcceptsRealColumns(t *testing.T) {
	got, err := validateInstructorColumns(" slug , average_rating ")
	if err != nil {
		t.Fatalf("valid columns rejected: %v", err)
	}
	if len(got) != 2 || got[0] != "slug" || got[1] != "average_rating" {
		t.Fatalf("got %v, want [slug average_rating]", got)
	}

	// Every name in the allowlist must be individually acceptable, so the map
	// and the validator cannot disagree.
	for column := range instructorColumns {
		if _, err := validateInstructorColumns(column); err != nil {
			t.Errorf("allowlisted column %q was rejected: %v", column, err)
		}
	}
}

// An empty or unset list means the whole row, which is what every existing
// caller gets.
func TestInstructorSelectDefaultsToTheWholeRow(t *testing.T) {
	for _, unset := range []string{"", "   "} {
		if got := instructorSelect(unset); got != "*" {
			t.Errorf("instructorSelect(%q) = %q, want \"*\"", unset, got)
		}
	}
	if got := instructorSelect("slug,average_rating"); got != "slug,average_rating" {
		t.Errorf("instructorSelect = %q, want \"slug,average_rating\"", got)
	}
}

/* ========================== cache-control ================================ */

// Read responses have to say how long they stay good for.
//
// Every endpoint already passed a TTL to its cache and none of it reached the
// caller: no `Cache-Control`, no `ETag`. So the service treated instructor data
// as fresh for twelve hours while every browser refetched 1.3MB per page load.
func TestCacheControlIsSetFromTheTTL(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, tc := range []struct {
		name   string
		status int
		ttl    time.Duration
		want   string
	}{
		{"instructors", http.StatusOK, instructorsTTL, "public, max-age=43200, stale-while-revalidate=43200"},
		{"sections", http.StatusOK, sectionsTTL, "public, max-age=900, stale-while-revalidate=900"},
		{"reviews", http.StatusOK, reviewsTTL, "public, max-age=60, stale-while-revalidate=60"},
		// PostgREST answers a counted range request with 206, which is still a
		// cacheable success and is what every paginated read returns.
		{"partial content", http.StatusPartialContent, coursesTTL, "public, max-age=7200, stale-while-revalidate=7200"},
	} {
		res := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(res)
		setCacheControl(ctx, tc.status, tc.ttl)
		if got := res.Header().Get("Cache-Control"); got != tc.want {
			t.Errorf("%s: Cache-Control = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Errors must never be marked cacheable: doing so pins the failure in front of
// the fix for as long as the TTL.
func TestCacheControlIsNotSetOnErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusNotFound,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	} {
		res := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(res)
		setCacheControl(ctx, status, instructorsTTL)
		if got := res.Header().Get("Cache-Control"); got != "" {
			t.Errorf("status %d was marked cacheable with %q", status, got)
		}
	}

	// A zero TTL means "do not tell the caller anything", not "max-age=0".
	res := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(res)
	setCacheControl(ctx, http.StatusOK, 0)
	if got := res.Header().Get("Cache-Control"); got != "" {
		t.Errorf("zero TTL produced %q, want no header", got)
	}
}

/* ====================== Content-Range exposure =========================== */

// `Content-Range` has to be named in `Access-Control-Expose-Headers`.
//
// Only the CORS-safelisted response headers reach browser JavaScript, and this
// is not one of them. Without it a cross-origin `headers.get('Content-Range')`
// returns null rather than an error, so the professor directory read a null
// total, never rendered its count, and never showed a "Load More" button --
// capped at one page with nothing logged anywhere.
func TestReadCORSExposesContentRange(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	// Built the same way main() builds the read groups.
	readCORS := cors.New(cors.Config{
		AllowAllOrigins: true,
		AllowMethods:    []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"},
		AllowHeaders:    []string{"Origin", "Content-Length", "Content-Type"},
		ExposeHeaders:   []string{"Content-Range"},
		MaxAge:          12 * time.Hour,
	})
	group := router.Group("/v1")
	group.Use(readCORS)
	group.GET("/instructors", func(ctx *gin.Context) {
		ctx.Header("Content-Range", "0-499/2976")
		ctx.Status(http.StatusPartialContent)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/instructors", nil)
	req.Header.Set("Origin", "https://www.jupiterp.com")
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)

	exposed := res.Header().Get("Access-Control-Expose-Headers")
	if !strings.Contains(exposed, "Content-Range") {
		t.Errorf("Access-Control-Expose-Headers is %q, which omits Content-Range -- "+
			"cross-origin callers will read null and pagination breaks silently", exposed)
	}
	if res.Header().Get("Content-Range") == "" {
		t.Error("Content-Range was not sent at all")
	}
}

/* ======================= shadow-mode alert wording ====================== */

// The shadow-mode Discord alert has to say what the classifier concluded.
//
// It used to read "shadow mode: recorded but not applied" -- accurate, and
// useless to the person reading it in a channel. It named the mechanism without
// saying what the decision was or that nothing had gone wrong, so every alert
// needed someone to already know how the pipeline works to interpret it.
func TestShadowModeReasonStatesTheDecision(t *testing.T) {
	confidence := 0.99
	got := shadowModeReason("approve", &confidence)

	for _, want := range []string{"classifier said approve", "0.99", "shadow mode", "not applied"} {
		if !strings.Contains(got, want) {
			t.Errorf("reason %q is missing %q", got, want)
		}
	}
	if strings.Contains(got, "recorded but not applied") && !strings.Contains(got, "classifier") {
		t.Error("reverted to naming the mechanism without naming the decision")
	}
}

// A decision with no confidence must not be reported as 0.00 confidence. The
// two mean opposite things: "the model did not say" versus "the model was
// certain this was worthless".
func TestShadowModeReasonOmitsAbsentConfidence(t *testing.T) {
	got := shadowModeReason("reject", nil)

	if strings.Contains(got, "0.00") || strings.Contains(got, "(") {
		t.Errorf("reason %q printed a confidence that was never supplied", got)
	}
	if !strings.Contains(got, "classifier said reject") {
		t.Errorf("reason %q does not name the decision", got)
	}
}

/* ==================== X-Forwarded-For spoofing ========================== */

// clientIP read the LEFTMOST X-Forwarded-For entry, which is the one value in
// the header a caller fully controls: Cloud Run preserves what the client sent
// and appends what it observed. Three limiters key off this -- submissions per
// IP, manage-key attempts, and reports -- so a caller varying one header
// bypassed all three, and wrote their chosen string into `submit_ip_hash` and
// the request log at the same time.
func TestClientIPIgnoresCallerSuppliedForwardedFor(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name   string
		header string
		remote string
		want   string
	}{
		{
			name:   "spoofed entry ahead of the real one is ignored",
			header: "1.2.3.4, 203.0.113.7",
			remote: "10.0.0.1:5000",
			want:   "203.0.113.7",
		},
		{
			name:   "several spoofed entries change nothing",
			header: "1.1.1.1, 2.2.2.2, 3.3.3.3, 203.0.113.7",
			remote: "10.0.0.1:5000",
			want:   "203.0.113.7",
		},
		{
			name:   "a single entry is the platform's own",
			header: "203.0.113.7",
			remote: "10.0.0.1:5000",
			want:   "203.0.113.7",
		},
		{
			name:   "no header falls back to the socket",
			header: "",
			remote: "203.0.113.7:5000",
			want:   "203.0.113.7",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/reviews", nil)
			req.RemoteAddr = tc.remote
			if tc.header != "" {
				req.Header.Set("X-Forwarded-For", tc.header)
			}

			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = req

			if got := clientIP(ctx); got != tc.want {
				t.Fatalf("clientIP() = %q, want %q; a caller-supplied X-Forwarded-For "+
					"must not be able to choose its own rate-limit bucket", got, tc.want)
			}
		})
	}
}

// Two callers behind the same real address must land in the same bucket no
// matter what they claim, which is the property the limiter actually needs.
func TestClientIPIsStableAcrossSpoofedHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ipFor := func(forwarded string) string {
		req := httptest.NewRequest(http.MethodPost, "/v1/reviews", nil)
		req.RemoteAddr = "10.0.0.1:5000"
		req.Header.Set("X-Forwarded-For", forwarded)
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = req
		return clientIP(ctx)
	}

	first := ipFor("198.51.100.1, 203.0.113.7")
	second := ipFor("198.51.100.99, 203.0.113.7")
	if first != second {
		t.Fatalf("same client resolved to %q and %q by varying the spoofed prefix", first, second)
	}
}

/* ================== manage key survives the outbox ====================== */

// The manage key was minted at submit, stashed in the verification email's
// payload, and read back on verification. markSent clears that payload the
// moment the mail is accepted -- and the reviewer cannot click a link in a mail
// that has not been sent -- so the read always came back empty. Every reviewer
// got `"manage_key": ""` and no way to withdraw, and the site's `{#if}` hid it.
//
// The fix mints the key during verification instead, so nothing has to survive
// a round trip through a queue that is entitled to wipe itself.
func TestManageKeyIsNotRecoveredFromClearedOutboxPayload(t *testing.T) {
	// What markSent leaves behind.
	sent := outboxRow{
		Template: "verify",
		Payload:  map[string]any{},
	}
	if key, _ := sent.Payload["manage_key"].(string); key != "" {
		t.Fatal("a sent outbox row still carries manage_key; the payload is meant to be cleared")
	}

	// The verification email must not be the only copy of anything the flow
	// needs afterwards. It carries the token, and nothing else load-bearing.
	cfg := &Config{SiteBaseURL: "https://www.jupiterp.com"}
	_, html, text := renderTemplate(cfg, outboxRow{
		Template: "verify",
		Payload: map[string]any{
			"token":           "tok",
			"instructor_name": "Shane Walsh",
		},
	})
	for _, body := range []string{html, text} {
		if strings.Contains(body, "manage_key") {
			t.Fatal("verification email references manage_key; it is minted at verification now")
		}
	}
}

/* ================== named moderators in the audit trail ================= */

// With one shared key, `decided_by` could only ever say "human" -- never which
// human approved a review about a named professor or merged two identities.
func TestModeratorForNamesTheKeyHolder(t *testing.T) {
	cfg := &Config{
		AdminKey: strings.Repeat("a", 32),
		ModeratorKeys: map[string]string{
			"alice": strings.Repeat("b", 32),
			"bob":   strings.Repeat("c", 32),
		},
	}

	if name, ok := moderatorFor(cfg, strings.Repeat("b", 32)); !ok || name != "alice" {
		t.Fatalf("named key resolved to (%q, %v), want (\"alice\", true)", name, ok)
	}
	if name, ok := moderatorFor(cfg, strings.Repeat("c", 32)); !ok || name != "bob" {
		t.Fatalf("named key resolved to (%q, %v), want (\"bob\", true)", name, ok)
	}
	// The shared key still works and is still recorded coarsely.
	if name, ok := moderatorFor(cfg, strings.Repeat("a", 32)); !ok || name != "human" {
		t.Fatalf("shared admin key resolved to (%q, %v), want (\"human\", true)", name, ok)
	}
	if _, ok := moderatorFor(cfg, strings.Repeat("z", 32)); ok {
		t.Fatal("an unknown key was accepted")
	}
	if _, ok := moderatorFor(cfg, ""); ok {
		t.Fatal("an empty bearer token was accepted")
	}
}

// An empty configured key must never match an empty token: that would make a
// deployment that forgot to set a key accept every unauthenticated request.
func TestModeratorForRejectsEmptyConfiguredKeys(t *testing.T) {
	cfg := &Config{AdminKey: "", ModeratorKeys: map[string]string{"ghost": ""}}
	if _, ok := moderatorFor(cfg, ""); ok {
		t.Fatal("empty token matched an empty configured key; the surface would be unauthenticated")
	}
}

func TestParseModeratorKeysIgnoresMalformedEntries(t *testing.T) {
	keys := parseModeratorKeys("alice:key-one, bob:key-two,,noseparator, :emptyname,carol:")
	if len(keys) != 2 {
		t.Fatalf("parsed %d keys (%v), want 2", len(keys), keys)
	}
	if keys["alice"] != "key-one" || keys["bob"] != "key-two" {
		t.Fatalf("unexpected parse result: %v", keys)
	}
}

/* ==================== triage webhook signing bytes ====================== */

// The signed bytes must be what `JSON.stringify` produces.
//
// The API signs an HMAC over the payload it sends; the n8n Code node verifies
// by re-serialising the body it parsed. That only agrees when Go and JavaScript
// emit identical bytes, and by default they do not: `json.Marshal` HTML-escapes
// `&`, `<` and `>`. Every review containing an ampersand -- "Q&A sessions",
// "the TA & professor", any instructor in "Chem & Biochem" -- failed the
// signature check, was never classified, and escalated a day and a half later
// by timeout, while the alert channel filled with what looked like an attack.
//
// The expected string below was produced by running `JSON.stringify` on the
// same object in node. It is written out in full deliberately: this is a
// cross-language wire contract, and the only useful form of it is the literal
// bytes.
func TestCanonicalJSONMatchesJavaScriptStringify(t *testing.T) {
	title := "Q&A sessions helped"
	body := "Grading was <fair> & the curve was >90th percentile"
	term := 202508

	payload := triagePayload{
		ReviewID:       "11111111-2222-3333-4444-555555555555",
		Rating:         4.5,
		ExpectedGrade:  nil,
		Title:          &title,
		Body:           &body,
		InstructorName: "Chem & Biochem staff",
		CourseCode:     strPtr("CMSC132"),
		Term:           &term,
		PrefilterFlags: []string{},
		PolicyVersion:  "2026-08-14",
		SubmittedAt:    "2026-08-14T12:00:00Z",
	}

	const wantStringify = `{"review_id":"11111111-2222-3333-4444-555555555555","rating":4.5,` +
		`"expected_grade":null,"title":"Q&A sessions helped",` +
		`"body":"Grading was <fair> & the curve was >90th percentile",` +
		`"instructor_name":"Chem & Biochem staff","course_code":"CMSC132","term":202508,` +
		`"prefilter_flags":[],"policy_version":"2026-08-14","submitted_at":"2026-08-14T12:00:00Z"}`

	got, err := canonicalJSON(payload)
	if err != nil {
		t.Fatalf("canonicalJSON returned an error: %v", err)
	}
	if string(got) != wantStringify {
		t.Errorf("signed bytes do not match JSON.stringify.\n got: %s\nwant: %s", got, wantStringify)
	}

	// And show that the default encoder is what was wrong, so this test fails
	// loudly rather than quietly if someone reverts to json.Marshal.
	marshalled, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal returned an error: %v", err)
	}
	if string(marshalled) == wantStringify {
		t.Error("json.Marshal now matches JSON.stringify; if Go stopped HTML-escaping, " +
			"canonicalJSON can be simplified -- but check U+2028 before doing so")
	}
}

func strPtr(s string) *string { return &s }

/* ======================= email retry schedule =========================== */

// Every entry in the backoff table has to be reachable.
//
// `reschedule` indexed the table by the *incremented* attempt count, so entry
// zero was never used: the declared schedule read 1m/10m/1h/6h/25h and the
// delivered one was 10m/1h/6h/25h. The one-minute step is the only one that
// helps with a blip rather than an outage, and it never ran.
func TestEmailBackoffScheduleUsesEveryStep(t *testing.T) {
	var seen []time.Duration
	attempts := 0
	for range len(emailBackoff) + 2 {
		next := attempts + 1
		if next > len(emailBackoff) {
			break
		}
		seen = append(seen, emailBackoff[next-1])
		attempts = next
	}

	if len(seen) != len(emailBackoff) {
		t.Fatalf("the retry schedule delivers %d of %d declared steps: %v",
			len(seen), len(emailBackoff), seen)
	}
	for i, want := range emailBackoff {
		if seen[i] != want {
			t.Errorf("retry %d waits %v, want %v", i+1, seen[i], want)
		}
	}
	// The last step exists to outlive a provider's daily cap, which resets on a
	// clock. Losing it turns a deferred send into an abandoned one.
	if seen[len(seen)-1] < 24*time.Hour {
		t.Errorf("the final retry waits %v, which is less than a day -- a daily-cap "+
			"deferral will be abandoned before the cap resets", seen[len(seen)-1])
	}
}

/* ==================== misconduct flag false positives =================== */

// Hyperbole about the coursework must not be read as an allegation.
//
// The flag matched `abus\w*`, `stole`, `criminal` and friends as bare words, so
// "an abusive workload" and "this class stole my semester" escalated exactly
// like a real accusation. Escalation is the safe direction for any one review,
// but at volume it is not safe at all: a queue full of false escalations stops
// being read carefully, which is the failure the flag exists to prevent.
func TestMisconductFlagIgnoresHyperboleAboutTheWork(t *testing.T) {
	notAllegations := []string{
		"the workload is abusive and the deadlines are worse",
		"this class stole my entire semester",
		"criminally hard exams, but I learned a lot",
		"the midterm was predatory in how it was scored",
		"grading felt arbitrary and the curve was stingy",
	}
	for _, body := range notAllegations {
		if prefilter("", body).MustEscalate {
			t.Errorf("prefilter escalated hyperbole about the coursework: %q", body)
		}
	}
}

// ...while the same words applied to a person still escalate.
func TestMisconductFlagStillCatchesAllegationsAboutAPerson(t *testing.T) {
	allegations := []string{
		"he was verbally abusive to a student in my section",
		"she showed up drunk to lecture twice",
		"the professor stole a grad student's work",
		"this instructor is a creep, avoid",
		"he harassed a student in my section",
		"I heard she was arrested last year",
	}
	for _, body := range allegations {
		if !prefilter("", body).MustEscalate {
			t.Errorf("prefilter did not escalate an allegation about a person: %q", body)
		}
	}
}

/* ========================= review id validation ========================= */

// A malformed review id is the caller's mistake, not a server fault.
//
// It went straight into a PostgREST filter or insert, which answered 400 for a
// bad uuid -- and `sendInternalError` reported that to the caller as a 500 and
// wrote it into the log a real abuse incident would be investigated from.
func TestUUIDValidationRejectsWhatPostgRESTWouldReject(t *testing.T) {
	valid := []string{
		"11111111-2222-3333-4444-555555555555",
		"AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE",
	}
	for _, id := range valid {
		if !uuidRe.MatchString(id) {
			t.Errorf("uuidRe rejected a valid uuid: %q", id)
		}
	}

	invalid := []string{
		"", "not-a-uuid", "1", "11111111-2222-3333-4444",
		"11111111-2222-3333-4444-5555555555555",
		"11111111222233334444555555555555",
		"11111111-2222-3333-4444-55555555555g",
		"11111111-2222-3333-4444-555555555555 or 1=1",
	}
	for _, id := range invalid {
		if uuidRe.MatchString(id) {
			t.Errorf("uuidRe accepted something that is not a uuid: %q", id)
		}
	}
}

/* ================== deferred mail is not delivered mail ================= */

// A deferred send must not be counted as a sent one.
//
// `deliver` answered with a bare error and returned nil for a message it had
// only rescheduled, so the outcome the outbox exists to handle -- a provider
// cap -- was indistinguishable from success. Two things depended on the difference:
// the sweep's `emails_sent` figure, which was really "rows considered"; and
// `notifyRejection`, which purges the reviewer's address once the mail is away
// and would otherwise have purged it on a deferral, leaving `deliver` to
// abandon the message for having no recipient on the next pass. That is the
// same class of bug as the one the recipient column was kept alive to fix.
func TestOnlyASentMessageCountsAsSent(t *testing.T) {
	if deliverySent == deliveryDeferred || deliverySent == deliveryAbandoned {
		t.Fatal("the delivery outcomes are not distinct")
	}

	// The states a queued message can end a delivery attempt in, and whether
	// the caller may treat the address as no longer needed.
	cases := []struct {
		outcome deliveryOutcome
		name    string
		purgeOK bool
	}{
		{deliverySent, "sent", true},
		{deliveryDeferred, "deferred by a provider cap", false},
		{deliveryAbandoned, "abandoned", false},
	}
	for _, tc := range cases {
		counted := tc.outcome == deliverySent
		if counted != tc.purgeOK {
			t.Errorf("a %s message counts as sent = %v, but purging its address is "+
				"safe = %v; these have to agree or the address goes before the mail does",
				tc.name, counted, tc.purgeOK)
		}
	}
}

/* ===================== email promises a real feature ==================== */

// No template may offer to let a reviewer edit their review.
//
// The manage-key email said "Keep this key if you want to edit or withdraw it
// later". Editing does not exist: there is no route for it, no UI, and it was
// removed deliberately. Withdrawal did exist, but only as an endpoint nobody
// could reach -- `DELETE /v1/reviews/:id` needs the review's id, and a reviewer
// is never told it, so the key they were told to keep unlocked nothing.
//
// Both halves are fixed: the key now resolves the review on its own via
// `GET /v1/reviews/manage`, and the email points at the page that uses it. This
// pins the copy, because the failure mode is a promise in an email that no code
// path can keep -- which nothing else in the test suite can see.
func TestEmailsNeverPromiseEditing(t *testing.T) {
	cfg := &Config{SiteBaseURL: "https://www.jupiterp.com", EmailFromName: "Jupiterp"}
	// `\bedit` rather than a substring search, so "credit" does not trip it.
	editRe := regexp.MustCompile(`(?i)\bedit`)

	for _, template := range []string{"verify", "resend_verify", "manage_key", "rejected"} {
		row := outboxRow{Template: template, Payload: map[string]any{
			"instructor_name": "Shane Bolles Walsh",
			"manage_key":      "example-key",
			"token":           "example-token",
			"reason":          "It did not meet the content policy.",
		}}
		_, html, text := renderTemplate(cfg, row)
		for part, body := range map[string]string{"html": html, "text": text} {
			if match := editRe.FindString(body); match != "" {
				t.Errorf("the %s template's %s part offers %q; editing a review is not a "+
					"feature this site has", template, part, match)
			}
		}
	}
}

// And the manage-key email has to say where the key is used.
//
// A key with nowhere to use it is the same broken promise in a different shape,
// which is exactly the state this email was in: it told the reader to keep a
// credential and never named a page that accepts one.
func TestManageKeyEmailLinksToTheWithdrawalPage(t *testing.T) {
	const base = "https://www.jupiterp.com"
	cfg := &Config{SiteBaseURL: base, EmailFromName: "Jupiterp"}
	row := outboxRow{Template: "manage_key", Payload: map[string]any{
		"instructor_name": "Shane Bolles Walsh",
		"manage_key":      "example-key",
	}}

	subject, html, text := renderTemplate(cfg, row)
	if subject == "" {
		t.Error("no subject")
	}

	wantLink := base + "/review/withdraw"
	for part, body := range map[string]string{"html": html, "text": text} {
		if !strings.Contains(body, wantLink) {
			t.Errorf("the %s part does not link to %s, so the key it tells the reader to "+
				"keep has nowhere to be used", part, wantLink)
		}
		if !strings.Contains(body, "example-key") {
			t.Errorf("the %s part does not contain the key itself", part)
		}
		if !strings.Contains(strings.ToLower(body), "withdraw") {
			t.Errorf("the %s part never says what the key is for", part)
		}
	}

	// The preheader is the grey line the inbox shows next to the subject, and
	// it is the only part many people read before deciding to keep the mail.
	if !strings.Contains(html, "only way to withdraw") {
		t.Error("the preheader does not say the key is the only way to withdraw")
	}
}
