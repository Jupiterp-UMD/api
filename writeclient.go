package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// WriteClient is the only thing in this service that holds the service-role
// key, and the only thing that issues anything other than GET.
//
// Choosing to put the write path in this API rather than in Supabase Edge
// Functions means the service key now lives in a public, internet-facing
// binary that had no auth code at all until recently. That was a deliberate
// trade -- one API surface, one deploy, one language -- but it converts a
// containment property into work, and this type is where that work is
// concentrated. Nothing outside this file should ever see `key`.
//
// Row-level security is the backstop, not the perimeter: the schema is written
// on the assumption that one of the handlers here is one day wrong.
type WriteClient struct {
	url  string
	key  string
	http *http.Client
}

func NewWriteClient(dbURL, serviceKey string) *WriteClient {
	return &WriteClient{
		url: dbURL,
		key: serviceKey,
		// Bounded, because a hung Supabase request otherwise holds a Cloud Run
		// request slot open until the platform kills it.
		http: &http.Client{Timeout: 15 * time.Second},
	}
}

// do issues a request against PostgREST with the service-role key.
func (w *WriteClient) do(method, path string, params url.Values, body any, prefer string) (*http.Response, error) {
	full := w.url + "/rest/v1/" + path
	if len(params) > 0 {
		full += "?" + params.Encode()
	}

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding request body: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, full, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("apikey", w.key)
	req.Header.Set("Authorization", "Bearer "+w.key)
	req.Header.Set("Content-Type", "application/json")
	if prefer != "" {
		req.Header.Set("Prefer", prefer)
	}
	return w.http.Do(req)
}

// decode runs a request and unmarshals the response into out.
func (w *WriteClient) decode(method, path string, params url.Values, body any, prefer string, out any) error {
	res, err := w.do(method, path, params, body, prefer)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	payload, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// The response body from PostgREST names the constraint that failed,
		// which is most of the diagnosis for a write.
		return fmt.Errorf("supabase %s %s: %d %s", method, path, res.StatusCode, string(payload))
	}
	if out == nil || len(payload) == 0 {
		return nil
	}
	return json.Unmarshal(payload, out)
}

// Select reads rows with the service role, bypassing row-level security.
//
// Used for the moderation queue and for looking up a review by token, neither
// of which the anon role can see -- correctly, since those rows are unapproved
// content and hashed identity data.
func (w *WriteClient) Select(table string, params url.Values, out any) error {
	return w.decode(http.MethodGet, table, params, nil, "", out)
}

// Insert writes rows and returns what the database stored.
func (w *WriteClient) Insert(table string, rows any, out any) error {
	return w.decode(http.MethodPost, table, nil, rows, "return=representation", out)
}

// Update patches rows matching params.
func (w *WriteClient) Update(table string, params url.Values, patch any, out any) error {
	return w.decode(http.MethodPatch, table, params, patch, "return=representation", out)
}

// Delete removes rows matching params.
func (w *WriteClient) Delete(table string, params url.Values) error {
	return w.decode(http.MethodDelete, table, params, nil, "", nil)
}

// DeleteReturning removes rows and decodes the ones it removed into out.
//
// Exists so a caller can report how many rows a purge actually touched.
// Without the representation there is nothing to count, and a maintenance
// endpoint that reports "1" for "the statement ran" tells an operator less
// than it appears to.
func (w *WriteClient) DeleteReturning(table string, params url.Values, out any) error {
	return w.decode(http.MethodDelete, table, params, nil, "return=representation", out)
}

// RPC calls a Postgres function.
func (w *WriteClient) RPC(fn string, args any, out any) error {
	return w.decode(http.MethodPost, "rpc/"+fn, nil, args, "", out)
}

// eq builds a PostgREST equality filter, which is most of what the write path
// needs and is easy to get subtly wrong by hand.
func eq(column, value string) url.Values {
	params := url.Values{}
	params.Set(column, "eq."+value)
	return params
}
