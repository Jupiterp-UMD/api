package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

// The instructor-matching admin surface.
//
// `instructor_match_queue` holds every name the resolver could not settle. Each
// entry is a professor whose grade history is split across two records or
// attached to none, and the only way to fix one is for a person to say which
// record it is. Until this existed that meant hand-written SQL, which is why
// 257 entries had accumulated untouched.
//
// Behind the same admin key as the review queue. It is a smaller blast radius
// than moderation -- nothing here publishes text -- but it can merge two real
// professors' histories, which is not reversible from the merged state.

// matchQueueEntry is one row of `instructor_match_queue_detail`.
type matchQueueEntry struct {
	ID           int64           `json:"id"`
	Observed     string          `json:"observed"`
	ObservedNorm string          `json:"observed_norm"`
	Source       string          `json:"source"`
	Context      map[string]any  `json:"context"`
	CreatedAt    string          `json:"created_at"`
	Candidates   json.RawMessage `json:"candidates"`
}

// HandleInstructorQueue lists names awaiting a matching decision.
func (m *ModerationServer) HandleInstructorQueue(ctx *gin.Context) {
	params := url.Values{}
	params.Set("select", "*")
	params.Set("order", "id.asc")
	params.Set("limit", ctx.DefaultQuery("limit", "50"))
	params.Set("offset", ctx.DefaultQuery("offset", "0"))

	// Filtering by source lets the two populations be worked separately: a
	// registrar name is sixteen years of grade history looking for a home, a
	// testudo name is someone teaching right now whose page is unreachable.
	if source := ctx.Query("source"); source != "" {
		params.Set("source", "eq."+source)
	}

	var entries []matchQueueEntry
	if err := m.write.Select("instructor_match_queue_detail", params, &entries); err != nil {
		sendInternalError(ctx, "v1/admin/instructors/queue", err)
		return
	}

	ctx.JSON(http.StatusOK, gin.H{"count": len(entries), "entries": entries})
}

// instructorSearchResult is a candidate a moderator found by searching, for the
// case where the resolver offered nothing useful.
type instructorSearchResult struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	IsActive  bool   `json:"is_active"`
	NameNorm  string `json:"name_norm"`
	FirstTerm *int   `json:"first_seen_term"`
	LastTerm  *int   `json:"last_seen_term"`
}

// HandleInstructorSearch finds instructors by name for manual matching.
//
// The resolver's candidate list is generated from a surname match, so it misses
// exactly the cases a human is best at: a married name, a transliteration, a
// registrar spelling that shares no surname token with the Testudo one. This is
// the escape hatch for those.
func (m *ModerationServer) HandleInstructorSearch(ctx *gin.Context) {
	query := strings.TrimSpace(ctx.Query("q"))
	if len([]rune(query)) < 2 {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "q must be at least 2 characters"})
		return
	}

	params := url.Values{}
	params.Set("select", "id,name,slug,is_active,name_norm,first_seen_term,last_seen_term")
	// Against the normalized column, so an apostrophe or an accent typed by the
	// moderator does not decide whether they find the professor.
	params.Set("name_norm", "ilike.*"+strings.ToLower(query)+"*")
	params.Set("order", "name.asc")
	params.Set("limit", "20")

	var results []instructorSearchResult
	if err := m.write.Select("instructors", params, &results); err != nil {
		sendInternalError(ctx, "v1/admin/instructors/search", err)
		return
	}

	ctx.JSON(http.StatusOK, gin.H{"count": len(results), "instructors": results})
}

// MatchDecisionRequest is a moderator's answer for one queue entry.
type MatchDecisionRequest struct {
	// link, merge, create, or dismiss.
	Action string `json:"action" binding:"required,oneof=link merge create dismiss"`
	// Required for `link`. For `merge`, the record to keep.
	InstructorID *int64 `json:"instructor_id"`
	// The duplicate records to fold into InstructorID. Required for `merge`,
	// ignored otherwise.
	//
	// The survivor is named separately rather than taken as the first element,
	// because which record survives is the consequential half of the decision
	// -- it owns the slug every existing link points at -- and a positional
	// convention is the kind of thing a caller gets backwards exactly once.
	MergeIDs []int64 `json:"merge_ids"`
	// Proceed with a merge the database refused as probably-two-people.
	//
	// Only ever set by a moderator answering the confirmation the previous
	// unforced call returned; see the `needs_confirmation` branch in 0036.
	Force bool `json:"force"`
	// A note about who decided, for a caller that knows something the key does
	// not -- a shared key operated by a named person, say.
	//
	// NOT the audit identity. That comes from the key that authenticated the
	// request; see HandleInstructorMatch.
	Actor string `json:"actor"`
}

// HandleInstructorMatch applies a matching decision.
//
// The work happens in `resolve_instructor_match`, in one transaction, because
// the alias and the grade rows have to move together: repoint the alias alone
// and the professor page is empty, move the rows alone and the next scrape
// undoes it. A `merge` carries the same requirement one level further out: the
// duplicates are folded together and the observed spelling is linked to the
// survivor in that same transaction, so the queue entry can never be left open
// pointing at ids that no longer exist.
//
// A merge the database judged to be two different people comes back 200 with
// `status: needs_confirmation` and nothing written, not as an error. It is a
// question for the moderator, and the answer is `force: true` on the retry.
func (m *ModerationServer) HandleInstructorMatch(ctx *gin.Context) {
	var req MatchDecisionRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "action must be one of link, merge, create, dismiss"})
		return
	}
	if (req.Action == "link" || req.Action == "merge") && req.InstructorID == nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": req.Action + " requires instructor_id"})
		return
	}
	if req.Action == "merge" {
		if len(req.MergeIDs) == 0 {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "merge requires merge_ids"})
			return
		}
		// The SQL drops the survivor out of the merge list anyway, so this
		// rejects only the request that names nothing else -- which is a
		// no-op the caller almost certainly did not mean, not a merge.
		others := 0
		for _, id := range req.MergeIDs {
			if id != *req.InstructorID {
				others++
			}
		}
		if others == 0 {
			ctx.JSON(http.StatusBadRequest, gin.H{
				"error": "merge_ids names only the record being kept"})
			return
		}
	}

	// The actor is whoever the key says, not whoever the body says.
	//
	// This used to take `req.Actor` straight from the request, and the admin
	// page hardcodes `actor: 'moderator'` -- so every merge was recorded as
	// "moderator" no matter which key made it, while `ctx.GetString("moderator")`,
	// already resolved by AdminAuth from the key that authenticated, went
	// unread. That is precisely the capability REVIEW_MODERATOR_KEYS exists to
	// provide, on the one operation where it matters most: merging two
	// instructor identities cannot be undone from the merged state, and "who
	// did this" is the first question anyone will ask about it.
	//
	// A client-supplied note is still accepted, but only alongside the
	// authenticated name -- it can add detail, never replace the identity.
	actor := ctx.GetString("moderator")
	if actor == "" {
		// Only reachable if the auth middleware is ever changed to not set it.
		// Falls back to a generic label rather than rejecting, because the SQL
		// refuses the reserved machine actors outright and a human decision
		// recorded coarsely still tells human from automated.
		actor = "moderator"
	}
	if note := strings.TrimSpace(req.Actor); note != "" && note != actor {
		actor = actor + " (" + note + ")"
	}

	args := map[string]any{
		"p_queue_id": ctx.Param("id"),
		"p_action":   req.Action,
		"p_actor":    actor,
	}
	if req.InstructorID != nil {
		args["p_instructor_id"] = *req.InstructorID
	}
	if req.Action == "merge" {
		args["p_merge_ids"] = req.MergeIDs
		args["p_force"] = req.Force
	}

	var result map[string]any
	if err := m.write.RPC("resolve_instructor_match", args, &result); err != nil {
		sendInternalError(ctx, "v1/admin/instructors/queue/:id", err)
		return
	}

	ctx.JSON(http.StatusOK, result)
}
