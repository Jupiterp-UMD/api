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

// Arguments for getting info for a single course (without sections info).
type SingleCourseArgs struct {
	CourseCode string `form:"courseCode" binding:"required"`
}

// Arguments for getting a list of courses.
type CoursesArgs struct {
	// Number of courses to return per page.
	// Default value: 100; Maximum value: 500
	Limit uint16 `form:"limit" binding:"omitempty,min=1,max=500"`

	// The offset of courses to view. For example, offset=30 will return
	// courses starting at the 30th result.
	// Default value: 0
	Offset uint16 `form:"offset"`

	// The 4-letter code of a specific department to return results for.
	// Ex. CMSC, ENGL, MATH.
	Department string `form:"department" binding:"omitempty,len=4"`

	// A list of 4-letter GenEd codes to filter courses by.
	// Gin doesn't separate values by comma, so separate `genEd` arguments
	// need to be used for each one. Ex. ...&genEd=DSSP&genEd=SCIS...
	GenEds []string `form:"genEd"`
}

func (c *CoursesArgs) setDefaults() {
	if c.Limit == 0 {
		c.Limit = 100
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

// Base endpoint
func (client SupabaseClient) handleBaseEndpoint(ctx *gin.Context) {
	ctx.String(http.StatusOK, "Welcome to the Jupiterp API!")
}

// Get info for a single course WITHOUT any section info. Takes courseCode as a
// query parameter. Example: /v1/course/?courseCode=MATH240
func (client SupabaseClient) handleGetCourse(ctx *gin.Context) {
	path := "v1/course"

	// Parse args
	var args SingleCourseArgs
	if err := ctx.ShouldBindQuery(&args); err != nil {
		sendInvalidArgsError(ctx, reflect.TypeOf(args), path, err)
		return
	}
	courseCode := args.CourseCode

	// Get data from DB
	res, err := client.getSimpleCourse(courseCode)
	if err != nil {
		sendInternalError(ctx, path, err)
		return
	}

	streamResponseToCaller(ctx, res, path)
}

// Get a list of courses WITHOUT any section info.
// Example: /v1/courses/?limit=10&offset=50&department=CMSC
func (client SupabaseClient) handleGetCourses(ctx *gin.Context) {
	path := "v1/courses"
	client.getCoursesAndSendResponse(ctx, []string{"*"}, path)
}

// Get a minified list of courses. Returns only the course code and title.
// Same arguments as `handleGetCourses`.
func (client SupabaseClient) handleMinifiedCourses(ctx *gin.Context) {
	path := "v1/courses/minified"
	client.getCoursesAndSendResponse(ctx, []string{"course_code", "name"}, path)
}
