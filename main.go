package main

import (
	"bufio"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/lucifer1708/object_storage_manager/handlers"
)

func main() {
	loadEnv(".env")

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	handlers.InitAuth()
	handlers.AutoConnect()

	mux := http.NewServeMux()

	// Static files (public)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	// Auth routes (public)
	mux.HandleFunc("GET /login", handlers.LoginPage)
	mux.HandleFunc("POST /login", handlers.LoginSubmit)
	mux.HandleFunc("GET /login/totp", handlers.TOTPVerifyPage)
	mux.HandleFunc("POST /login/totp", handlers.TOTPVerifySubmit)
	mux.HandleFunc("GET /logout", handlers.Logout)

	// TOTP setup (requires full session)
	mux.HandleFunc("GET /totp/setup", handlers.TOTPSetupPage)
	mux.HandleFunc("POST /totp/setup/verify", handlers.TOTPSetupVerify)

	// Protected app routes
	mux.HandleFunc("GET /{$}", handlers.Index)
	mux.HandleFunc("POST /connect", handlers.Connect)
	mux.HandleFunc("GET /disconnect", handlers.Disconnect)

	mux.HandleFunc("GET /buckets", handlers.ListBuckets)
	mux.HandleFunc("POST /buckets", handlers.CreateBucket)
	mux.HandleFunc("DELETE /buckets/{bucket}", handlers.DeleteBucket)

	mux.HandleFunc("GET /buckets/{bucket}/objects", handlers.ListObjects)
	mux.HandleFunc("POST /buckets/{bucket}/upload", handlers.UploadObject)
	mux.HandleFunc("GET /buckets/{bucket}/download/{key...}", handlers.DownloadObject)
	mux.HandleFunc("DELETE /buckets/{bucket}/objects/{key...}", handlers.DeleteObject)
	mux.HandleFunc("GET /buckets/{bucket}/presign/{key...}", handlers.PresignObject)
	mux.HandleFunc("POST /buckets/{bucket}/folder", handlers.CreateFolder)
	mux.HandleFunc("GET /buckets/{bucket}/preview/{key...}", handlers.PreviewObject)
	mux.HandleFunc("POST /buckets/{bucket}/copy", handlers.CopyObject)
	mux.HandleFunc("GET /search", handlers.SearchObjects)

	// Wrap all routes with auth middleware
	protected := handlers.AuthMiddleware(mux)

	log.Printf("Object Storage Manager running on http://localhost:%s", port)
	if err := http.ListenAndServe(":"+port, protected); err != nil {
		log.Fatal(err)
	}
}

// loadEnv reads key=value pairs from a file and sets them as env vars.
// Existing env vars are NOT overwritten (real env always wins over .env).
func loadEnv(filename string) {
	f, err := os.Open(filename)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = value[1 : len(value)-1]
		}
		if os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
	}
}
