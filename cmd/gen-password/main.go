package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/lucifer1708/object_storage_manager/handlers"
)

func main() {
	fmt.Print("Enter password: ")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	password := strings.TrimSpace(scanner.Text())
	if password == "" {
		fmt.Fprintln(os.Stderr, "Error: password cannot be empty")
		os.Exit(1)
	}

	hash, err := handlers.GenPasswordHash(password)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\nAdd this to your .env file:")
	fmt.Printf("AUTH_PASSWORD_HASH=%s\n", hash)
}
