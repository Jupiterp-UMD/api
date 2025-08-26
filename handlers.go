package main

import (
	"errors"
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

// Arguments for getting a list of courses.
type CoursesArgs struct {
	// A string of one or multiple comma-separated course codes.
	CourseCodes string `form:"courseCodes"`

	// The prefix
	Prefix string `form:"prefix"`

	// A comma-separated list of GenEd codes to filter courses by.
	GenEds string `form:"genEds"`

	// Conditions for credits; for example, eq.3
	Credits []string `form:"credits"`

	// Number of courses to return per page.
	// Default value: 100; Maximum value: 500
	Limit uint16 `form:"limit" binding:"omitempty,min=1,max=500"`

	// The offset of courses to view. For example, offset=30 will return
	// courses starting at the 30th result.
	// Default value: 0
	Offset uint16 `form:"offset"`

	// String of columns to sort by
	SortBy string `form:"sortBy"`
}

func (c *CoursesArgs) setDefaults() {
	if c.Limit == 0 {
		c.Limit = 100
	}
}

// Arguments for getting a list of sections.
type SectionsArgs struct {
	// A comma-separated list of course codes to get sections for.
	CourseCodes string `form:"courseCodes"`

	// A prefix to filter courses by. For example, prefix=MATH will return
	// all sections for all MATH courses.
	CoursePrefix string `form:"prefix"`

	// Number of sections to return per page.
	// Default value: 100; Maximum value: 500
	Limit uint16 `form:"limit" binding:"omitempty,min=1,max=500"`

	// The offset of sections to view. For example, offset=30 will return
	// sections starting at the 30th result.
	// Default value: 0
	Offset uint16 `form:"offset"`

	// String of columns to sort by
	SortBy string `form:"sortBy"`
}

func (s *SectionsArgs) setDefaults() {
	if s.Limit == 0 {
		s.Limit = 100
	}
}

/* =============================== UTILITIES =============================== */

// Takes the error from a failed query argument validation/binding and sends a
// message to the caller listing any missing or invalid args.
func sendInvalidArgsError(ctx *gin.Context, argsType reflect.Type, path string, err error) {
	missing := []string{}
	invalid := []string{}

	var valErrs validator.ValidationErrors
	if errors.As(err, &valErrs) {
		errs := err.(validator.ValidationErrors)
		for _, e := range errs {
			fieldName := e.Field()
			if field, ok := argsType.FieldByName(fieldName); ok {
				if e.Tag() == "required" {
					missing = append(missing, field.Tag.Get("form"))
				} else {
					invalid = append(invalid, fmt.Sprintf("%s: %s", fieldName, field.Tag.Get("binding")))
				}
			} else {
				sendInternalError(ctx, path, fmt.Errorf("failed to identify fieldName: %s", fieldName))
				return
			}
		}

		errMsgList := []string{}
		if len(missing) > 0 {
			errMsgList = append(errMsgList, fmt.Sprintf("Missing required fields: %s", strings.Join(missing, ", ")))
		}
		if len(invalid) > 0 {
			errMsgList = append(errMsgList, fmt.Sprintf("Invalid fields: %s", strings.Join(invalid, ", ")))
		}
		errMsg := strings.Join(errMsgList, "; ")
		log.Printf("Received GET %s but was missing arguments: %s", path, errMsg)
		ctx.JSON(http.StatusBadRequest, gin.H{
			"error": errMsg,
		})
		return
	}

	// Non-validation bind errors (e.g., strconv.NumError: value out of range)
	log.Printf("Received GET %s with malformed query params: %v", path, err)
	ctx.JSON(http.StatusBadRequest, gin.H{
		"error": "Malformed query parameters. Check types and ranges.",
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

// Stream a response from DB back to the API caller.
func streamResponseToCaller(ctx *gin.Context, res *http.Response, path string) {
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
		log.Printf("Unexpected error occurred while streaming response to caller of %s: %s", path, err)
		return
	}

	log.Printf("Successfully handled GET %s with status %s", path, res.Status)
}

// General method for getting courses and sending the response to the caller.
func (client SupabaseClient) getCoursesAndSendResponse(
	ctx *gin.Context, columns []string, path string) {
	// Parse args
	var args CoursesArgs
	if err := ctx.ShouldBindQuery(&args); err != nil {
		sendInvalidArgsError(ctx, reflect.TypeOf(args), path, err)
		return
	}
	if args.CourseCodes != "" && args.Prefix != "" {
		ctx.JSON(http.StatusBadRequest, gin.H{
			"error": "Cannot specify both courseCodes and prefix",
		})
		return
	}
	args.setDefaults()

	// Get data from DB
	res, err := client.getCourses(args, columns)
	if err != nil {
		sendInternalError(ctx, path, err)
		return
	}

	streamResponseToCaller(ctx, res, path)
}

/* =============================== HANDLERS ================================ */

// Docs endpoint
func handleDocs(ctx *gin.Context) {
	ctx.File("docs.html")
}

// Base v0 endpoint
func (client SupabaseClient) handleBaseEndpoint(ctx *gin.Context) {
	ctx.String(http.StatusOK, "Welcome to the Jupiterp API!")
}

// Get a list of courses WITHOUT any section info.
// Example: /v0/courses/?limit=10&offset=50&prefix=CMSC
func (client SupabaseClient) handleGetCourses(ctx *gin.Context) {
	path := "v0/courses"
	client.getCoursesAndSendResponse(ctx, []string{"*"}, path)
}

// Get a minified list of courses. Returns only the course code and title.
// Same arguments as `handleGetCourses`.
func (client SupabaseClient) handleMinifiedCourses(ctx *gin.Context) {
	path := "v0/courses/minified"
	client.getCoursesAndSendResponse(ctx, []string{"course_code", "name"}, path)
}

// Get a list of sections for a given course.
func (client SupabaseClient) handleGetSections(ctx *gin.Context) {
	path := "v0/sections"

	var args SectionsArgs
	if err := ctx.ShouldBindQuery(&args); err != nil {
		sendInvalidArgsError(ctx, reflect.TypeOf(args), path, err)
		return
	}
	args.setDefaults()

	// Get data from DB
	res, err := client.getSections(args)
	if err != nil {
		sendInternalError(ctx, path, err)
		return
	}

	streamResponseToCaller(ctx, res, path)
}
