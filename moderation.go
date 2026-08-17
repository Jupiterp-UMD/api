package main

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
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

	out := make([]enriched, 0, len(items))
	for _, item := range items {
		row := enriched{queueItem: item}

		var instructors []instructorRow
		if err := m.write.Select("instructors", eqSelect("id", fmt.Sprintf("%d", item.InstructorID)), &instructors); err == nil && len(instructors) > 0 {
			row.Instructor = instructors[0].Name
		}

		decisionParams := url.Values{}
		decisionParams.Set("select", "decision,decided_by,actor,confidence,categories,reason,applied,created_at")
		decisionParams.Set("review_id", "eq."+item.ID)
		decisionParams.Set("order", "created_at.desc")
		decisionParams.Set("limit", "1")

		var decisions []map[string]any
		if err := m.write.Select("moderation_decisions", decisionParams, &decisions); err == nil && len(decisions) > 0 {
			row.LastDecision = decisions[0]
		}

		out = append(out, row)
	}

	ctx.JSON(http.StatusOK, gin.H{"reviews": out, "count": len(out)})
}

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
		m.recordDecision(reviewID, req, decidedBy, false)
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

	m.recordDecision(reviewID, req, decidedBy, apply)

	if !apply {
		// Recorded, not acted on. The review still needs a person, so make
		// that explicit rather than leaving it pending until the sweeper
		// notices.
		m.setStatus(reviewID, "escalated", decidedBy, "")
		m.triage.notifyDiscord(reviewID, req.Action,
			derefFloat(req.Confidence), req.Categories,
			"shadow mode: recorded but not applied")
		ctx.JSON(http.StatusOK, gin.H{
			"status":  "escalated",
			"applied": false,
			"note":    "recorded for comparison; a human decides while shadow mode is on",
		})
		return
	}

	moderator := decidedBy
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

	ctx.JSON(http.StatusOK, gin.H{"status": targetStatus, "applied": true, "changed": true})
}

func (m *ModerationServer) recordDecision(reviewID string, req DecisionRequest, decidedBy string, applied bool) {
	actor := decidedBy
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
	params := url.Values{}
	params.Set("select", "recipient,payload")
	params.Set("review_id", "eq."+review.ID)
	params.Set("template", "eq.verify")
	params.Set("limit", "1")

	var rows []outboxRow
	if err := m.write.Select("email_outbox", params, &rows); err != nil || len(rows) == 0 {
		return
	}
	// The address is nulled once the verification mail is sent, so this only
	// reaches people whose rejection happened before that. Deliberate: keeping
	// addresses around longer to enable rejection mail would undo the decision
	// not to store them.
	if rows[0].Recipient == nil || *rows[0].Recipient == "" {
		return
	}
	name, _ := rows[0].Payload["instructor_name"].(string)
	if reason == "" {
		reason = "It did not meet the content policy."
	}
	if err := m.email.Queue(review.ID, *rows[0].Recipient, "rejected", map[string]any{
		"instructor_name": name,
		"reason":          reason,
	}); err != nil {
		log.Printf("moderation: queueing rejection email failed: %v", err)
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
	retried, escalated := m.triage.Sweep()
	purged := m.triage.PurgeAbandoned()
	sent, err := m.email.Flush(50)
	if err != nil {
		log.Printf("sweep: email flush failed: %v", err)
	}

	// Scalar, not an array: `refresh_instructor_ratings` returns `integer` and
	// is not set-returning, so PostgREST sends a bare number. Decoding into
	// []int failed on every sweep -- logged and non-fatal, so the nightly
	// rating recompute never ran and nothing said so.
	var ratingsUpdated *int
	if err := m.write.RPC("refresh_instructor_ratings", map[string]any{}, &ratingsUpdated); err != nil {
		log.Printf("sweep: rating refresh failed: %v", err)
	}

	updated := 0
	if ratingsUpdated != nil {
		updated = *ratingsUpdated
	}

	ctx.JSON(http.StatusOK, gin.H{
		"triage_retried":   retried,
		"triage_escalated": escalated,
		"purged":           purged,
		"emails_sent":      sent,
		"ratings_updated":  updated,
	})
}

/* ============================== public read ============================= */

// HandleListReviews serves approved reviews for one professor.
//
// Reads `public_reviews`, which is a view over approved rows that does not
// expose the identity columns at all. The anon key has no grant on `reviews`
// itself, so a mistake here cannot leak an unapproved review.
func (client SupabaseClient) HandleListReviews(ctx *gin.Context) {
	path := "v1/reviews"

	slug := ctx.Query("instructorSlug")
	if slug == "" {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "instructorSlug is required"})
		return
	}

	params := url.Values{}
	params.Set("select", "*")
	params.Set("instructor_slug", "eq."+slug)
	params.Set("order", "submitted_at.desc")
	params.Set("limit", ctx.DefaultQuery("limit", "25"))
	params.Set("offset", ctx.DefaultQuery("offset", "0"))
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

func derefFloat(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}
