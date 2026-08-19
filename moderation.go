package main

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// The moderation surface: the human queue, and the endpoint the automated
// triage calls back on.
//
// Both callers share one route with different keys. The endpoint is harder
// than a human-only one would need to be, because a machine caller retries,
// races, and is on the public internet:
//
//   - idempotent, so a retry is a no-op rather than a second audit row;
//   - state-guarded, so a late retry cannot overturn a human's decision;
//   - always audited, including which key was used.

type ModerationServer struct {
	cfg    *Config
	write  *WriteClient
	email  *EmailSender
	triage *TriageClient
}

func NewModerationServer(cfg *Config, write *WriteClient, email *EmailSender, triage *TriageClient) *ModerationServer {
	return &ModerationServer{cfg: cfg, write: write, email: email, triage: triage}
}

/* ================================ queue ================================= */

type queueItem struct {
	ID            string  `json:"id"`
	InstructorID  int64   `json:"instructor_id"`
	CourseCode    *string `json:"course_code"`
	Term          *int    `json:"term"`
	Rating        float64 `json:"rating"`
	ExpectedGrade *string `json:"expected_grade"`
	Title         *string `json:"title"`
	Body          *string `json:"body"`
	Status        string  `json:"status"`
	SubmittedAt   string  `json:"submitted_at"`
	VerifiedAt    *string `json:"verified_at"`
	EmailDomain   string  `json:"email_domain"`
}

// HandleQueue lists reviews awaiting a decision, newest first.
//
// Each row carries the classifier's most recent opinion alongside the content.
// A moderator who sees "escalated: possible misconduct allegation, confidence
// 0.71" triages far faster than one reading cold, and during shadow mode this
// is how disagreements with the classifier become visible while they still
// cost nothing.
func (m *ModerationServer) HandleQueue(ctx *gin.Context) {
	status := ctx.DefaultQuery("status", "pending,escalated")

	params := url.Values{}
	// Never `select=*`: email_hash and the forensics hashes have no business
	// on a moderator's screen, and a queue endpoint that returns them is one
	// misconfigured admin key away from being the leak.
	params.Set("select", "id,instructor_id,course_code,term,rating,expected_grade,"+
		"title,body,status,submitted_at,verified_at,email_domain")
	params.Set("status", "in.("+status+")")
	params.Set("order", "submitted_at.asc")
	params.Set("limit", ctx.DefaultQuery("limit", "50"))

	var items []queueItem
	if err := m.write.Select("reviews", params, &items); err != nil {
		sendInternalError(ctx, "v1/admin/reviews", err)
		return
	}

	type enriched struct {
		queueItem
		Instructor   string         `json:"instructor"`
		LastDecision map[string]any `json:"last_decision"`
	}

	// Two batched lookups, not two per row.
	//
	// This loop used to issue one instructor query and one decision query for
	// every review it returned: 101 sequential PostgREST round trips behind a
	// single moderator page load at the default limit, growing linearly with
	// the queue. Both are now `in.(...)` lookups joined in memory, so the
	// handler costs three requests regardless of queue depth.
	names := m.instructorNames(items)
	decisions := m.latestDecisions(items)

	out := make([]enriched, 0, len(items))
	for _, item := range items {
		out = append(out, enriched{
			queueItem:    item,
			Instructor:   names[item.InstructorID],
			LastDecision: decisions[item.ID],
		})
	}

	ctx.JSON(http.StatusOK, gin.H{"reviews": out, "count": len(out)})
}

// instructorNames resolves every instructor named in the queue in one request.
func (m *ModerationServer) instructorNames(items []queueItem) map[int64]string {
	names := make(map[int64]string, len(items))
	if len(items) == 0 {
		return names
	}

	seen := make(map[int64]struct{}, len(items))
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if _, dup := seen[item.InstructorID]; dup {
			continue
		}
		seen[item.InstructorID] = struct{}{}
		ids = append(ids, strconv.FormatInt(item.InstructorID, 10))
	}

	params := url.Values{}
	params.Set("select", "id,slug,name")
	params.Set("id", "in.("+strings.Join(ids, ",")+")")

	var rows []instructorRow
	if err := m.write.Select("instructors", params, &rows); err != nil {
		log.Printf("moderation: batch instructor lookup failed: %v", err)
		return names
	}
	for _, row := range rows {
		names[row.ID] = row.Name
	}
	return names
}

// latestDecisions returns the most recent decision per review, in one request.
//
// PostgREST cannot express "latest per group", so this fetches the decisions
// for these reviews newest-first and keeps the first one seen for each. The
// per-review cap is what bounds the response: a review that has been through
// triage several times has a handful of rows, not an unbounded history.
func (m *ModerationServer) latestDecisions(items []queueItem) map[string]map[string]any {
	latest := make(map[string]map[string]any, len(items))
	if len(items) == 0 {
		return latest
	}

	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}

	params := url.Values{}
	params.Set("select", "review_id,decision,decided_by,actor,confidence,categories,reason,applied,created_at")
	params.Set("review_id", "in.("+strings.Join(ids, ",")+")")
	params.Set("order", "created_at.desc")
	params.Set("limit", strconv.Itoa(len(ids)*decisionsPerReviewCap))

	var rows []map[string]any
	if err := m.write.Select("moderation_decisions", params, &rows); err != nil {
		log.Printf("moderation: batch decision lookup failed: %v", err)
		return latest
	}
	for _, row := range rows {
		reviewID, _ := row["review_id"].(string)
		if reviewID == "" {
			continue
		}
		if _, have := latest[reviewID]; have {
			continue
		}
		latest[reviewID] = row
	}
	return latest
}

// How many decision rows to allow for per review when batching. Generous
// enough that the newest is always in the window.
const decisionsPerReviewCap = 8

/* =============================== decide ================================= */

type DecisionRequest struct {
	Action        string   `json:"action" binding:"required,oneof=approve reject escalate"`
	Reason        string   `json:"reason"`
	Confidence    *float64 `json:"confidence"`
	Categories    []string `json:"categories"`
	PolicyVersion string   `json:"policy_version"`
	Model         string   `json:"model"`
}

// HandleDecide applies or records a moderation decision.
func (m *ModerationServer) HandleDecide(ctx *gin.Context) {
	reviewID := ctx.Param("id")
	actorKind, _ := ctx.Get("actor")
	decidedBy, _ := actorKind.(string)
	// Who, as distinct from what kind. Falls back to decidedBy so a deployment
	// with only the shared REVIEW_ADMIN_KEY behaves exactly as before.
	moderatorName := ctx.GetString("moderator")
	if moderatorName == "" {
		moderatorName = decidedBy
	}

	var req DecisionRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{
			"error": "action must be one of approve, reject, escalate",
		})
		return
	}

	var reviews []reviewRow
	if err := m.write.Select("reviews", eqSelect("id", reviewID), &reviews); err != nil {
		sendInternalError(ctx, "v1/admin/reviews/:id", err)
		return
	}
	if len(reviews) == 0 {
		ctx.JSON(http.StatusNotFound, gin.H{"error": "no such review"})
		return
	}
	review := reviews[0]

	targetStatus := map[string]string{
		"approve":  "approved",
		"reject":   "rejected",
		"escalate": "escalated",
	}[req.Action]

	// Idempotency. A retry that asks for the state the review is already in is
	// a success, not a second audit row claiming a decision was applied twice.
	if review.Status == targetStatus {
		ctx.JSON(http.StatusOK, gin.H{"status": review.Status, "changed": false})
		return
	}

	// State guard. Only pending and escalated reviews are decidable. A late
	// retry must not silently overturn what a human already concluded.
	if review.Status != "pending" && review.Status != "escalated" {
		m.recordDecision(reviewID, req, decidedBy, moderatorName, false)
		ctx.JSON(http.StatusConflict, gin.H{
			"error":  "review is " + review.Status + " and is no longer awaiting a decision",
			"status": review.Status,
		})
		return
	}

	// Shadow mode. The classifier's opinion is recorded but not applied until
	// the corresponding gate is opened, which is why enabling automation is a
	// config change rather than a code change.
	apply := true
	if decidedBy == "ai" {
		switch req.Action {
		case "approve":
			apply = m.cfg.AutoApprove &&
				req.Confidence != nil && *req.Confidence >= m.cfg.AutoApproveMinConf &&
				len(req.Categories) == 0
		case "reject":
			apply = m.cfg.AutoReject &&
				req.Confidence != nil && *req.Confidence >= m.cfg.AutoRejectMinConf
		case "escalate":
			apply = true
		}
	}

	m.recordDecision(reviewID, req, decidedBy, moderatorName, apply)

	if !apply {
		// Recorded, not acted on. The review still needs a person, so make
		// that explicit rather than leaving it pending until the sweeper
		// notices.
		m.setStatus(reviewID, "escalated", decidedBy, "")
		m.triage.notifyDiscord(reviewID, req.Action,
			derefFloat(req.Confidence), req.Categories,
			shadowModeReason(req.Action, req.Confidence))
		ctx.JSON(http.StatusOK, gin.H{
			"status":  "escalated",
			"applied": false,
			"note":    "recorded for comparison; a human decides while shadow mode is on",
		})
		return
	}

	moderator := moderatorName
	if req.Model != "" {
		moderator = req.Model
	}
	m.setStatus(reviewID, targetStatus, moderator, req.Reason)

	if req.Action == "reject" {
		m.notifyRejection(review, req.Reason)
	}
	if req.Action == "escalate" {
		m.triage.notifyDiscord(reviewID, "escalate",
			derefFloat(req.Confidence), req.Categories, req.Reason)
	}

	// Approve and reject are terminal: neither generates further mail, so the
	// reviewer's address is dropped here. Escalate is not terminal -- the
	// review is still headed for a decision that may need to notify them.
	if req.Action == "approve" || req.Action == "reject" {
		m.email.PurgeContact(reviewID)
	}

	ctx.JSON(http.StatusOK, gin.H{"status": targetStatus, "applied": true, "changed": true})
}

func (m *ModerationServer) recordDecision(reviewID string, req DecisionRequest, decidedBy, moderatorName string, applied bool) {
	// `decided_by` stays coarse -- "human" or "ai" -- because that is what the
	// shadow-mode gates key off. `actor` is the specific one: a model name when
	// a classifier decided, otherwise the named moderator.
	actor := moderatorName
	if req.Model != "" {
		actor = req.Model
	}
	policy := req.PolicyVersion
	if policy == "" {
		policy = PolicyVersion
	}

	row := map[string]any{
		"review_id":      reviewID,
		"decision":       req.Action,
		"decided_by":     decidedBy,
		"actor":          actor,
		"policy_version": policy,
		"categories":     req.Categories,
		"reason":         req.Reason,
		"applied":        applied,
	}
	if req.Confidence != nil {
		row["confidence"] = *req.Confidence
	}
	if err := m.write.Insert("moderation_decisions", []any{row}, nil); err != nil {
		log.Printf("moderation: recording decision for %s failed: %v", reviewID, err)
	}
}

func (m *ModerationServer) setStatus(reviewID, status, moderator, reason string) {
	patch := map[string]any{
		"status":       status,
		"moderated_at": time.Now().UTC().Format(time.RFC3339),
		"moderator":    moderator,
	}
	if reason != "" {
		patch["reject_reason"] = reason
	}
	params := url.Values{}
	params.Set("id", "eq."+reviewID)
	params.Set("status", "in.(pending,escalated)")

	if err := m.write.Update("reviews", params, patch, nil); err != nil {
		log.Printf("moderation: setting %s on %s failed: %v", status, reviewID, err)
	}
}

// notifyRejection emails the reviewer, with an appeal route.
//
// A rejection with no explanation and no way to contest it is how a moderation
// system loses the people who were writing good reviews.
func (m *ModerationServer) notifyRejection(review reviewRow, reason string) {
	// The address survives until the review reaches a terminal state, which is
	// exactly this moment. Previously it was destroyed when the verification
	// mail was sent -- always before any rejection could happen -- so this
	// function returned early every time and the `rejected` template was
	// unreachable code.
	recipient := m.email.RecipientFor(review.ID)
	if recipient == "" {
		return
	}

	name := ""
	var instructors []instructorRow
	if err := m.write.Select("instructors", eqSelect("id", fmt.Sprintf("%d", review.InstructorID)), &instructors); err == nil && len(instructors) > 0 {
		name = instructors[0].Name
	}

	if reason == "" {
		reason = "It did not meet the content policy."
	}
	if err := m.email.Queue(review.ID, recipient, "rejected", map[string]any{
		"instructor_name": name,
		"reason":          reason,
	}); err != nil {
		log.Printf("moderation: queueing rejection email failed: %v", err)
	}
	// Deliver before the purge below removes the address.
	if _, err := m.email.Flush(5); err != nil {
		log.Printf("moderation: flushing rejection email failed: %v", err)
	}
}

/* ============================== reports ================================= */

// HandleReports lists open reports against published reviews.
func (m *ModerationServer) HandleReports(ctx *gin.Context) {
	params := url.Values{}
	params.Set("select", "id,review_id,reason,detail,created_at")
	params.Set("resolved_at", "is.null")
	params.Set("order", "created_at.asc")
	params.Set("limit", "100")

	var reports []map[string]any
	if err := m.write.Select("review_reports", params, &reports); err != nil {
		sendInternalError(ctx, "v1/admin/reports", err)
		return
	}
	ctx.JSON(http.StatusOK, gin.H{"reports": reports, "count": len(reports)})
}

/* ============================== maintenance ============================= */

// HandleSweep runs the scheduled maintenance passes.
//
// Exposed as an admin route rather than an in-process ticker because Cloud Run
// scales to zero: a ticker in a container that is not running does not tick.
// Cloud Scheduler calls this.
func (m *ModerationServer) HandleSweep(ctx *gin.Context) {
	// Component failures are reported, not just logged.
	//
	// This handler used to answer 200 with a body of counts even when every
	// step inside it had failed. That is the shape of most of the bugs this
	// service has had: the rating recompute failing on a type error, the
	// matview refresh failing on ownership, PostgREST scalars failing to
	// decode. Each ran broken for weeks because the only signal was a log line
	// nobody was watching, and the scheduler saw a success either way.
	//
	// Now a partial failure answers 207 and names what broke, so Cloud
	// Scheduler's own alerting is enough to surface it.
	failures := map[string]string{}

	retried, escalated := m.triage.Sweep()
	purged := m.triage.PurgeAbandoned()

	sent, err := m.email.Flush(50)
	if err != nil {
		log.Printf("sweep: email flush failed: %v", err)
		failures["email_flush"] = err.Error()
	}

	// Scalar, not an array: `refresh_instructor_ratings` returns `integer` and
	// is not set-returning, so PostgREST sends a bare number. Decoding into
	// []int failed on every sweep -- logged and non-fatal, so the nightly
	// rating recompute never ran and nothing said so.
	var ratingsUpdated *int
	if err := m.write.RPC("refresh_instructor_ratings", map[string]any{}, &ratingsUpdated); err != nil {
		log.Printf("sweep: rating refresh failed: %v", err)
		failures["rating_refresh"] = err.Error()
	} else if ratingsUpdated == nil {
		// A null where an integer was promised means the function did not
		// return what this code expects, which is the same class of silent
		// breakage as an outright error.
		log.Printf("sweep: rating refresh returned no count")
		failures["rating_refresh"] = "returned no count"
	}

	updated := 0
	if ratingsUpdated != nil {
		updated = *ratingsUpdated
	}

	body := gin.H{
		"ok":               len(failures) == 0,
		"triage_retried":   retried,
		"triage_escalated": escalated,
		"purged":           purged,
		"emails_sent":      sent,
		"ratings_updated":  updated,
	}
	if len(failures) > 0 {
		body["failures"] = failures
		ctx.JSON(http.StatusMultiStatus, body)
		return
	}
	ctx.JSON(http.StatusOK, body)
}

/* ============================== public read ============================= */

// HandleListReviews serves approved reviews for one professor.
//
// Reads `public_reviews`, which is a view over approved rows that does not
// expose the identity columns at all. The anon key has no grant on `reviews`
// itself, so a mistake here cannot leak an unapproved review.
// ReviewListArgs bounds the public review listing, matching the limits every
// other read endpoint already enforces.
type ReviewListArgs struct {
	Limit  uint16 `form:"limit"  binding:"omitempty,min=1,max=500"`
	Offset uint16 `form:"offset"`
}

func (client SupabaseClient) HandleListReviews(ctx *gin.Context) {
	path := "v1/reviews"

	slug := ctx.Query("instructorSlug")
	if slug == "" {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "instructorSlug is required"})
		return
	}

	// Bound the pagination.
	//
	// These used to be forwarded to PostgREST as raw strings while every other
	// read handler bound them into a uint16 capped at 500. On a public,
	// unauthenticated endpoint that is both an unbounded query and an unbounded
	// cache-key space: each distinct limit/offset pair mints a new LRU entry,
	// so a caller could evict the whole 4096-entry cache at will.
	var page ReviewListArgs
	if err := ctx.ShouldBindQuery(&page); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{
			"error": "limit must be between 1 and 500, and offset a non-negative integer",
		})
		return
	}
	if page.Limit == 0 {
		page.Limit = 25
	}

	params := url.Values{}
	params.Set("select", "*")
	params.Set("instructor_slug", "eq."+slug)
	params.Set("order", "submitted_at.desc")
	params.Set("limit", strconv.FormatUint(uint64(page.Limit), 10))
	params.Set("offset", strconv.FormatUint(uint64(page.Offset), 10))
	if course := ctx.Query("courseCode"); course != "" {
		params.Set("course_code", "eq."+strings.ToUpper(course))
	}

	key := buildCacheKey(ctx.Request)
	// Same 60s as the write below, for the same reason: a newly approved review
	// should not be held back from a reader for longer than the service holds
	// it itself.
	if client.serveFromCache(ctx, path, key, reviewsTTL) {
		return
	}

	res, err := client.requestWithPrefer("public_reviews", params.Encode(), preferCount(true))
	if err != nil {
		sendInternalError(ctx, path, err)
		return
	}

	// Deliberately short. The cache is per Cloud Run instance with no
	// cross-instance invalidation, so a newly approved review would otherwise
	// appear on one refresh and vanish on the next depending on which instance
	// answered.
	client.writeAndCacheResponse(ctx, res, path, key, reviewsTTL)
}

// shadowModeReason explains, in the alert itself, why a decision was made and
// then not acted on.
//
// This used to read "shadow mode: recorded but not applied", which is accurate
// and tells a reader nothing: it names the mechanism without saying what the
// classifier concluded or what the reader is expected to do. Someone seeing it
// in a channel cannot tell whether something went wrong.
//
// Confidence is omitted rather than printed as 0.00 when the caller did not
// supply one -- a decision with no confidence is different from one the model
// was certain was worthless, and the two should not look alike.
func shadowModeReason(action string, confidence *float64) string {
	if confidence == nil {
		return fmt.Sprintf(
			"classifier said %s — shadow mode is on, so it was not applied", action)
	}
	return fmt.Sprintf(
		"classifier said %s (%.2f) — shadow mode is on, so it was not applied",
		action, *confidence)
}

func derefFloat(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}
