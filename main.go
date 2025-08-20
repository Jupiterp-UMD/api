/*
This is the main package for the Jupiterp API binary. The Jupiterp API provides
access for external (non-Jupiterp) developers to Jupiterp data, including
courses and their sections compiled from Testudo, and instructor ratings
retrieved from PlanetTerp (these can be accessed directly via the PlanetTerp
API).

This binary uses the following environment variables:
  - DATABASE_URL (mandatory): The database URL to retrieve course, section,
    and instructor data from
  - DATABASE_KEY (mandatory): The database key used to access course, section,
    and instructor data
  - PORT (optional): The port to serve API on; default is 8080
*/
package main

import (
	"log"
	"os"

	"github.com/gin-gonic/gin"
)

type Course struct {
	course_code string
	name        string
	min_credits uint16
	max_credits uint16
	gen_eds     []string
	conditions  []string
	description string
}

type Instructor struct {
	slug           string
	name           string
	average_rating float32
}

type Section struct {
	course_code string
	sec_code    string
	instructors []string
	meetings    []string
	open_seats  uint16
	total_seats uint16
	waitlist    uint16
	holdfile    uint16
}

// Get value of `key` from environment vars. Fatal if `key` not present.
func mustEnv(key string) string {
	val := os.Getenv(key)
	if val == "" {
		log.Fatalf("missing required env var: %s", key)
	}
	return val
}

func main() {
	dbUrl := mustEnv("DATABASE_URL")
	dbKey := mustEnv("DATABASE_KEY")
	port := os.Getenv("PORT")

	if port == "" {
		port = "8080"
		log.Printf("Defaulting to port %s", port)
	}

	// Initialize Gin instance and middleware
	r := gin.New()
	r.Use(gin.Recovery())
	// TODO: Add logger, CORS, auth with keys

	// Create SupabaseClient to connect with DB
	client := SupabaseClient{Url: dbUrl, Key: dbKey}

	// TODO: Use actual handlers
	handler := client.curryToHandlerFunc(handle)

	// Define handlers
	r.GET("/", handler)

	// Listen and serve on defined port
	log.Printf("Listening on port %s", port)
	r.Run(":" + port)
}
