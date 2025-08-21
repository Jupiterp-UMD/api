package main

import (
	"io"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Takes an internal error and logs it for devs; sends a generic internal error
// message to an API caller. This allows for devs to see internal errors, but
// avoids exposing internal data to callers.
func sendInternalError(ctx *gin.Context, path string, err error) {
	log.Printf("Internal error while handling %s: %s", path, err)
	ctx.JSON(http.StatusInternalServerError, gin.H{
		"error": "Internal server error.",
	})
}

// Base endpoint
func (client SupabaseClient) handleBaseEndpoint(ctx *gin.Context) {
	ctx.String(http.StatusOK, "Welcome to the Jupiterp API!")
}

// Get course
// /v1/course/
func (client SupabaseClient) handleGetCourse(ctx *gin.Context) {
	path := "v1/course/"

	courseCode := ctx.Query("courseCode")
	if courseCode == "" {
		log.Printf("Received GET %s but no courseCode specified.", path)
		ctx.JSON(http.StatusBadRequest, gin.H{
			"error": "specify courseCode parameter (ex. courseCode=ENGL123)",
		})
		return
	}

	res, err := client.getSimpleCourse(courseCode)
	if err != nil {
		sendInternalError(ctx, path, err)
		return
	}

	// Stream DB response back to user
	defer res.Body.Close()
	for k, vv := range res.Header {
		switch http.CanonicalHeaderKey(k) {
		case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
			"Te", "Trailers", "Transfer-Encoding", "Upgrade":
			continue
		default:
			for _, v := range vv {
				ctx.Writer.Header().Add(k, v)
			}
		}
	}
	ctx.Status(res.StatusCode)
	if _, err := io.Copy(ctx.Writer, res.Body); err != nil {
		// client aborted or network issue; nothing else to do safely
		_ = ctx.Error(err)
		return
	}

	log.Printf("Successfully handled GET %s with status %s", path, res.Status)
}
