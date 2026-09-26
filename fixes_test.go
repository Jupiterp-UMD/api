package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
)

// Handler-level tests against a fake PostgREST.
//
// The bugs these pin were all in what a handler *sent* to the database -- a
// missing guard on an update, a filter never added, a request made when it
// should not have been -- so the fake records every request and the tests
// assert on those, rather than on a database that is not here.

type recordedRequest struct {
	Method string
	Table  string
	Query  string
	Body   string
}

type fakePostgREST struct {
	t      *testing.T
	server *httptest.Server
	mu     sync.Mutex
	seen   []recordedRequest
	// "METHOD table" -> status and body. Anything unlisted gets a benign
	// default: an empty array, or 1 for an RPC.
	routes map[string]func(r recordedRequest) (int, string)
}

func newFakePostgREST(t *testing.T) *fakePostgREST {
	t.Helper()
	fake := &fakePostgREST{t: t, routes: map[string]func(recordedRequest) (int, string){}}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := recordedRequest{
			Method: r.Method,
			Table:  strings.TrimPrefix(r.URL.Path, "/rest/v1/"),
			Query:  r.URL.RawQuery,
			Body:   string(body),
		}
		fake.mu.Lock()
		fake.seen = append(fake.seen, req)
		handler := fake.routes[req.Method+" "+req.Table]
		fake.mu.Unlock()

		status, payload := http.StatusOK, "[]"
		if strings.HasPrefix(req.Table, "rpc/") {
			payload = "1"
		}
		if handler != nil {
			status, payload = handler(req)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakePostgREST) on(method, table, body string) {
	f.onFunc(method, table, func(recordedRequest) (int, string) { return http.StatusOK, body })
}

func (f *fakePostgREST) onFunc(method, table string, handler func(recordedRequest) (int, string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[method+" "+table] = handler
}

func (f *fakePostgREST) requests(method, table string) []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedRequest
	for _, req := range f.seen {
		if req.Method == method && req.Table == table {
			out = append(out, req)
		}
	}
	return out
}

func testConfig() *Config {
	return &Config{
		EmailPepper:         "test-pepper",
		SiteBaseURL:         "https://www.jupiterp.com",
		AllowedEmailDomains: []string{"umd.edu", "terpmail.umd.edu"},
		TriageMaxAttempts:   3,
		AutoApprove:         true,
		AutoApproveMinConf:  0.9,
		// Dispatch runs in a goroutine after verification; with triage
		// disabled it only escalates, which the fake absorbs.
		TriageDisabled: true,
	}
}

func servers(t *testing.T) (*fakePostgREST, *ReviewServer, *ModerationServer) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	fake := newFakePostgREST(t)
	cfg := testConfig()
	write := NewWriteClient(fake.server.URL, "service-key")
	email := NewEmailSender(cfg, write)
	triage := NewTriageClient(cfg, write)
	return fake, NewReviewServer(cfg, write, email, triage), NewModerationServer(cfg, write, email, triage)
}

func serve(handler gin.HandlerFunc, method, route, target, body string, actor string) *httptest.ResponseRecorder {
	r := gin.New()
	r.Handle(method, route, func(ctx *gin.Context) {
		if actor != "" {
			ctx.Set("actor", actor)
			ctx.Set("moderator", actor)
		}
		ctx.Next()
	}, handler)
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	r.ServeHTTP(res, req)
	return res
}

func decodeBody(t *testing.T, res *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %q", res.Body.String())
	}
	return out
}

const testReviewID = "0b0f4f8e-3a52-4a5e-9d3a-0c1f5b8f2a11"

/* ======================== submit: token insert fails ==================== */

// A verification token that failed to store used to be logged and the email
// sent anyway: a link that could only ever say "not valid", while the review
// held the reviewer's dedupe slot for two days.
func TestSubmitUndoesTheReviewWhenItsTokenCannotBeStored(t *testing.T) {
	fake, reviews, _ := servers(t)
	fake.on(http.MethodGet, "instructors", `[{"id":7,"slug":"shane-walsh","name":"Shane Walsh"}]`)
	fake.on(http.MethodPost, "reviews", `[{"id":"`+testReviewID+`","instructor_id":7,"status":"unverified"}]`)
	fake.onFunc(http.MethodPost, "review_tokens", func(recordedRequest) (int, string) {
		return http.StatusInternalServerError, `{"message":"boom"}`
	})

	res := serve(reviews.HandleSubmit, http.MethodPost, "/v1/reviews", "/v1/reviews",
		`{"instructor_slug":"shane-walsh","rating":4,"email":"student@umd.edu"}`, "")

	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", res.Code, res.Body.String())
	}
	deletes := fake.requests(http.MethodDelete, "reviews")
	if len(deletes) != 1 || !strings.Contains(deletes[0].Query, "id=eq."+testReviewID) {
		t.Fatalf("the orphaned review was not deleted: %+v", deletes)
	}
	if queued := fake.requests(http.MethodPost, "email_outbox"); len(queued) != 0 {
		t.Fatalf("a verification email was queued for a token that does not exist: %+v", queued)
	}
}

/* ============================ verify: the race ========================== */

func verifyFake(fake *fakePostgREST) {
	fake.on(http.MethodGet, "review_tokens",
		`[{"token_hash":"h","review_id":"`+testReviewID+`","purpose":"verify","expires_at":"2999-01-01T00:00:00Z"}]`)
	fake.on(http.MethodGet, "reviews",
		`[{"id":"`+testReviewID+`","instructor_id":7,"rating":4,"status":"unverified"}]`)
}

// Two visits can both read `unverified`. The one whose guarded update matches
// nothing lost the race, and must hand out nothing -- the winner's key is the
// one that works and the one being emailed.
func TestVerifyThatLosesTheRaceHandsOutNoKey(t *testing.T) {
	fake, reviews, _ := servers(t)
	verifyFake(fake)
	fake.on(http.MethodPatch, "reviews", `[]`)

	res := serve(reviews.HandleVerify, http.MethodGet, "/v1/reviews/verify/:token", "/v1/reviews/verify/tok", "", "")

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	body := decodeBody(t, res)
	if body["status"] != "already_verified" {
		t.Fatalf("status = %v, want already_verified", body["status"])
	}
	if _, handedOut := body["manage_key"]; handedOut {
		t.Fatal("the losing visit handed out a manage key that does not work")
	}

	patches := fake.requests(http.MethodPatch, "reviews")
	if len(patches) != 1 || !strings.Contains(patches[0].Query, "status=eq.unverified") {
		t.Fatalf("the verify update is not guarded on status: %+v", patches)
	}
}

func TestVerifyThatWinsReturnsTheKey(t *testing.T) {
	fake, reviews, _ := servers(t)
	verifyFake(fake)
	fake.on(http.MethodPatch, "reviews", `[{"id":"`+testReviewID+`"}]`)

	res := serve(reviews.HandleVerify, http.MethodGet, "/v1/reviews/verify/:token", "/v1/reviews/verify/tok", "", "")

	body := decodeBody(t, res)
	if body["status"] != "verified" || body["manage_key"] == "" || body["manage_key"] == nil {
		t.Fatalf("winning verification did not return a key: %v", body)
	}
}

/* ================= decide: the classifier and escalated reviews ========= */

func decideFake(fake *fakePostgREST, status string) {
	fake.on(http.MethodGet, "reviews",
		`[{"id":"`+testReviewID+`","instructor_id":7,"rating":4,"status":"`+status+`"}]`)
}

// `escalated` means a person decides. A classifier verdict on one -- late, or
// retried -- used to be applied like any other, so with auto-approve on it
// could publish a review a moderator had deliberately held back.
func TestClassifierCannotDecideAnEscalatedReview(t *testing.T) {
	fake, _, moderation := servers(t)
	decideFake(fake, "escalated")

	res := serve(moderation.HandleDecide, http.MethodPut, "/v1/admin/reviews/:id", "/v1/admin/reviews/"+testReviewID,
		`{"action":"approve","confidence":0.99,"categories":[]}`, "ai")

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 so the workflow does not retry: %s", res.Code, res.Body.String())
	}
	if body := decodeBody(t, res); body["applied"] != false {
		t.Fatalf("applied = %v, want false", body["applied"])
	}
	if patches := fake.requests(http.MethodPatch, "reviews"); len(patches) != 0 {
		t.Fatalf("an escalated review was written by the classifier: %+v", patches)
	}
	recorded := fake.requests(http.MethodPost, "moderation_decisions")
	if len(recorded) != 1 || !strings.Contains(recorded[0].Body, `"applied":false`) {
		t.Fatalf("the shadow comparison was not recorded: %+v", recorded)
	}
}

// And the write itself re-checks, so a human escalating between the
// classifier's read and its write still wins.
func TestClassifierWriteIsGuardedToPending(t *testing.T) {
	fake, _, moderation := servers(t)
	decideFake(fake, "pending")
	fake.on(http.MethodPatch, "reviews", `[{"id":"`+testReviewID+`"}]`)

	serve(moderation.HandleDecide, http.MethodPut, "/v1/admin/reviews/:id", "/v1/admin/reviews/"+testReviewID,
		`{"action":"approve","confidence":0.99,"categories":[]}`, "ai")

	patches := fake.requests(http.MethodPatch, "reviews")
	if len(patches) != 1 || !strings.Contains(patches[0].Query, "status=in.%28pending%29") {
		t.Fatalf("classifier write not guarded to pending: %+v", patches)
	}
}

func TestHumanWriteMayStillDecideEscalated(t *testing.T) {
	fake, _, moderation := servers(t)
	decideFake(fake, "escalated")
	fake.on(http.MethodPatch, "reviews", `[{"id":"`+testReviewID+`"}]`)

	serve(moderation.HandleDecide, http.MethodPut, "/v1/admin/reviews/:id", "/v1/admin/reviews/"+testReviewID,
		`{"action":"approve"}`, "human")

	patches := fake.requests(http.MethodPatch, "reviews")
	if len(patches) != 1 || !strings.Contains(patches[0].Query, "status=in.%28pending%2Cescalated%29") {
		t.Fatalf("human write guard changed: %+v", patches)
	}
}

/* ========================= remove: published reviews ==================== */

func TestRemoveTakesDownAnApprovedReviewAndClosesItsReports(t *testing.T) {
	fake, _, moderation := servers(t)
	decideFake(fake, "approved")
	fake.onFunc(http.MethodPatch, "reviews", func(recordedRequest) (int, string) {
		return http.StatusOK, `[{"id":"` + testReviewID + `"}]`
	})
	fake.on(http.MethodPatch, "review_reports", `[{"id":1},{"id":2}]`)

	res := serve(moderation.HandleDecide, http.MethodPut, "/v1/admin/reviews/:id", "/v1/admin/reviews/"+testReviewID,
		`{"action":"remove","reason":"names a student"}`, "human")

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Code, res.Body.String())
	}
	body := decodeBody(t, res)
	if body["status"] != "rejected" || body["reports_resolved"] != float64(2) {
		t.Fatalf("unexpected response: %v", body)
	}

	patches := fake.requests(http.MethodPatch, "reviews")
	if len(patches) != 1 || !strings.Contains(patches[0].Query, "status=in.%28approved%29") ||
		!strings.Contains(patches[0].Body, `"status":"rejected"`) {
		t.Fatalf("removal write wrong: %+v", patches)
	}
	reports := fake.requests(http.MethodPatch, "review_reports")
	if len(reports) != 1 || !strings.Contains(reports[0].Query, "resolved_at=is.null") ||
		!strings.Contains(reports[0].Body, `"resolution":"removed"`) {
		t.Fatalf("reports not resolved: %+v", reports)
	}
	recorded := fake.requests(http.MethodPost, "moderation_decisions")
	if len(recorded) != 1 || !strings.Contains(recorded[0].Body, `"decision":"remove"`) {
		t.Fatalf("removal not audited as a removal: %+v", recorded)
	}
	if rpc := fake.requests(http.MethodPost, "rpc/refresh_instructor_ratings"); len(rpc) != 1 {
		t.Fatal("rating not recomputed after removal")
	}
}

func TestRemoveIsRefusedWhereItDoesNotApply(t *testing.T) {
	cases := []struct {
		name, status, actor, body string
		want                      int
	}{
		{"classifier", "approved", "ai", `{"action":"remove","reason":"x"}`, http.StatusForbidden},
		{"no reason", "approved", "human", `{"action":"remove"}`, http.StatusBadRequest},
		{"not published", "pending", "human", `{"action":"remove","reason":"x"}`, http.StatusConflict},
		{"already removed", "rejected", "human", `{"action":"remove","reason":"x"}`, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake, _, moderation := servers(t)
			decideFake(fake, tc.status)

			res := serve(moderation.HandleDecide, http.MethodPut, "/v1/admin/reviews/:id",
				"/v1/admin/reviews/"+testReviewID, tc.body, tc.actor)

			if res.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", res.Code, tc.want, res.Body.String())
			}
			if patches := fake.requests(http.MethodPatch, "reviews"); len(patches) != 0 {
				t.Fatalf("review written: %+v", patches)
			}
		})
	}
}

func TestReportsCarryTheReviewTheyAreAbout(t *testing.T) {
	fake, _, moderation := servers(t)
	fake.on(http.MethodGet, "review_reports", `[{"id":3,"review_id":"`+testReviewID+`","reason":"defamatory",
		"detail":null,"created_at":"2026-09-01T00:00:00Z",
		"reviews":{"instructor_id":7,"course_code":"CMSC132","term":202508,"rating":1,
		"title":"t","body":"the text","status":"approved","submitted_at":"2026-08-01T00:00:00Z"}}]`)
	fake.on(http.MethodGet, "instructors", `[{"id":7,"slug":"shane-walsh","name":"Shane Walsh"}]`)

	res := serve(moderation.HandleReports, http.MethodGet, "/v1/admin/reports", "/v1/admin/reports", "", "human")

	var payload struct {
		Reports []struct {
			Instructor string `json:"instructor"`
			Review     struct {
				Body string `json:"body"`
			} `json:"review"`
		} `json:"reports"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil || len(payload.Reports) != 1 {
		t.Fatalf("bad payload %q: %v", res.Body.String(), err)
	}
	if payload.Reports[0].Instructor != "Shane Walsh" || payload.Reports[0].Review.Body != "the text" {
		t.Fatalf("report missing its review: %+v", payload.Reports[0])
	}
	if selects := fake.requests(http.MethodGet, "review_reports"); !strings.Contains(selects[0].Query, "reviews%28") {
		t.Fatalf("reports query does not embed the review: %s", selects[0].Query)
	}
}

func TestDismissingAReportClosesOnlyThatReport(t *testing.T) {
	fake, _, moderation := servers(t)
	fake.on(http.MethodPatch, "review_reports", `[{"id":3}]`)

	res := serve(moderation.HandleResolveReport, http.MethodPost, "/v1/admin/reports/:id", "/v1/admin/reports/3", "", "alice")

	if res.Code != http.StatusOK || decodeBody(t, res)["changed"] != true {
		t.Fatalf("status = %d: %s", res.Code, res.Body.String())
	}
	patches := fake.requests(http.MethodPatch, "review_reports")
	if len(patches) != 1 || !strings.Contains(patches[0].Query, "id=eq.3") ||
		!strings.Contains(patches[0].Body, `"resolution":"dismissed"`) ||
		!strings.Contains(patches[0].Body, `"resolved_by":"alice"`) {
		t.Fatalf("dismissal wrote the wrong thing: %+v", patches)
	}

	bad := serve(moderation.HandleResolveReport, http.MethodPost, "/v1/admin/reports/:id", "/v1/admin/reports/abc", "", "alice")
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("malformed id answered %d", bad.Code)
	}
}

/* ====================== sweep: grade matview refresh ==================== */

func TestSweepRefreshesStaleGradeMatviews(t *testing.T) {
	fake, _, moderation := servers(t)
	fake.on(http.MethodPost, "rpc/refresh_grade_matviews_if_stale", `true`)

	res := serve(moderation.HandleSweep, http.MethodPost, "/v1/admin/sweep", "/v1/admin/sweep", "", "human")

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Code, res.Body.String())
	}
	if body := decodeBody(t, res); body["grades_refreshed"] != true {
		t.Fatalf("grades_refreshed = %v", body["grades_refreshed"])
	}
	if calls := fake.requests(http.MethodPost, "rpc/refresh_grade_matviews_if_stale"); len(calls) != 1 {
		t.Fatalf("refresh called %d times", len(calls))
	}
}

func TestSweepReportsAFailedGradeRefresh(t *testing.T) {
	fake, _, moderation := servers(t)
	fake.onFunc(http.MethodPost, "rpc/refresh_grade_matviews_if_stale", func(recordedRequest) (int, string) {
		return http.StatusInternalServerError, `{"message":"canceling statement due to statement timeout"}`
	})

	res := serve(moderation.HandleSweep, http.MethodPost, "/v1/admin/sweep", "/v1/admin/sweep", "", "human")

	if res.Code != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207", res.Code)
	}
	if failures, _ := decodeBody(t, res)["failures"].(map[string]any); failures["grade_matviews"] == nil {
		t.Fatalf("failure not named: %s", res.Body.String())
	}
}

/* ========================== request logging ============================= */

// The verification token is a path segment. Logging the raw path wrote a live
// bearer credential into the log sink on every confirmation.
func TestRequestLogCarriesNoTokenAndAPepperedIP(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(io.Discard) })

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(requestLogger("test-pepper"))
	r.GET("/v1/reviews/verify/:token", func(ctx *gin.Context) { ctx.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/v1/reviews/verify/SECRET-TOKEN-VALUE", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	r.ServeHTTP(httptest.NewRecorder(), req)

	line := buf.String()
	if strings.Contains(line, "SECRET-TOKEN-VALUE") {
		t.Fatalf("token written to the log: %q", line)
	}
	if !strings.Contains(line, "/v1/reviews/verify/:token") {
		t.Fatalf("route template missing: %q", line)
	}
	if !strings.Contains(line, "ip="+hashOpaque("203.0.113.9", "test-pepper")[:12]) {
		t.Fatalf("ip not logged as the peppered hash: %q", line)
	}
}

func TestRequestLogOmitsTheIPWithoutAPepper(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(io.Discard) })

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(requestLogger(""))
	r.GET("/v1/courses", func(ctx *gin.Context) { ctx.Status(http.StatusOK) })
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/courses", nil))

	if strings.Contains(buf.String(), "ip=") {
		t.Fatalf("an unpeppered IP hash is reversible and must not be logged: %q", buf.String())
	}
}

/* ============================ grade reads =============================== */

func gradesRouter(t *testing.T) (*fakePostgREST, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	fake := newFakePostgREST(t)
	client := SupabaseClient{
		Url:         fake.server.URL,
		Key:         "anon",
		cache:       NewLRUCache(defaultCacheCapacity),
		courseCache: NewLRUCache(courseCacheCapacity),
	}
	r := gin.New()
	r.GET("/v1/grades", client.handleGetGrades)
	r.GET("/v1/grades/summary", client.handleGetGradeSummary)
	return fake, r
}

func get(r *gin.Engine, target string) *httptest.ResponseRecorder {
	res := httptest.NewRecorder()
	r.ServeHTTP(res, httptest.NewRequest(http.MethodGet, target, nil))
	return res
}

// A professor's section list should be the sections their summary counts. By
// default it also included the `course` tier -- carried from elsewhere in the
// course, and the least reliable attribution there is.
func TestInstructorGradeSectionsDefaultToTheSummaryTiers(t *testing.T) {
	fake, r := gradesRouter(t)

	get(r, "/v1/grades?instructorSlug=shane-walsh")
	get(r, "/v1/grades?instructorSlug=shane-walsh&instructorSource=reported,lead,testudo,course")
	get(r, "/v1/grades?courseCodes=CMSC132")

	calls := fake.requests(http.MethodGet, "grades")
	if len(calls) != 3 {
		t.Fatalf("expected 3 upstream calls, got %d", len(calls))
	}
	if !strings.Contains(calls[0].Query, "instructor_source=in.%28reported%2Clead%2Ctestudo%29") {
		t.Errorf("instructor filter did not default the tiers: %s", calls[0].Query)
	}
	if !strings.Contains(calls[1].Query, "course%29") {
		t.Errorf("an explicit instructorSource was overridden: %s", calls[1].Query)
	}
	if strings.Contains(calls[2].Query, "instructor_source") {
		t.Errorf("a course-only query gained a tier filter: %s", calls[2].Query)
	}
}

// The two summary groupings still computed per request are refused unnarrowed,
// before anything reaches the database.
func TestUnnarrowedPerRequestSummariesAreRefused(t *testing.T) {
	fake, r := gradesRouter(t)

	for _, target := range []string{
		"/v1/grades/summary?groupBy=instructorTerm",
		"/v1/grades/summary?groupBy=instructor&includeCarried=true",
	} {
		if res := get(r, target); res.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d, want 400", target, res.Code)
		}
	}
	if calls := fake.requests(http.MethodGet, "instructor_term_grades"); len(calls) != 0 {
		t.Fatal("an unnarrowed request reached the database")
	}

	for _, target := range []string{
		"/v1/grades/summary?groupBy=instructorTerm&instructorSlug=shane-walsh",
		"/v1/grades/summary?groupBy=instructor&includeCarried=true&courseCodes=CMSC132",
		"/v1/grades/summary?groupBy=instructor&includeCarried=true&instructorId=7",
		// Materialized, so unfiltered is fine.
		"/v1/grades/summary?groupBy=course&sortBy=gpa.desc",
		"/v1/grades/summary?groupBy=instructor",
	} {
		if res := get(r, target); res.Code != http.StatusOK {
			t.Errorf("%s answered %d, want 200: %s", target, res.Code, res.Body.String())
		}
	}
}
