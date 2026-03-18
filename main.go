package main

import (
	"log"
	"net/http"
	"os"

	"github.com/joho/godotenv"
	"github.com/lucifer1708/object_storage_manager/db"
	"github.com/lucifer1708/object_storage_manager/handlers"
)

func main() {
	// Load .env — existing real env vars are never overwritten
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		log.Printf("warning: .env not loaded: %v", err)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "./data/osm.db"
	}
	if err := db.Init(dbPath); err != nil {
		log.Fatalf("database init failed: %v", err)
	}
	log.Printf("Database: %s", dbPath)

	handlers.InitAuth()
	handlers.AutoConnect()

	mux := http.NewServeMux()

	// Static (always public)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	// First-run setup
	mux.HandleFunc("GET /setup", handlers.SetupPage)
	mux.HandleFunc("POST /setup", handlers.SetupSubmit)

	// Auth
	mux.HandleFunc("GET /login", handlers.LoginPage)
	mux.HandleFunc("POST /login", handlers.LoginSubmit)
	mux.HandleFunc("GET /login/totp", handlers.TOTPVerifyPage)
	mux.HandleFunc("POST /login/totp", handlers.TOTPVerifySubmit)
	mux.HandleFunc("GET /logout", handlers.Logout)

	// TOTP setup & settings
	mux.HandleFunc("GET /totp/setup", handlers.TOTPSetupPage)
	mux.HandleFunc("POST /totp/setup/verify", handlers.TOTPSetupVerify)
	mux.HandleFunc("GET /settings", handlers.SettingsPage)
	mux.HandleFunc("POST /settings/password", handlers.ChangePassword)
	mux.HandleFunc("POST /settings/totp/reset", handlers.ResetTOTP)

	// Storage app
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

	log.Printf("Object Storage Manager → http://localhost:%s", port)
	if err := http.ListenAndServe(":"+port, handlers.AuthMiddleware(mux)); err != nil {
		log.Fatal(err)
	}
}

