package main

import (
	"fmt"
	"log"
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
//	table := "Courses"
//	params := url.Values{}
//	params.Set("select", "*")
//	params.Set("limit", "1")
//	res, err := s.request(table, params.Encode()) // SELECT * FROM Courses LIMIT 1
func (s SupabaseClient) request(table string, params string) (*http.Response, error) {
	fullUrl := s.Url + "/rest/v1/" + table + "?" + params
	method := "GET"                                 // GET requests will always be used
	req, _ := http.NewRequest(method, fullUrl, nil) // body always nil when getting data
	req.Header.Set("apikey", s.Key)
	req.Header.Set("Authorization", "Bearer "+s.Key)
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

// Get a specific course from the `Courses` table, without section info.
func (s SupabaseClient) getSingleCourse(courseCode string) (*http.Response, error) {
	// SELECT * FROM Courses WHERE course_code = `courseCode` LIMIT 1
	params := url.Values{}
	params.Set("select", "*")
	params.Set("course_code", "eq."+courseCode)
	params.Set("limit", "1")
	return s.request("Courses", params.Encode())
}

// Get a list of courses, without section info, that match the given args.
// Returns the columns provided as an argument.
func (s SupabaseClient) getCourses(args CoursesArgs, columns []string) (*http.Response, error) {
	// SELECT `columns` FROM Courses
	// WHERE course_code LIKE `args.Department`*
	// AND `args.GenEds` IN gen_eds
	// AND credits `args.Credits`
	// OFFSET `args.Offset` LIMIT `args.Limit`
	params := url.Values{}
	columnsStr := strings.Join(columns, ",")
	params.Set("select", columnsStr)
	params.Set("course_code", fmt.Sprintf("like.%s*", args.Department))
	if len(args.GenEds) > 0 {
		log.Printf("Filtering by GenEds: %v", args.GenEds)
		params.Set("gen_eds", fmt.Sprintf("cs.{%s}", strings.Join(args.GenEds, ",")))
	}
	if len(args.Credits) > 0 {
		params.Set("min_credits", strings.Join(args.Credits, ","))
	}
	params.Set("offset", fmt.Sprintf("%d", args.Offset))
	params.Set("limit", fmt.Sprintf("%d", args.Limit))
	return s.request("Courses", params.Encode())
}

// Get a list of sections for a specific course.
func (s SupabaseClient) getSectionsForCourse(courseCode string) (*http.Response, error) {
	// SELECT * FROM Sections
	// WHERE course_code = `courseCode`
	params := url.Values{}
	params.Set("select", "*")
	params.Set("course_code", "eq."+courseCode)
	return s.request("Sections", params.Encode())
}
