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

// Get value of `key` from environment vars. Fatal if `key` not present.
func mustEnv(key string) string {
	val := os.Getenv(key)
	if val == "" {
		log.Fatalf("missing required env var: %s", key)
	}
	return val
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.LUTC | log.Lshortfile)

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

	/* ============================== ROUTES =============================== */
	v1 := r.Group("/v1")
	v1.GET("/", client.handleBaseEndpoint)

	v1.GET("/course", client.handleGetCourse)                 // single course
	v1.GET("/courses", client.handleGetCourses)               // multiple full courses
	v1.GET("/courses/minified", client.handleMinifiedCourses) // minified courses

	// Listen and serve on defined port
	log.Printf("Listening on port %s", port)
	r.Run(":" + port)
}
