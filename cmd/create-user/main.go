package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/lucifer1708/object_storage_manager/db"
	"github.com/lucifer1708/object_storage_manager/handlers"
	"golang.org/x/term"
)

func main() {
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "./data/osm.db"
	}

	if err := db.Init(dbPath); err != nil {
		fatalf("Failed to open database: %v\n", err)
	}

	fmt.Println("─── Create User ────────────────────────────────")

	username := prompt("Username: ")
	if len(strings.TrimSpace(username)) < 2 {
		fatalf("Username must be at least 2 characters\n")
	}

	// Check if username already exists
	existing, _ := db.GetUserByUsername(username)
	if existing != nil {
		fatalf("User %q already exists\n", username)
	}

	password := promptPassword("Password (min 8 chars): ")
	if len(password) < 8 {
		fatalf("Password must be at least 8 characters\n")
	}
	confirm := promptPassword("Confirm password: ")
	if password != confirm {
		fatalf("Passwords do not match\n")
	}

	hash, err := handlers.GenPasswordHash(password)
	if err != nil {
		fatalf("Failed to hash password: %v\n", err)
	}

	user, err := db.CreateUser(strings.TrimSpace(username), hash)
	if err != nil {
		fatalf("Failed to create user: %v\n", err)
	}

	fmt.Printf("\n✓ User %q created (id=%d)\n", user.Username, user.ID)
	fmt.Println("  On next login, you will be prompted to set up 2FA.")
}

func prompt(label string) string {
	fmt.Print(label)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	return strings.TrimSpace(scanner.Text())
}

func promptPassword(label string) string {
	fmt.Print(label)
	b, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Println()
	if err != nil {
		// fallback for non-TTY
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Scan()
		return strings.TrimSpace(scanner.Text())
	}
	return string(b)
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "Error: "+format, args...)
	os.Exit(1)
}
