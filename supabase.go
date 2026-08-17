package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// A SupabaseClient connects with Supabase and retrieves course, section,
// or instructor data.
type SupabaseClient struct {
	Url string
	Key string
	// Cache for everything except course search.
	cache *LRUCache
	// Course search runs on every page load and has a small, hot key space.
	// Professor pages have a large one - a key per slug, plus per-professor
	// grade summaries - so they are kept in separate caches and a burst of
	// professor traffic cannot evict the course entries.
	courseCache *LRUCache
}

// The cache serving a given endpoint path.
func (s SupabaseClient) cacheFor(path string) *LRUCache {
	if strings.HasPrefix(path, "v0/courses") || strings.HasPrefix(path, "v0/sections") {
		return s.courseCache
	}
	return s.cache
}

// Request data from the `table` with the given query parameters `params`.
//
// Example use:
//
//	import "net/url"
//
//	s := SupabaseClient{Url: "database.com", Key: "myapikey"}
//	table := "courses"
//	params := url.Values{}
//	params.Set("select", "*")
//	params.Set("limit", "1")
//	res, err := s.request(table, params.Encode()) // SELECT * FROM courses LIMIT 1
func (s SupabaseClient) request(table string, params string) (*http.Response, error) {
	return s.requestWithPrefer(table, params, "")
}

// As `request`, but sets a PostgREST `Prefer` header.
//
// The only use so far is `count=exact`, which makes PostgREST return a
// `Content-Range` header carrying the total row count alongside the page. The
// professor directory needs it to render "1-50 of 4,812" and to know how many
// pages exist; without it a client can only discover the end by requesting
// past it.
//
// `count=exact` is opt-in per request because it costs a second aggregate over
// the filtered set. On a course search that already returns everything it is
// wasted work, and on `instructor_grades` it is a count over every instructor.
func (s SupabaseClient) requestWithPrefer(table string, params string, prefer string) (*http.Response, error) {
	fullUrl := s.Url + "/rest/v1/" + table + "?" + params
	method := "GET"                                 // GET requests will always be used
	req, _ := http.NewRequest(method, fullUrl, nil) // body always nil when getting data
	req.Header.Set("apikey", s.Key)
	req.Header.Set("Authorization", "Bearer "+s.Key)
	req.Header.Set("Content-Type", "application/json")
	if prefer != "" {
		req.Header.Set("Prefer", prefer)
	}
	return http.DefaultClient.Do(req)
}

// The `Prefer` header value for a request that asked for a total count.
func preferCount(exact bool) string {
	if exact {
		return "count=exact"
	}
	return ""
}

// Get a list of courses, without section info, that match the given args.
// Returns the columns provided as an argument.
func (s SupabaseClient) getCourses(args CoursesArgs, columns []string) (*http.Response, error) {
	// SELECT `columns` FROM courses
	// WHERE course_code LIKE `args.Prefix`*
	// / WHERE course_code IN `args.CourseCodes`
	// / WHERE course_code LIKE ____`args.Number`*
	// AND `args.GenEds` IN gen_eds
	// AND credits `args.Credits`
	// OFFSET `args.Offset` LIMIT `args.Limit`
	// SORT BY `args.SortBy`
	params := url.Values{}
	columnsStr := strings.Join(columns, ",")
	params.Set("select", columnsStr)
	if args.CourseCodes != "" {
		params.Set("course_code", fmt.Sprintf("in.(%s)", args.CourseCodes))
	} else if args.Prefix != "" {
		params.Set("course_code", fmt.Sprintf("like.%s*", args.Prefix))
	} else if args.Number != "" {
		params.Set("course_code", fmt.Sprintf("like.____%s*", args.Number))
	}
	if args.GenEds != "" {
		params.Set("gen_eds", fmt.Sprintf("cs.{%s}", args.GenEds))
	}
	for _, cond := range args.Credits {
		params.Add("min_credits", cond)
	}
	params.Set("offset", fmt.Sprintf("%d", args.Offset))
	params.Set("limit", fmt.Sprintf("%d", args.Limit))
	if args.SortBy != "" {
		params.Set("order", args.SortBy)
	}
	return s.request("courses", params.Encode())
}

// Get a list of sections for one or many courses.
func (s SupabaseClient) getSections(args SectionsArgs) (*http.Response, error) {
	// SELECT * FROM sections
	// WHERE course_code IN `args.CourseCodes` / WHERE course_code LIKE `args.CoursePrefix`*
	// AND credits `args.Credits`
	// AND total_seats `args.TotalClassSize`
	// AND open_seats > 0 (if `args.OnlyOpen` is true)
	// AND `args.Instructor` = ANY(instructors)
	// OFFSET `args.Offset` LIMIT `args.Limit`
	// SORT BY `args.SortBy`
	params := url.Values{}
	params.Set("select", "*")
	if args.CourseCodes != "" {
		params.Set("course_code", fmt.Sprintf("in.(%s)", args.CourseCodes))
	}
	if args.CoursePrefix != "" {
		params.Set("course_code", fmt.Sprintf("like.%s*", args.CoursePrefix))
	}
	params.Set("offset", fmt.Sprintf("%d", args.Offset))
	params.Set("limit", fmt.Sprintf("%d", args.Limit))
	if args.SortBy != "" {
		params.Set("order", args.SortBy)
	}
	if args.TotalClassSize != nil {
		for _, cond := range args.TotalClassSize {
			params.Add("total_seats", cond)
		}
	}
	if args.OnlyOpen {
		params.Add("open_seats", "gt.0")
	}
	if args.Instructor != "" {
		params.Set("instructors", fmt.Sprintf("cs.{%s}", args.Instructor))
	}
	if args.InstructorSlug != "" {
		params.Set("instructor_slugs", fmt.Sprintf("cs.{%s}", args.InstructorSlug))
	}
	// The view rather than the table: it carries `instructor_slugs`, the
	// resolved slug for each name in `instructors`, so a client can link a
	// professor without matching on their name. See migration 0024.
	return s.request("sections_with_instructors", params.Encode())
}

func (s SupabaseClient) getCoursesWithSections(args CoursesWithSectionsArgs) (*http.Response, error) {
	// SELECT * FROM courses
	// INNER JOIN sections ON courses.course_code = sections.course_code
	// AND sections.total_seats `args.TotalClassSize`
	// AND sections.open_seats > 0 (if `args.OnlyOpen` is true)
	// AND sections.`args.Instructor` = ANY(instructors)`
	// WHERE course_code LIKE `args.Prefix`*
	// / WHERE course_code IN `args.CourseCodes`
	// / WHERE course_code LIKE ____`args.Number`*
	// AND `args.GenEds` IN gen_eds
	// AND credits `args.Credits`
	// OFFSET `args.Offset` LIMIT `args.Limit`
	// SORT BY `args.SortBy`

	params := url.Values{}
	// `sections:sections_with_instructors` embeds the view but keeps the JSON
	// key `sections`, so the response shape is unchanged for existing clients
	// while every section gains `instructor_slugs`.
	selectStr := "*,sections:sections_with_instructors"
	if args.TotalClassSize != nil || args.OnlyOpen || args.Instructor != "" {
		selectStr += "!inner(*)"
	} else {
		selectStr += "(*)"
	}
	if args.TotalClassSize != nil {
		for _, cond := range args.TotalClassSize {
			params.Add("sections.total_seats", cond)
		}
	}
	if args.OnlyOpen {
		params.Add("sections.open_seats", "gt.0")
	}
	if args.Instructor != "" {
		params.Set("sections.instructors", fmt.Sprintf("cs.{%s}", args.Instructor))
	}
	params.Set("select", selectStr)
	if args.CourseCodes != "" {
		params.Set("course_code", fmt.Sprintf("in.(%s)", args.CourseCodes))
	} else if args.Prefix != "" {
		params.Set("course_code", fmt.Sprintf("like.%s*", args.Prefix))
	} else if args.Number != "" {
		params.Set("course_code", fmt.Sprintf("like.____%s*", args.Number))
	}
	if args.GenEds != "" {
		params.Set("gen_eds", fmt.Sprintf("cs.{%s}", args.GenEds))
	}
	for _, cond := range args.Credits {
		params.Add("min_credits", cond)
	}
	params.Set("offset", fmt.Sprintf("%d", args.Offset))
	params.Set("limit", fmt.Sprintf("%d", args.Limit))
	if args.SortBy != "" {
		params.Set("order", args.SortBy)
	}
	return s.request("courses", params.Encode())
}

// Get a list of instructors (including inactive ones) and their ratings.
func (s SupabaseClient) getInstructors(args InstructorArgs, table string) (*http.Response, error) {
	// SELECT * FROM instructors
	// WHERE instructor_name IN `args.InstructorNames`
	// AND instructor_slug IN `args.InstructorSlugs`
	// AND ratings `args.Ratings`
	// OFFSET `args.Offset` LIMIT `args.Limit`
	// SORT BY `args.SortBy`
	params := url.Values{}
	params.Set("select", "*")
	if args.InstructorNames != "" {
		params.Set("name", fmt.Sprintf("in.(%s)", args.InstructorNames))
	}
	if args.InstructorSlugs != "" {
		params.Set("slug", fmt.Sprintf("in.(%s)", args.InstructorSlugs))
	}
	// Case-insensitive substring search over the normalized name column,
	// backed by the gin_trgm_ops index on `name_norm`.
	//
	// Matching on `name_norm` rather than `name` is what makes searching for
	// "obrien" find "O'Brien" and "jose" find "José", since the stored value
	// has already had its punctuation and accents removed. The search term is
	// normalized the same way client-side before being sent.
	if args.NameSearch != "" {
		params.Set("name_norm", fmt.Sprintf("ilike.*%s*", args.NameSearch))
	}
	if args.ActiveOnly {
		params.Set("is_active", "eq.true")
	}
	for _, cond := range args.Ratings {
		params.Add("average_rating", cond)
	}
	params.Set("offset", fmt.Sprintf("%d", args.Offset))
	params.Set("limit", fmt.Sprintf("%d", args.Limit))
	if args.SortBy != "" {
		params.Set("order", args.SortBy)
	}
	return s.requestWithPrefer(table, params.Encode(), preferCount(args.Count))
}

// Get a list of all 4-letter department codes.
func (s SupabaseClient) getDepartments() (*http.Response, error) {
	// SELECT * FROM departments
	// ORDER BY dept_code
	params := url.Values{}
	params.Set("select", "*")
	params.Set("order", "dept_code")
	return s.request("departments", params.Encode())
}

// Apply the shared course-matching filters used by both grade endpoints. Only
// one of `courseCodes`, `prefix`, or `number` is honored; handlers reject
// requests that set more than one.
func applyCourseFilter(params url.Values, courseCodes, prefix, number string) {
	if courseCodes != "" {
		params.Set("course_code", fmt.Sprintf("in.(%s)", courseCodes))
	} else if prefix != "" {
		params.Set("course_code", fmt.Sprintf("like.%s*", prefix))
	} else if number != "" {
		params.Set("course_code", fmt.Sprintf("like.____%s*", number))
	}
}

// Get section-level grade distributions.
func (s SupabaseClient) getGrades(args GradesArgs) (*http.Response, error) {
	// SELECT * FROM grades
	// WHERE course_code IN `args.CourseCodes`
	// / WHERE course_code LIKE `args.Prefix`*
	// / WHERE course_code LIKE ____`args.Number`*
	// AND term `args.Terms`
	// AND instructor_name = `args.Instructor`
	// AND instructor_source IN `args.InstructorSource`
	// AND gpa `args.Gpa`
	// AND graded `args.Graded`
	// OFFSET `args.Offset` LIMIT `args.Limit`
	// SORT BY `args.SortBy`
	params := url.Values{}
	params.Set("select", "*")
	applyCourseFilter(params, args.CourseCodes, args.Prefix, args.Number)
	for _, cond := range args.Terms {
		params.Add("term", cond)
	}
	for _, cond := range args.Gpa {
		params.Add("gpa", cond)
	}
	for _, cond := range args.Graded {
		params.Add("graded", cond)
	}
	// Identity filters first: when a caller gives a slug or an id, the name is
	// redundant and would only narrow the result by an unreliable string
	// comparison on top of a reliable join.
	if args.InstructorId != 0 {
		params.Set("instructor_id", fmt.Sprintf("eq.%d", args.InstructorId))
	} else if args.InstructorSlug != "" {
		// `grades` holds instructor_id, not the slug, so this resolves through
		// the embedded instructors relationship rather than a second round
		// trip. PostgREST turns this into an inner join on the foreign key.
		params.Set("instructors.slug", fmt.Sprintf("eq.%s", args.InstructorSlug))
		params.Set("select", "*,instructors!inner(slug)")
	} else if args.Instructor != "" {
		params.Set("instructor_name", fmt.Sprintf("eq.%s", args.Instructor))
	}
	if args.InstructorSource != "" {
		params.Set("instructor_source", fmt.Sprintf("in.(%s)", args.InstructorSource))
	}
	params.Set("offset", fmt.Sprintf("%d", args.Offset))
	params.Set("limit", fmt.Sprintf("%d", args.Limit))
	if args.SortBy != "" {
		params.Set("order", args.SortBy)
	}
	return s.request("grades", params.Encode())
}

// Get aggregated grade distributions from one of the summary views. The view is
// chosen by the caller's `groupBy`; see `summaryTable`.
func (s SupabaseClient) getGradeSummary(args GradeSummaryArgs, table string) (*http.Response, error) {
	// SELECT * FROM `table`
	// WHERE course_code IN `args.CourseCodes` / LIKE `args.Prefix`* / LIKE ____`args.Number`*
	// AND term `args.Terms` (only when the view carries a term column)
	// AND instructor = `args.Instructor` (only on the instructor views)
	// AND gpa `args.Gpa`
	// AND total `args.MinStudents`
	// OFFSET `args.Offset` LIMIT `args.Limit`
	// SORT BY `args.SortBy`
	params := url.Values{}
	params.Set("select", "*")
	// The instructor-only rollups have no course_code column. Handlers reject
	// a course filter against them rather than letting it be dropped here,
	// because a silently ignored filter returns a professor's average across
	// everything to a caller who asked about one course.
	if !isCourselessSummary(table) {
		applyCourseFilter(params, args.CourseCodes, args.Prefix, args.Number)
	}
	if hasTermColumn(table) {
		for _, cond := range args.Terms {
			params.Add("term", cond)
		}
	}
	if isInstructorSummary(table) {
		if args.InstructorId != 0 {
			params.Set("instructor_id", fmt.Sprintf("eq.%d", args.InstructorId))
		} else if args.InstructorSlug != "" {
			params.Set("instructor_slug", fmt.Sprintf("eq.%s", args.InstructorSlug))
		} else if args.Instructor != "" {
			params.Set("instructor", fmt.Sprintf("eq.%s", args.Instructor))
		}
	}
	for _, cond := range args.Gpa {
		params.Add("gpa", cond)
	}
	if args.MinStudents > 0 {
		// `graded`, not `total`. Before Fall 2017 the registrar's total counts
		// students whose outcome was never categorized, so it is not
		// comparable across eras; `graded` is the letter-grade count and is
		// also the GPA denominator, which makes this threshold mean the same
		// thing as the sample the GPA came from.
		params.Set("graded", fmt.Sprintf("gte.%d", args.MinStudents))
	}
	params.Set("offset", fmt.Sprintf("%d", args.Offset))
	params.Set("limit", fmt.Sprintf("%d", args.Limit))
	if args.SortBy != "" {
		params.Set("order", args.SortBy)
	}
	return s.requestWithPrefer(table, params.Encode(), preferCount(args.Count))
}

// Get every term for which grade data has been loaded, newest first.
func (s SupabaseClient) getGradeTerms() (*http.Response, error) {
	// SELECT * FROM grade_terms ORDER BY term DESC
	params := url.Values{}
	params.Set("select", "*")
	params.Set("order", "term.desc")
	return s.request("grade_terms", params.Encode())
}
