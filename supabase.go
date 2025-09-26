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
	fullUrl := s.Url + "/rest/v1/" + table + "?" + params
	method := "GET"                                 // GET requests will always be used
	req, _ := http.NewRequest(method, fullUrl, nil) // body always nil when getting data
	req.Header.Set("apikey", s.Key)
	req.Header.Set("Authorization", "Bearer "+s.Key)
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
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
	return s.request("sections", params.Encode())
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
	for _, cond := range args.Ratings {
		params.Add("average_rating", cond)
	}
	params.Set("offset", fmt.Sprintf("%d", args.Offset))
	params.Set("limit", fmt.Sprintf("%d", args.Limit))
	if args.SortBy != "" {
		params.Set("order", args.SortBy)
	}
	return s.request(table, params.Encode())
}

// Get a list of all 4-letter department codes.
func (s SupabaseClient) getDepartments() (*http.Response, error) {
	// SELECT * FROM dept_codes
	// ORDER BY dept_code
	params := url.Values{}
	params.Set("select", "*")
	params.Set("order", "dept_code")
	return s.request("dept_codes", params.Encode())
}
