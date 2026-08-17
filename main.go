/*
This is the main package for the Jupiterp API binary. The Jupiterp API provides
access for external (non-Jupiterp) developers to Jupiterp data, including
courses and their sections compiled from Testudo, and instructor ratings
retrieved from PlanetTerp (these can be accessed directly via the PlanetTerp
API).

This binary uses the following environment variables:
  - DATABASE_URL (mandatory): The database URL to retrieve course, section,
    and instructor data from
  - DATABASE_KEY (mandatory): The database key used to access course, section,
    and instructor data
  - PORT (optional): The port to serve API on; default is 8080
*/
package main

// docs.html is generated from docs.md; do not edit it by hand.
//
//	go generate ./...
//
// The two were maintained in parallel by hand until the grade endpoints made
// that untenable, and they had already drifted. See tools/docsgen.
//go:generate go run ./tools/docsgen

import (
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.LUTC | log.Lshortfile)

	cfg := LoadConfig()
	cfg.Validate()

	// Initialize Gin instance and middleware
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(requestLogger())

	// Create SupabaseClient to connect with DB
	client := SupabaseClient{
		Url:         cfg.DatabaseURL,
		Key:         cfg.DatabaseKey,
		cache:       NewLRUCache(defaultCacheCapacity),
		courseCache: NewLRUCache(courseCacheCapacity),
	}

	/* ============================== CORS ================================= */
	//
	// Per group, not global. /v0 is a read-only public API and stays open to
	// every origin, which is what makes it usable from anywhere. /v1 accepts
	// writes and gets an explicit origin allowlist -- a permissive policy on a
	// write endpoint means any page on the internet can make a visitor's
	// browser submit a review.

	permissiveCORS := cors.Default()

	/* ========================== STATIC CONTENT =========================== */

	r.StaticFile("/favicon.svg", "./favicon.svg")
	r.StaticFile("/docs.css", "./docs.css")

	/* ============================== ROUTES =============================== */

	r.GET("/", permissiveCORS, handleDocs) // API Docs

	v0 := r.Group("/v0")
	v0.Use(permissiveCORS)
	v0.GET("/", client.handleBaseEndpoint) // base v0 endpoint

	v0.GET("/courses", client.handleGetCourses)                       // full courses
	v0.GET("/courses/minified", client.handleMinifiedCourses)         // minified courses
	v0.GET("/courses/withSections", client.handleCoursesWithSections) // courses with sections

	v0.GET("/deptList", client.handleGetDepartments) // list of all 4-letter department codes

	v0.GET("/sections", client.handleGetSections) // sections for courses

	v0.GET("/instructors", client.handleGetInstructors)              // all instructors with ratings
	v0.GET("/instructors/active", client.handleGetActiveInstructors) // all instructors currently teaching

	v0.GET("/grades", client.handleGetGrades)               // section-level grade distributions
	v0.GET("/grades/summary", client.handleGetGradeSummary) // grades aggregated by course, term, or instructor
	v0.GET("/grades/terms", client.handleGetGradeTerms)     // terms for which grade data exists

	/* =============================== V1 ================================== */
	//
	// Everything that writes. Every new security property in this service
	// lands here and nowhere else, which is what keeps /v0 the simple,
	// cacheable, unauthenticated surface it has always been.

	if cfg.WriteEnabled() {
		writeClient := NewWriteClient(cfg.DatabaseURL, cfg.ServiceKey)
		emailSender := NewEmailSender(cfg, writeClient)
		triageClient := NewTriageClient(cfg, writeClient)
		reviewServer := NewReviewServer(cfg, writeClient, emailSender, triageClient)
		moderationServer := NewModerationServer(cfg, writeClient, emailSender, triageClient)

		v1 := r.Group("/v1")
		v1.Use(cors.New(cors.Config{
			AllowOrigins: cfg.AllowedOrigins,
			// PUT is here because `admin.PUT /reviews/:id` is the moderation
			// decision route -- the one a moderator uses to approve or reject.
			// It was the only verb the group serves that this list omitted, so
			// the preflight answered 204 while advertising a method set without
			// it, and the browser refused the request. Anything added to this
			// group needs its verb here too; the route registering is not what
			// makes it reachable from a browser.
			AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
			AllowHeaders:     []string{"Origin", "Content-Type", "Authorization"},
			AllowCredentials: false,
			MaxAge:           12 * time.Hour,
		}))

		// Answer CORS preflight.
		//
		// Gin routes by method, and group middleware only runs once a route in
		// that group matches. With no OPTIONS handler registered, an OPTIONS
		// request fell through to the engine's 404 and the CORS middleware above
		// -- the thing meant to answer it -- never ran.
		//
		// That broke every browser write. A POST carrying
		// `Content-Type: application/json` is not a simple request, so the
		// browser preflights it first; the preflight 404s, the browser refuses
		// to send the POST, and the form sits on "sending" with no error the
		// server ever sees. Reads were unaffected, which is why this looked like
		// a submission bug rather than a CORS one.
		//
		// The handler body is never reached for an allowed origin -- the CORS
		// middleware aborts with 204 first -- but registering the route is what
		// puts the middleware in the chain at all.
		v1.OPTIONS("/*path", func(ctx *gin.Context) {
			ctx.Status(http.StatusNoContent)
		})

		// Public reads of approved reviews. Served from public_reviews, which
		// cannot expose an unapproved row or an identity column.
		v1.GET("/reviews", client.HandleListReviews)

		// Reviewer-facing writes.
		v1.POST("/reviews", reviewServer.HandleSubmit)
		v1.GET("/reviews/verify/:token", reviewServer.HandleVerify)
		v1.DELETE("/reviews/:id", reviewServer.HandleWithdraw)
		v1.POST("/reviews/:id/report", reviewServer.HandleReport)

		// Moderation. The decision route accepts the admin key or the scoped
		// triage callback key and records which one acted; everything else
		// requires the admin key.
		admin := v1.Group("/admin")
		admin.GET("/reviews", AdminAuth(cfg), moderationServer.HandleQueue)
		admin.GET("/reports", AdminAuth(cfg), moderationServer.HandleReports)
		admin.PUT("/reviews/:id", ModerationAuth(cfg), moderationServer.HandleDecide)
		admin.POST("/sweep", AdminAuth(cfg), moderationServer.HandleSweep)

		// Instructor matching. Same admin key as moderation: it publishes no
		// text, but it can merge two real professors' histories, which is not
		// reversible once merged.
		admin.GET("/instructors/queue", AdminAuth(cfg), moderationServer.HandleInstructorQueue)
		admin.GET("/instructors/search", AdminAuth(cfg), moderationServer.HandleInstructorSearch)
		admin.POST("/instructors/queue/:id", AdminAuth(cfg), moderationServer.HandleInstructorMatch)

		log.Printf("v1 write path enabled for origins %v", cfg.AllowedOrigins)
	}

	// Listen and serve on defined port
	log.Printf("Listening on port %s", cfg.Port)
	r.Run(":" + cfg.Port)
}

// requestLogger emits one structured line per request.
//
// `main.go` carried a "TODO: Add logger, auth with keys" for as long as the
// service was a read-only proxy, where it did not much matter. With a write
// path it is what makes an abuse incident investigable, so it is no longer a
// TODO. Deliberately does not log query strings or bodies on /v1: those carry
// email addresses and tokens.
func requestLogger() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		start := time.Now()
		path := ctx.Request.URL.Path
		ctx.Next()

		fields := []any{
			ctx.Request.Method,
			path,
			ctx.Writer.Status(),
			time.Since(start).Round(time.Millisecond),
		}
		if strings.HasPrefix(path, "/v1") {
			log.Printf("%s %s -> %d in %s ip=%s", append(fields, hashedIPForLog(ctx))...)
			return
		}
		log.Printf("%s %s -> %d in %s", fields...)
	}
}

// hashedIPForLog gives a stable per-client identifier for correlating abuse
// without writing raw addresses into a log sink.
func hashedIPForLog(ctx *gin.Context) string {
	ip := clientIP(ctx)
	if ip == "" {
		return "unknown"
	}
	sum := sha256.Sum256([]byte(ip))
	return hex.EncodeToString(sum[:])[:12]
}
