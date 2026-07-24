package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
)

const (
	coursesTTL     time.Duration = 2 * time.Hour
	instructorsTTL time.Duration = 12 * time.Hour
	departmentsTTL time.Duration = 2 * time.Hour
	sectionsTTL    time.Duration = 15 * time.Minute
	// Grade data changes once a term, when a new records request is fulfilled.
	gradesTTL time.Duration = 12 * time.Hour
)

// Views backing the /v0/grades/summary endpoint, selected by `groupBy`.
const (
	gradeSummaryByCourseTable     = "course_grades"
	gradeSummaryByTermTable       = "course_term_grades"
	gradeSummaryByInstructorTable = "course_instructor_grades"
	// As above, but also counting sections whose instructor was carried across
	// lecture groups rather than within one. See `instructor_source`.
	gradeSummaryByInstructorAllTable = "course_instructor_grades_all"
)

func isInstructorSummary(table string) bool {
	return table == gradeSummaryByInstructorTable ||
		table == gradeSummaryByInstructorAllTable
}

/* ================================= ARGS ================================== */
// For all argument structs, the first character of a field must be upper-case
// so it can be written to when parsing query args.

// Arguments for getting a list of courses.
type CoursesArgs struct {
	// A string of one or multiple comma-separated course codes.
	CourseCodes string `form:"courseCodes"`

	// The course prefix to filter by (ex. CMSC1 for all CMSC1XX courses).
	Prefix string `form:"prefix"`

	// The number to filter by (ex. 132 for CMSC132).
	Number string `form:"number"`

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

// Arguments for getting a list of courses WITH section info.
type CoursesWithSectionsArgs struct {
	// A string of one or multiple comma-separated course codes.
	CourseCodes string `form:"courseCodes"`

	// The course prefix to filter by (ex. CMSC1 for all CMSC1XX courses).
	Prefix string `form:"prefix"`

	// The number to filter by (ex. 132 for CMSC132).
	Number string `form:"number"`

	// A comma-separated list of GenEd codes to filter courses by.
	GenEds string `form:"genEds"`

	// Conditions for credits; for example, eq.3
	Credits []string `form:"credits"`

	// Total class size conditions; for example, lt.30
	TotalClassSize []string `form:"totalClassSize"`

	// Only open sections if true
	OnlyOpen bool `form:"onlyOpen"`

	// Instructor name filter (case sensitive, exact contains match)
	Instructor string `form:"instructor"`

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

func (c *CoursesWithSectionsArgs) setDefaults() {
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

	// Total class size conditions; for example, lt.30
	TotalClassSize []string `form:"totalClassSize"`

	// Only open sections if true
	OnlyOpen bool `form:"onlyOpen"`

	// Instructor name filter (case sensitive, exact contains match)
	Instructor string `form:"instructor"`
}

func (s *SectionsArgs) setDefaults() {
	if s.Limit == 0 {
		s.Limit = 100
	}
}

// Arguments for getting a list of instructors.
type InstructorArgs struct {
	// A comma-separated list of instructor names.
	InstructorNames string `form:"instructorNames"`

	// A comma-separated list of instructor slugs.
	InstructorSlugs string `form:"instructorSlugs"`

	// Conditions for instructor ratings; for example, gt.3.5
	Ratings []string `form:"ratings"`

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

func (i *InstructorArgs) setDefaults() {
	if i.Limit == 0 {
		i.Limit = 100
	}
}

// Arguments for getting section-level grade distributions.
type GradesArgs struct {
	// A string of one or multiple comma-separated course codes.
	CourseCodes string `form:"courseCodes"`

	// The course prefix to filter by (ex. CMSC1 for all CMSC1XX courses).
	Prefix string `form:"prefix"`

	// The number to filter by (ex. 132 for CMSC132).
	Number string `form:"number"`

	// Conditions for the term code; for example, gte.202008. For a specific
	// set of terms, in.(202408,202501).
	Terms []string `form:"term"`

	// Instructor name filter, in "First Last" order (case sensitive, exact).
	Instructor string `form:"instructor"`

	// A comma-separated list of instructor_source values to include; defaults
	// to every row. Use reported,lead to exclude attributions carried across
	// lecture groups.
	InstructorSource string `form:"instructorSource"`

	// Conditions for GPA; for example, gte.3.5
	Gpa []string `form:"gpa"`

	// Conditions for the number of students who received a letter grade; for
	// example, gte.30. Useful for excluding sections too small to read
	// anything into.
	Graded []string `form:"graded"`

	// Number of records to return per page.
	// Default value: 100; Maximum value: 500
	Limit uint16 `form:"limit" binding:"omitempty,min=1,max=500"`

	// The offset of records to view.
	// Default value: 0
	Offset uint16 `form:"offset"`

	// String of columns to sort by
	SortBy string `form:"sortBy"`
}

func (g *GradesArgs) setDefaults() {
	if g.Limit == 0 {
		g.Limit = 100
	}
}

// Arguments for getting aggregated grade distributions.
type GradeSummaryArgs struct {
	// How to group the results: course (default), term, or instructor.
	GroupBy string `form:"groupBy" binding:"omitempty,oneof=course term instructor"`

	// When grouping by instructor, also count sections whose instructor was
	// carried from a different lecture group or a differently-coded offering.
	// Wider coverage, lower confidence.
	IncludeCarried bool `form:"includeCarried"`

	// A string of one or multiple comma-separated course codes.
	CourseCodes string `form:"courseCodes"`

	// The course prefix to filter by (ex. CMSC1 for all CMSC1XX courses).
	Prefix string `form:"prefix"`

	// The number to filter by (ex. 132 for CMSC132).
	Number string `form:"number"`

	// Conditions for the term code; only applied when groupBy=term, since the
	// other groupings are aggregated across every term on file.
	Terms []string `form:"term"`

	// Instructor name filter, in "First Last" order (case sensitive, exact).
	// Only applied when groupBy=instructor.
	Instructor string `form:"instructor"`

	// Conditions for GPA; for example, gte.3.5
	Gpa []string `form:"gpa"`

	// Exclude groups totalling fewer than this many students.
	MinStudents uint16 `form:"minStudents"`

	// Number of records to return per page.
	// Default value: 100; Maximum value: 500
	Limit uint16 `form:"limit" binding:"omitempty,min=1,max=500"`

	// The offset of records to view.
	// Default value: 0
	Offset uint16 `form:"offset"`

	// String of columns to sort by
	SortBy string `form:"sortBy"`
}

func (g *GradeSummaryArgs) setDefaults() {
	if g.Limit == 0 {
		g.Limit = 100
	}
	if g.GroupBy == "" {
		g.GroupBy = "course"
	}
}

// Resolve `groupBy` and `includeCarried` to the view that serves them.
func (g GradeSummaryArgs) summaryTable() string {
	switch g.GroupBy {
	case "term":
		return gradeSummaryByTermTable
	case "instructor":
		if g.IncludeCarried {
			return gradeSummaryByInstructorAllTable
		}
		return gradeSummaryByInstructorTable
	default:
		return gradeSummaryByCourseTable
	}
}

/* =============================== UTILITIES =============================== */

// Reject requests that set more than one of the mutually exclusive course
// filters, all of which target the same column.
func checkCourseFilters(courseCodes, prefix, number string) error {
	set := 0
	for _, arg := range []string{courseCodes, prefix, number} {
		if arg != "" {
			set++
		}
	}
	if set > 1 {
		return errors.New("cannot specify more than one of courseCodes, prefix, and number")
	}
	return nil
}

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

func buildCacheKey(r *http.Request) string {
	base := r.Method + ":" + r.URL.Path
	rawQuery := r.URL.RawQuery
	if rawQuery == "" {
		return base
	}
	parts := strings.Split(rawQuery, "&")
	filtered := parts[:0]
	for _, part := range parts {
		if part == "" {
			continue
		}
		filtered = append(filtered, part)
	}
	if len(filtered) == 0 {
		return base
	}
	sort.Strings(filtered)
	return base + "?" + strings.Join(filtered, "&")
}

func writePayload(ctx *gin.Context, payload *cachedPayload, path string) bool {
	header := ctx.Writer.Header()
	replacedKeys := make(map[string]struct{}, len(payload.header))
	for k := range payload.header {
		replacedKeys[http.CanonicalHeaderKey(k)] = struct{}{}
	}
	for k := range header {
		if _, shouldReplace := replacedKeys[k]; shouldReplace {
			header.Del(k)
		}
	}
	for k, values := range payload.header {
		canonicalKey := http.CanonicalHeaderKey(k)
		header.Del(canonicalKey)
		for _, v := range values {
			header.Add(canonicalKey, v)
		}
	}
	ctx.Status(payload.status)
	if _, err := ctx.Writer.Write(payload.body); err != nil {
		_ = ctx.Error(err)
		log.Printf("Unexpected error occurred while streaming response to caller of %s: %s", path, err)
		return false
	}
	return true
}

func buildPayloadFromResponse(res *http.Response) (*cachedPayload, error) {
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	return &cachedPayload{
		status: res.StatusCode,
		header: filterHeadersForCaching(res.Header),
		body:   body,
	}, nil
}

func (client SupabaseClient) serveFromCache(ctx *gin.Context, path, key string) bool {
	payload, ok := client.cache.Get(key)
	if !ok {
		log.Printf("Cache MISS for GET %s with key %s", path, key)
		return false
	}
	if writePayload(ctx, payload, path) {
		log.Printf("Cache HIT and served GET %s from cache with status %d", path, payload.status)
	}

	return true
}

func (client SupabaseClient) writeAndCacheResponse(ctx *gin.Context, res *http.Response, path, key string, ttl time.Duration) {
	statusText := res.Status
	payload, err := buildPayloadFromResponse(res)
	if err != nil {
		sendInternalError(ctx, path, err)
		return
	}
	if writePayload(ctx, payload, path) {
		log.Printf("Successfully handled GET %s with status %s", path, statusText)
	}
	if res.StatusCode < http.StatusInternalServerError {
		client.cache.Set(key, payload, ttl)
	}
}

// General method for getting courses and sending the response to the caller.
func (client SupabaseClient) getCoursesAndSendResponse(
	ctx *gin.Context, columns []string, path string, ttl time.Duration) {
	// Parse args
	var args CoursesArgs
	if err := ctx.ShouldBindQuery(&args); err != nil {
		sendInvalidArgsError(ctx, reflect.TypeOf(args), path, err)
		return
	}
	if args.CourseCodes != "" && args.Prefix != "" && args.Number != "" {
		ctx.JSON(http.StatusBadRequest, gin.H{
			"error": "Cannot specify courseCodes, prefix, and number simultaneously",
		})
		return
	}
	if args.CourseCodes != "" && args.Prefix != "" {
		ctx.JSON(http.StatusBadRequest, gin.H{
			"error": "Cannot specify both courseCodes and prefix",
		})
		return
	}
	if args.CourseCodes != "" && args.Number != "" {
		ctx.JSON(http.StatusBadRequest, gin.H{
			"error": "Cannot specify both courseCodes and number",
		})
		return
	}
	if args.Prefix != "" && args.Number != "" {
		ctx.JSON(http.StatusBadRequest, gin.H{
			"error": "Cannot specify both prefix and number",
		})
		return
	}
	args.setDefaults()

	key := buildCacheKey(ctx.Request)
	if client.serveFromCache(ctx, path, key) {
		return
	}

	// Get data from DB
	res, err := client.getCourses(args, columns)
	if err != nil {
		sendInternalError(ctx, path, err)
		return
	}

	client.writeAndCacheResponse(ctx, res, path, key, ttl)
}

// General method for getting instructors and sending the response to the caller.
func (client SupabaseClient) getInstructorsAndSendResponse(
	ctx *gin.Context, path string, table string, ttl time.Duration) {
	var args InstructorArgs
	if err := ctx.ShouldBindQuery(&args); err != nil {
		sendInvalidArgsError(ctx, reflect.TypeOf(args), path, err)
		return
	}
	if args.InstructorNames != "" && args.InstructorSlugs != "" {
		sendInvalidArgsError(ctx, reflect.TypeOf(args), path, errors.New("cannot specify both instructorNames and instructorSlugs"))
		return
	}
	args.setDefaults()

	key := buildCacheKey(ctx.Request)
	if client.serveFromCache(ctx, path, key) {
		return
	}

	// Get data from DB
	res, err := client.getInstructors(args, table)
	if err != nil {
		sendInternalError(ctx, path, err)
		return
	}

	client.writeAndCacheResponse(ctx, res, path, key, ttl)
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
	client.getCoursesAndSendResponse(ctx, []string{"*"}, path, coursesTTL)
}

// Get a minified list of courses. Returns only the course code and title.
// Same arguments as `handleGetCourses`.
func (client SupabaseClient) handleMinifiedCourses(ctx *gin.Context) {
	path := "v0/courses/minified"
	client.getCoursesAndSendResponse(ctx, []string{"course_code", "name"}, path, coursesTTL)
}

func (client SupabaseClient) handleCoursesWithSections(ctx *gin.Context) {
	path := "v0/courses/withSections"

	var args CoursesWithSectionsArgs
	if err := ctx.ShouldBindQuery(&args); err != nil {
		sendInvalidArgsError(ctx, reflect.TypeOf(args), path, err)
		return
	}
	if (args.CourseCodes != "" && args.Prefix != "" && args.Number != "") ||
		(args.CourseCodes != "" && args.Prefix != "") ||
		(args.CourseCodes != "" && args.Number != "") ||
		(args.Prefix != "" && args.Number != "") {
		ctx.JSON(http.StatusBadRequest, gin.H{
			"error": "Cannot specify courseCodes, prefix, and number simultaneously",
		})
		return
	}

	args.setDefaults()

	key := buildCacheKey(ctx.Request)
	if client.serveFromCache(ctx, path, key) {
		return
	}

	// Get data from DB
	res, err := client.getCoursesWithSections(args)
	if err != nil {
		sendInternalError(ctx, path, err)
		return
	}

	client.writeAndCacheResponse(ctx, res, path, key, sectionsTTL)
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

	key := buildCacheKey(ctx.Request)
	if client.serveFromCache(ctx, path, key) {
		return
	}

	// Get data from DB
	res, err := client.getSections(args)
	if err != nil {
		sendInternalError(ctx, path, err)
		return
	}

	client.writeAndCacheResponse(ctx, res, path, key, sectionsTTL)
}

// Get a list of instructors with their ratings.
func (client SupabaseClient) handleGetInstructors(ctx *gin.Context) {
	path := "v0/instructors"
	client.getInstructorsAndSendResponse(ctx, path, "instructors", instructorsTTL)
}

// Get a list of instructors currently teaching courses.
func (client SupabaseClient) handleGetActiveInstructors(ctx *gin.Context) {
	path := "v0/instructors/active"
	client.getInstructorsAndSendResponse(ctx, path, "active_instructors", instructorsTTL)
}

// Get a list of all 4-letter department codes.
func (client SupabaseClient) handleGetDepartments(ctx *gin.Context) {
	path := "v0/deptList"

	key := buildCacheKey(ctx.Request)
	if client.serveFromCache(ctx, path, key) {
		return
	}

	// Get data from DB
	res, err := client.getDepartments()
	if err != nil {
		sendInternalError(ctx, path, err)
		return
	}

	client.writeAndCacheResponse(ctx, res, path, key, departmentsTTL)
}

// Get section-level grade distributions.
// Example: /v0/grades?courseCodes=CMSC132&term=gte.202008&sortBy=term.desc
func (client SupabaseClient) handleGetGrades(ctx *gin.Context) {
	path := "v0/grades"

	var args GradesArgs
	if err := ctx.ShouldBindQuery(&args); err != nil {
		sendInvalidArgsError(ctx, reflect.TypeOf(args), path, err)
		return
	}
	if err := checkCourseFilters(args.CourseCodes, args.Prefix, args.Number); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	args.setDefaults()

	key := buildCacheKey(ctx.Request)
	if client.serveFromCache(ctx, path, key) {
		return
	}

	// Get data from DB
	res, err := client.getGrades(args)
	if err != nil {
		sendInternalError(ctx, path, err)
		return
	}

	client.writeAndCacheResponse(ctx, res, path, key, gradesTTL)
}

// Get grade distributions aggregated by course, by course and term, or by
// course and instructor.
// Example: /v0/grades/summary?groupBy=instructor&courseCodes=CMSC330&minStudents=100
func (client SupabaseClient) handleGetGradeSummary(ctx *gin.Context) {
	path := "v0/grades/summary"

	var args GradeSummaryArgs
	if err := ctx.ShouldBindQuery(&args); err != nil {
		sendInvalidArgsError(ctx, reflect.TypeOf(args), path, err)
		return
	}
	if err := checkCourseFilters(args.CourseCodes, args.Prefix, args.Number); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	args.setDefaults()

	key := buildCacheKey(ctx.Request)
	if client.serveFromCache(ctx, path, key) {
		return
	}

	// Get data from DB
	res, err := client.getGradeSummary(args, args.summaryTable())
	if err != nil {
		sendInternalError(ctx, path, err)
		return
	}

	client.writeAndCacheResponse(ctx, res, path, key, gradesTTL)
}

// Get every term for which grade data is available.
func (client SupabaseClient) handleGetGradeTerms(ctx *gin.Context) {
	path := "v0/grades/terms"

	key := buildCacheKey(ctx.Request)
	if client.serveFromCache(ctx, path, key) {
		return
	}

	// Get data from DB
	res, err := client.getGradeTerms()
	if err != nil {
		sendInternalError(ctx, path, err)
		return
	}

	client.writeAndCacheResponse(ctx, res, path, key, gradesTTL)
}
