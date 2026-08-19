package main

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

/* ================================ types ================================= */

type SubmitReviewRequest struct {
	InstructorSlug string   `json:"instructor_slug" binding:"required"`
	CourseCode     string   `json:"course_code"`
	Term           *int     `json:"term"`
	Rating         *float64 `json:"rating" binding:"required"`
	ExpectedGrade  string   `json:"expected_grade"`
	Title          string   `json:"title"`
	Body           string   `json:"body"`
	Email          string   `json:"email" binding:"required"`
	CaptchaToken   string   `json:"captcha_token"`
}

type reviewRow struct {
	ID           string  `json:"id"`
	InstructorID int64   `json:"instructor_id"`
	CourseCode   *string `json:"course_code"`
	Term         *int    `json:"term"`
	Rating       float64 `json:"rating"`
	Title        *string `json:"title"`
	Body         *string `json:"body"`
	Status       string  `json:"status"`
	EditKeyHash  string  `json:"edit_key_hash"`
	SubmittedAt  string  `json:"submitted_at"`
}

type instructorRow struct {
	ID   int64  `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type tokenRow struct {
	TokenHash string  `json:"token_hash"`
	ReviewID  string  `json:"review_id"`
	Purpose   string  `json:"purpose"`
	ExpiresAt string  `json:"expires_at"`
	UsedAt    *string `json:"used_at"`
}

/* =============================== validation ============================= */

var (
	courseCodeRe = regexp.MustCompile(`^[A-Z]{4}\d{3}[A-Z]?$`)
	emailRe      = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

	// Zero-width and bidirectional-override characters. These are invisible
	// and are used to smuggle content past both moderators and classifiers --
	// a slur split by zero-width joiners reads normally and matches nothing.
	invisibleRe = regexp.MustCompile(`[\x{200B}-\x{200F}\x{202A}-\x{202E}\x{2060}-\x{206F}\x{FEFF}]`)

	// C0 and C1 control characters, except tab and newline.
	controlRe = regexp.MustCompile(`[\x{0000}-\x{0008}\x{000B}\x{000C}\x{000E}-\x{001F}\x{007F}-\x{009F}]`)
)

// sanitizeText strips what should never reach moderation or the page.
//
// Done on ingest rather than on display, so that what a moderator reads is
// exactly what a reader would see. Sanitising at render time instead means the
// moderator approves one string and the site publishes another.
func sanitizeText(value string) string {
	value = invisibleRe.ReplaceAllString(value, "")
	value = controlRe.ReplaceAllString(value, "")
	// Any HTML is stripped rather than escaped: reviews are plain text, and
	// there is no case where a reviewer needs markup.
	value = strings.ReplaceAll(value, "<", "")
	value = strings.ReplaceAll(value, ">", "")
	return strings.TrimSpace(value)
}

// validRating enforces 1-5 in half steps.
//
// Checked here as well as in the database so that a typo produces a clear
// message instead of a 500 from a constraint violation.
func validRating(r float64) bool {
	return r >= 1 && r <= 5 && r*2 == float64(int(r*2))
}

// validTerm accepts real Fall and Spring terms only.
//
// The grade dataset covers Fall and Spring, permanently -- there is no plan to
// request Winter or Summer. Accepting a Summer term here would let a reviewer
// file against a term the rest of the site cannot represent.
func validTerm(term int, now time.Time) bool {
	year, month := term/100, term%100
	if year < 2000 || year > now.Year()+1 {
		return false
	}
	return month == 1 || month == 8
}

func (c *Config) emailDomainAllowed(email string) (string, bool) {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return "", false
	}
	domain := strings.ToLower(email[at+1:])
	for _, allowed := range c.AllowedEmailDomains {
		if domain == allowed {
			return domain, true
		}
	}
	return domain, false
}

/* ================================ server ================================ */

// ReviewServer holds everything the /v1 review routes need.
type ReviewServer struct {
	cfg    *Config
	write  *WriteClient
	email  *EmailSender
	triage *TriageClient
}

func NewReviewServer(cfg *Config, write *WriteClient, email *EmailSender, triage *TriageClient) *ReviewServer {
	return &ReviewServer{cfg: cfg, write: write, email: email, triage: triage}
}

/* ================================ submit ================================ */

// HandleSubmit accepts a review and emails a confirmation link.
//
// Every check below runs on every request, in this order. The response is
// identical whether or not the address has already reviewed this professor:
// a distinguishable "you already reviewed this" turns the endpoint into an
// oracle for "did person X review professor Y", which is exactly the privacy
// property the hashing is meant to provide.
func (s *ReviewServer) HandleSubmit(ctx *gin.Context) {
	var req SubmitReviewRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "malformed request body"})
		return
	}

	ip := clientIP(ctx)

	// 1. Captcha.
	ok, err := verifyTurnstile(s.cfg, req.CaptchaToken, ip)
	if err != nil {
		log.Printf("turnstile verification errored: %v", err)
		ctx.JSON(http.StatusServiceUnavailable, gin.H{"error": "could not verify captcha, try again"})
		return
	}
	if !ok {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "captcha verification failed"})
		return
	}

	// 2. Email shape and domain.
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !emailRe.MatchString(email) {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "that does not look like an email address"})
		return
	}
	domain, allowed := s.cfg.emailDomainAllowed(email)
	if !allowed {
		ctx.JSON(http.StatusBadRequest, gin.H{
			"error": "reviews are limited to " + strings.Join(s.cfg.AllowedEmailDomains, " and ") + " addresses",
		})
		return
	}

	emailHash := hashEmail(email, s.cfg.EmailPepper)

	// 3. Rate limits, before any expensive work.
	for _, check := range []struct {
		bucket string
		limit  RateLimit
	}{
		{"ip:" + hashOpaque(ip, s.cfg.EmailPepper), limitPerIP},
		{"email:" + emailHash, limitPerEmail},
	} {
		within, err := checkRateLimit(s.write, check.bucket, check.limit)
		if err != nil {
			log.Printf("rate limit check failed: %v", err)
			ctx.JSON(http.StatusServiceUnavailable, gin.H{"error": "try again shortly"})
			return
		}
		if !within {
			ctx.JSON(http.StatusTooManyRequests, gin.H{"error": "too many reviews submitted recently"})
			return
		}
	}

	// 4. Instructor exists.
	instructor, err := s.instructorBySlug(req.InstructorSlug)
	if err != nil {
		log.Printf("instructor lookup failed: %v", err)
		ctx.JSON(http.StatusServiceUnavailable, gin.H{"error": "try again shortly"})
		return
	}
	if instructor == nil {
		ctx.JSON(http.StatusNotFound, gin.H{"error": "no such professor"})
		return
	}

	// Per-instructor limit, which is the one that catches brigading.
	//
	// Fails closed, like the two above. It used to swallow the error and let
	// the request through, which disabled the anti-brigading control precisely
	// when the database was struggling -- the moment a brigade is most likely
	// to be what is causing the load.
	within, err := checkRateLimit(s.write, fmt.Sprintf("instructor:%d", instructor.ID), limitPerInstructor)
	if err != nil {
		log.Printf("per-instructor rate limit check failed for instructor %d: %v", instructor.ID, err)
		ctx.JSON(http.StatusServiceUnavailable, gin.H{"error": "try again shortly"})
		return
	}
	if !within {
		log.Printf("per-instructor rate limit hit for instructor %d", instructor.ID)
		ctx.JSON(http.StatusTooManyRequests, gin.H{"error": "too many reviews for this professor right now"})
		return
	}

	// 5. Course code, if given.
	courseCode := strings.ToUpper(strings.TrimSpace(req.CourseCode))
	if courseCode != "" && !courseCodeRe.MatchString(courseCode) {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "course code should look like CMSC132"})
		return
	}

	// 6. Term.
	if req.Term != nil && !validTerm(*req.Term, time.Now()) {
		ctx.JSON(http.StatusBadRequest, gin.H{
			"error": "term must be a past or current Fall or Spring term, like 202508",
		})
		return
	}

	// 7. Rating, on a half step.
	if !validRating(*req.Rating) {
		ctx.JSON(http.StatusBadRequest, gin.H{
			"error": "rating must be between 1 and 5 in half steps, like 3.5",
		})
		return
	}

	// 8. Content limits and sanitisation.
	title := sanitizeText(req.Title)
	body := sanitizeText(req.Body)
	if len([]rune(title)) > 120 {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "title is limited to 120 characters"})
		return
	}
	if len([]rune(body)) > 5000 {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "review is limited to 5000 characters"})
		return
	}

	if req.ExpectedGrade != "" && !validExpectedGrade(req.ExpectedGrade) {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "expected grade is not a valid grade"})
		return
	}

	// 9. Insert, mint a verification token, queue the email.
	verifyToken, err := newToken()
	if err != nil {
		sendInternalError(ctx, "v1/reviews", err)
		return
	}
	// A placeholder, because `edit_key_hash` is NOT NULL and there is nothing
	// to put there yet. The key the reviewer actually receives is minted in
	// HandleVerify and overwrites this. Random rather than a constant so that
	// an unverified row never shares a hash with any other.
	placeholderKey, err := newToken()
	if err != nil {
		sendInternalError(ctx, "v1/reviews", err)
		return
	}

	row := map[string]any{
		"instructor_id":   instructor.ID,
		"rating":          *req.Rating,
		"email_hash":      emailHash,
		"email_domain":    domain,
		"edit_key_hash":   hashToken(placeholderKey),
		"submit_ip_hash":  hashOpaque(ip, s.cfg.EmailPepper),
		"user_agent_hash": hashOpaque(ctx.GetHeader("User-Agent"), s.cfg.EmailPepper),
		"status":          "unverified",
	}
	if courseCode != "" {
		row["course_code"] = courseCode
	}
	if req.Term != nil {
		row["term"] = *req.Term
	}
	if title != "" {
		row["title"] = title
	}
	if body != "" {
		row["body"] = body
	}
	if req.ExpectedGrade != "" {
		row["expected_grade"] = req.ExpectedGrade
	}

	var inserted []reviewRow
	if err := s.write.Insert("reviews", []any{row}, &inserted); err != nil {
		// A unique-index violation means this address already has a live
		// review for this professor and course. Answer exactly as if it had
		// succeeded: see the note on this handler.
		if strings.Contains(err.Error(), "reviews_one_per_person") {
			ctx.JSON(http.StatusAccepted, gin.H{"status": "verification_sent"})
			return
		}
		log.Printf("review insert failed: %v", err)
		ctx.JSON(http.StatusServiceUnavailable, gin.H{"error": "try again shortly"})
		return
	}
	if len(inserted) == 0 {
		sendInternalError(ctx, "v1/reviews", fmt.Errorf("insert returned no rows"))
		return
	}
	review := inserted[0]

	tokenRowData := map[string]any{
		"token_hash": hashToken(verifyToken),
		"review_id":  review.ID,
		"purpose":    "verify",
		"expires_at": time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339),
	}
	if err := s.write.Insert("review_tokens", []any{tokenRowData}, nil); err != nil {
		log.Printf("verification token insert failed for review %s: %v", review.ID, err)
	}

	// The recipient stored on this row is what makes the reviewer contactable
	// later -- for their manage key on verification, and for a rejection
	// notice with an appeal route. It is purged by PurgeContact once the
	// review reaches a state that generates no further mail.
	if err := s.email.Queue(review.ID, email, "verify", map[string]any{
		"token":           verifyToken,
		"instructor_name": instructor.Name,
	}); err != nil {
		log.Printf("queueing verification email failed for review %s: %v", review.ID, err)
	}

	// Delivery does not block the response.
	go func() {
		if _, err := s.email.Flush(5); err != nil {
			log.Printf("email flush after submit failed: %v", err)
		}
	}()

	ctx.JSON(http.StatusAccepted, gin.H{"status": "verification_sent"})
}

func validExpectedGrade(grade string) bool {
	switch grade {
	case "A+", "A", "A-", "B+", "B", "B-", "C+", "C", "C-", "D+", "D", "D-", "F", "W", "Other":
		return true
	}
	return false
}

func (s *ReviewServer) instructorBySlug(slug string) (*instructorRow, error) {
	params := url.Values{}
	params.Set("select", "id,slug,name")
	params.Set("slug", "eq."+slug)
	params.Set("limit", "1")

	var rows []instructorRow
	if err := s.write.Select("instructors", params, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

/* ================================ verify ================================ */

// HandleVerify confirms an emailed link and moves the review to `pending`.
//
// This transition is the trigger point for automated triage. The workflow is
// never responsible for sending or awaiting the verification email itself:
// parking a workflow execution for hours holding the only copy of a
// submission's progress means an n8n restart during that window strands it.
// State lives in Postgres; n8n is told when the state changes.
//
// Idempotent on replay. Mail clients prefetch links, users double-click, and
// a second visit should show success rather than "invalid token".
func (s *ReviewServer) HandleVerify(ctx *gin.Context) {
	token := ctx.Param("token")
	if token == "" {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "missing token"})
		return
	}

	params := url.Values{}
	params.Set("select", "*")
	params.Set("token_hash", "eq."+hashToken(token))
	params.Set("purpose", "eq.verify")
	params.Set("limit", "1")

	var tokens []tokenRow
	if err := s.write.Select("review_tokens", params, &tokens); err != nil {
		sendInternalError(ctx, "v1/reviews/verify", err)
		return
	}
	if len(tokens) == 0 {
		ctx.JSON(http.StatusNotFound, gin.H{"error": "that link is not valid"})
		return
	}
	found := tokens[0]

	// An unparseable expiry is treated as expired, not as "no expiry".
	//
	// The previous `err == nil &&` guard meant a format the parser did not
	// recognise silently disabled the 48-hour window the email promises. If
	// this ever fires it means PostgREST changed its timestamp rendering,
	// which is worth a log line rather than a silently immortal token.
	expires, err := time.Parse(time.RFC3339, found.ExpiresAt)
	if err != nil {
		log.Printf("verify: unparseable expires_at %q on token for review %s: %v",
			found.ExpiresAt, found.ReviewID, err)
	}
	if (err != nil || time.Now().After(expires)) && found.UsedAt == nil {
		ctx.JSON(http.StatusGone, gin.H{
			"error": "that link has expired; submit the review again to get a new one",
		})
		return
	}

	var reviews []reviewRow
	if err := s.write.Select("reviews", eqSelect("id", found.ReviewID), &reviews); err != nil {
		sendInternalError(ctx, "v1/reviews/verify", err)
		return
	}
	if len(reviews) == 0 {
		ctx.JSON(http.StatusNotFound, gin.H{"error": "that review no longer exists"})
		return
	}
	review := reviews[0]

	// Replay: already verified. Report success without re-minting anything.
	if review.Status != "unverified" {
		ctx.JSON(http.StatusOK, gin.H{
			"status":  "already_verified",
			"message": "This review is already confirmed and awaiting moderation.",
		})
		return
	}

	// Mint the manage key here, not at submit.
	//
	// It used to be minted during submission and stashed in the verification
	// email's payload so this handler could read it back. That could never
	// work: the payload is cleared when the mail is sent, and the reviewer
	// cannot click a link in a mail that has not been sent. The key came back
	// empty every time.
	//
	// Minting it at the moment it is first needed removes the round trip
	// through the queue entirely. The column is overwritten rather than
	// filled because `edit_key_hash` is NOT NULL and submission has to put
	// something there; that placeholder is never delivered to anyone and is
	// superseded here.
	manageKey, err := newToken()
	if err != nil {
		sendInternalError(ctx, "v1/reviews/verify", err)
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if err := s.write.Update("reviews", eq("id", review.ID), map[string]any{
		"status":        "pending",
		"verified_at":   now,
		"edit_key_hash": hashToken(manageKey),
	}, nil); err != nil {
		sendInternalError(ctx, "v1/reviews/verify", err)
		return
	}
	if err := s.write.Update("review_tokens", eq("token_hash", found.TokenHash), map[string]any{
		"used_at": now,
	}, nil); err != nil {
		log.Printf("marking verify token used failed: %v", err)
	}

	// Hand the reviewer their manage key and email a copy. Shown once and
	// unrecoverable -- there is deliberately no way to link it back to a
	// person -- so emailing it too is the difference between a usable feature
	// and a support burden.
	s.emailManageKey(review.ID, review.InstructorID, manageKey)

	// Fire-and-forget: the reviewer's request completes as soon as the status
	// flips. They are never made to wait on n8n or on a model.
	go s.triage.Dispatch(review.ID)

	ctx.JSON(http.StatusOK, gin.H{
		"status":     "verified",
		"message":    "Thanks. Your review is awaiting moderation.",
		"manage_key": manageKey,
	})
}

// emailManageKey sends the reviewer a copy of the key just minted for them.
//
// Best effort: the key is already in the HTTP response, so a mail failure
// costs the reviewer their backup copy rather than the feature. The instructor
// name is looked up rather than read off the queue, so this does not depend on
// payload that the outbox is entitled to clear.
func (s *ReviewServer) emailManageKey(reviewID string, instructorID int64, manageKey string) {
	recipient := s.email.RecipientFor(reviewID)
	if recipient == "" {
		return
	}

	name := ""
	var instructors []instructorRow
	if err := s.write.Select("instructors", eqSelect("id", fmt.Sprintf("%d", instructorID)), &instructors); err == nil && len(instructors) > 0 {
		name = instructors[0].Name
	}

	if err := s.email.Queue(reviewID, recipient, "manage_key", map[string]any{
		"manage_key":      manageKey,
		"instructor_name": name,
	}); err != nil {
		log.Printf("queueing manage key email failed for review %s: %v", reviewID, err)
		return
	}
	go func() { _, _ = s.email.Flush(5) }()
}

/* ============================== manage ================================== */

// authorizeManage resolves a bearer manage key to the review it controls.
//
// The key is compared as a hash lookup rather than by fetching a candidate and
// comparing strings, so there is no per-character timing signal and no way to
// probe for which review ids exist.
func (s *ReviewServer) authorizeManage(ctx *gin.Context) (*reviewRow, bool) {
	key := bearerToken(ctx)
	if key == "" {
		ctx.JSON(http.StatusUnauthorized, gin.H{"error": "manage key required"})
		return nil, false
	}

	// Fails closed: this limiter is the brute-force guard on a bearer key, so
	// letting requests through when it errors removes the control at exactly
	// the wrong moment.
	within, err := checkRateLimit(s.write,
		"manage:"+hashOpaque(clientIP(ctx), s.cfg.EmailPepper), limitPerManageKey)
	if err != nil {
		log.Printf("manage-key rate limit check failed: %v", err)
		ctx.JSON(http.StatusServiceUnavailable, gin.H{"error": "try again shortly"})
		return nil, false
	}
	if !within {
		ctx.JSON(http.StatusTooManyRequests, gin.H{"error": "too many attempts"})
		return nil, false
	}

	params := url.Values{}
	params.Set("select", "*")
	params.Set("id", "eq."+ctx.Param("id"))
	params.Set("edit_key_hash", "eq."+hashToken(key))
	params.Set("limit", "1")

	var rows []reviewRow
	if err := s.write.Select("reviews", params, &rows); err != nil {
		sendInternalError(ctx, "v1/reviews/:id", err)
		return nil, false
	}
	if len(rows) == 0 {
		// Same answer for a wrong key and a missing review.
		ctx.JSON(http.StatusUnauthorized, gin.H{"error": "not authorized for that review"})
		return nil, false
	}
	return &rows[0], true
}

// HandleWithdraw retracts a review.
//
// A soft delete: the row stays so the dedupe index still means something, but
// the content is actually nulled rather than merely hidden. "Deleted" that
// leaves the text in the database is not what a reviewer asking for deletion
// is asking for.
func (s *ReviewServer) HandleWithdraw(ctx *gin.Context) {
	review, ok := s.authorizeManage(ctx)
	if !ok {
		return
	}

	if err := s.write.Update("reviews", eq("id", review.ID), map[string]any{
		"status":          "withdrawn",
		"title":           nil,
		"body":            nil,
		"expected_grade":  nil,
		"submit_ip_hash":  nil,
		"user_agent_hash": nil,
		"edited_at":       time.Now().UTC().Format(time.RFC3339),
	}, nil); err != nil {
		sendInternalError(ctx, "v1/reviews/:id", err)
		return
	}

	// Terminal, and the one status where a leftover address would be most
	// clearly wrong: the reviewer has just asked to be removed.
	s.email.PurgeContact(review.ID)

	ctx.JSON(http.StatusOK, gin.H{"status": "withdrawn"})
}

/* =============================== reporting ============================== */

type ReportRequest struct {
	Reason string `json:"reason" binding:"required"`
	Detail string `json:"detail"`
	Email  string `json:"email"`
}

// HandleReport files a report against a published review.
//
// This is the entirety of a professor's recourse in v1 -- there is no right of
// reply -- which makes the response time on these load-bearing rather than a
// nicety.
func (s *ReviewServer) HandleReport(ctx *gin.Context) {
	var req ReportRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "malformed request body"})
		return
	}

	within, err := checkRateLimit(s.write,
		"report:"+hashOpaque(clientIP(ctx), s.cfg.EmailPepper),
		RateLimit{Action: "report", Window: time.Hour, Max: 10})
	if err != nil {
		log.Printf("report rate limit check failed: %v", err)
		ctx.JSON(http.StatusServiceUnavailable, gin.H{"error": "try again shortly"})
		return
	}
	if !within {
		ctx.JSON(http.StatusTooManyRequests, gin.H{"error": "too many reports"})
		return
	}

	row := map[string]any{
		"review_id": ctx.Param("id"),
		"reason":    sanitizeText(req.Reason),
		"detail":    sanitizeText(req.Detail),
	}
	if req.Email != "" {
		row["reporter_email_hash"] = hashEmail(req.Email, s.cfg.EmailPepper)
	}

	if err := s.write.Insert("review_reports", []any{row}, nil); err != nil {
		sendInternalError(ctx, "v1/reviews/:id/report", err)
		return
	}
	ctx.JSON(http.StatusAccepted, gin.H{"status": "reported"})
}

func eqSelect(column, value string) url.Values {
	params := url.Values{}
	params.Set("select", "*")
	params.Set(column, "eq."+value)
	params.Set("limit", "1")
	return params
}
