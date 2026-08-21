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
	// Per group, not global, and per *kind of route* rather than per version.
	// Reads are a public API and stay open to every origin, which is what makes
	// them usable from anywhere. Writes get an explicit origin allowlist -- a
	// permissive policy on a write endpoint means any page on the internet can
	// make a visitor's browser submit a review.
	//
	// This distinction is why the read surface below is not simply added to the
	// existing /v1 group: that group carries the write allowlist, and reads
	// inheriting it would silently stop working from every origin except
	// jupiterp.com -- including the published npm client.

	// `cors.Default()` with one addition: `Content-Range` is exposed.
	//
	// Only the CORS-safelisted response headers reach browser JavaScript by
	// default, and `Content-Range` is not one of them. Without this, a
	// cross-origin `response.headers.get('Content-Range')` returns null -- not
	// an error, just null -- so a caller asking for `count=true`, and an API
	// dutifully computing the count, produced a total the page could never read.
	//
	// The professor directory is what this broke. It reads the total to render
	// "N professors" and to decide whether a "Load More" button exists; with the
	// total null, the count vanished and `hasMore` was permanently false, so
	// results were capped at the first page with no way forward. Nothing failed
	// and nothing logged.
	permissiveCORS := cors.New(cors.Config{
		AllowAllOrigins: true,
		AllowMethods:    []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"},
		AllowHeaders:    []string{"Origin", "Content-Length", "Content-Type"},
		ExposeHeaders:   []string{"Content-Range"},
		MaxAge:          12 * time.Hour,
	})

	/* ========================== STATIC CONTENT =========================== */

	r.StaticFile("/favicon.svg", "./favicon.svg")
	r.StaticFile("/docs.css", "./docs.css")

	/* ============================== ROUTES =============================== */

	r.GET("/", permissiveCORS, handleDocs) // API Docs

	/* ============================ READ SURFACE =========================== */
	//
	// The catalog and grade endpoints, served under both /v1 and /v0.
	//
	// /v1 is where these live now. /v0 stays registered against the same
	// handlers as a compatibility alias, because it is a documented public API:
	// `@jupiterp/jupiterp` 1.0.0 is on npm calling /v0 paths, and anything else
	// built against api.jupiterp.com/v0 would break the day it stopped
	// answering. An alias costs one line per route and removes any deadline for
	// consumers to migrate.
	//
	// Registered once and mounted twice so the two prefixes cannot drift. A new
	// endpoint added here appears on both; adding it to one group by hand is
	// how a version alias quietly becomes a version fork.
	//
	// Each route registers its own OPTIONS alongside its GET.
	//
	// Without one, a preflight for a read path fell through to the write
	// group's catch-all `OPTIONS /v1/*path` and was answered by the *write*
	// CORS policy -- so `OPTIONS /v1/courses` from a third-party origin got a
	// 403 while the GET behind it was open to everyone, and `/v0` had no
	// OPTIONS handler at all and answered 404. Simple GETs are unaffected,
	// which is why nothing caught this: only a caller that sends a header
	// forcing a preflight ever sees it, and that caller is a third party
	// rather than this site.
	registerReadRoutes := func(g *gin.RouterGroup) {
		get := func(path string, handler gin.HandlerFunc) {
			g.GET(path, handler)
			g.OPTIONS(path, handlePreflight)
		}

		get("/", client.handleBaseEndpoint) // base endpoint

		get("/courses", client.handleGetCourses)                       // full courses
		get("/courses/minified", client.handleMinifiedCourses)         // minified courses
		get("/courses/withSections", client.handleCoursesWithSections) // courses with sections

		get("/deptList", client.handleGetDepartments) // list of all 4-letter department codes

		get("/sections", client.handleGetSections) // sections for courses

		get("/instructors", client.handleGetInstructors)              // all instructors with ratings
		get("/instructors/active", client.handleGetActiveInstructors) // all instructors currently teaching

		get("/grades", client.handleGetGrades)               // section-level grade distributions
		get("/grades/summary", client.handleGetGradeSummary) // grades aggregated by course, term, or instructor
		get("/grades/terms", client.handleGetGradeTerms)     // terms for which grade data exists
	}

	// Deliberately outside the `cfg.WriteEnabled()` block below. The write
	// surface is conditional on a service key being present; the read surface
	// is not, and nesting it there would make the entire catalog disappear on
	// any deployment configured for reads only.
	v1Read := r.Group("/v1")
	v1Read.Use(permissiveCORS)
	registerReadRoutes(v1Read)

	v0 := r.Group("/v0")
	v0.Use(permissiveCORS)
	registerReadRoutes(v0)

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
			AllowMethods: []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
			AllowHeaders: []string{"Origin", "Content-Type", "Authorization"},
			// `GET /v1/reviews` and the moderation queue paginate the same way
			// the read surface does, so they need the same header exposed for
			// the same reason.
			ExposeHeaders:    []string{"Content-Range"},
			AllowCredentials: false,
			MaxAge:           12 * time.Hour,
		}))

		// Answer CORS preflight, per path.
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
		//
		// Listed rather than a catch-all. `OPTIONS /*path` swallowed the read
		// preflights on this same prefix and answered them with the write
		// origin allowlist; gin also refuses a catch-all once a static sibling
		// exists, which the read routes above now are. Enumerating is the
		// honest form anyway -- a write route is not reachable from a browser
		// until it appears here, and that is worth being visible.
		for _, path := range []string{
			"/reviews",
			"/reviews/verify/:token",
			"/reviews/manage",
			"/reviews/:id",
			"/reviews/:id/report",
		} {
			v1.OPTIONS(path, handlePreflight)
		}

		// Public reads of approved reviews. Served from public_reviews, which
		// cannot expose an unapproved row or an identity column.
		v1.GET("/reviews", client.HandleListReviews)

		// Reviewer-facing writes.
		v1.POST("/reviews", reviewServer.HandleSubmit)
		v1.GET("/reviews/verify/:token", reviewServer.HandleVerify)
		v1.GET("/reviews/manage", reviewServer.HandleManage)
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

		// Every admin route carries an Authorization header, so every admin
		// request from a browser is preflighted. The moderation queue is used
		// from a browser.
		for _, path := range []string{
			"/reviews",
			"/reviews/:id",
			"/reports",
			"/sweep",
			"/instructors/queue",
			"/instructors/queue/:id",
			"/instructors/search",
		} {
			admin.OPTIONS(path, handlePreflight)
		}

		log.Printf("v1 write path enabled for origins %v", cfg.AllowedOrigins)
	}

	// Listen and serve on defined port
	log.Printf("Listening on port %s", cfg.Port)
	r.Run(":" + cfg.Port)
}

// handlePreflight terminates a CORS preflight.
//
// Reached only when the request survives the group's CORS middleware, which
// aborts with 204 for an allowed origin and 403 for a disallowed one. Its job
// is to make the route exist so that middleware runs at all.
func handlePreflight(ctx *gin.Context) {
	ctx.Status(http.StatusNoContent)
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
