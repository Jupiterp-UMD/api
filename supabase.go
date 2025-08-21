package main

import (
	"net/http"
	"net/url"
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
func (s SupabaseClient) getSimpleCourse(courseCode string) (*http.Response, error) {
	params := url.Values{}
	params.Set("select", "*")
	params.Set("limit", "1")
	params.Set("course_code", "eq."+courseCode)
	return s.request("Courses", params.Encode())
}
