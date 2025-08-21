package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
)

/* ================================= ARGS ================================== */
// For all argument structs, the first character of a field must be upper-case
// so it can be written to when parsing query args.

type SingleCourseArgs struct {
	CourseCode string `form:"courseCode" binding:"required"`
}

/* =============================== UTILITIES =============================== */

// Takes the error from a failed query argument binding and sends an error
// message to the caller listing any missing args.
func sendMissingArgsError(ctx *gin.Context, argsType reflect.Type, path string, err error) {
	errs := err.(validator.ValidationErrors)
	missing := []string{}
	for _, e := range errs {
		if e.Tag() == "required" {
			fieldName := e.Field()
			if field, ok := argsType.FieldByName(fieldName); ok {
				missing = append(missing, field.Tag.Get("form"))
			} else {
				log.Printf("Unknown field name %s found when reporting missing args", fieldName)
			}
		}
	}

	missingString := strings.Join(missing, ", ")
	log.Printf("Received GET %s but was missing arguments: %s", path, missingString)
	ctx.JSON(http.StatusBadRequest, gin.H{
		"error": fmt.Sprintf("Missing required fields: %s", missingString),
	})
}

// Takes an internal error and logs it for devs; sends a generic internal error
// message to an API caller. This allows for devs to see internal errors, but
// avoids exposing internal data to callers.
func sendInternalError(ctx *gin.Context, path string, err error) {
	log.Printf("Internal error while handling %s: %s", path, err)
	ctx.JSON(http.StatusInternalServerError, gin.H{
		"error": "Internal server error.",
	})
}

/* =============================== HANDLERS ================================ */

// Base endpoint
func (client SupabaseClient) handleBaseEndpoint(ctx *gin.Context) {
	ctx.String(http.StatusOK, "Welcome to the Jupiterp API!")
}

// Get course
// /v1/course/
func (client SupabaseClient) handleGetCourse(ctx *gin.Context) {
	path := "v1/course/"

	// Parse args
	var args SingleCourseArgs
	if err := ctx.ShouldBindQuery(&args); err != nil {
		sendMissingArgsError(ctx, reflect.TypeOf(args), path, err)
		return
	}
	courseCode := args.CourseCode

	// Get data from DB
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
