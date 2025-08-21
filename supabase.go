package main

import (
	"net/http"

	"github.com/gin-gonic/gin"
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
//	q := url.Values{}
//	q.Set("select", *)
//	q.Set("limit", 1)
//	res, err := s.request(table, q.Encode()) // SELECT * FROM Courses LIMIT 1
func (s SupabaseClient) request(table string, params string) (*http.Response, error) {
	fullUrl := s.Url + "/rest/v1/" + table + params
	method := "GET"                                 // GET requests will always be used
	req, _ := http.NewRequest(method, fullUrl, nil) // body always nil when getting data
	req.Header.Set("apikey", s.Key)
	req.Header.Set("Authorization", "Bearer "+s.Key)
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

// Uses currying to take a function that takes a SupabaseClient and a
// Gin Context and returns a Gin-compatible HandlerFunc.
//
// Example use:
//
//	func handleWithClient(client SupabaseClient, ctx *gin.Context) {
//		...
//	}
//
//	s := SupabaseClient{Url: "database.com", Key: "myapikey"}
//	handler := s.curryToHandlerFunc(handleWithClient)
//
//	r := gin.New()
//	r.GET("/", handler)
func (s SupabaseClient) curryToHandlerFunc(f func(SupabaseClient, *gin.Context)) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		f(s, ctx)
	}
}
