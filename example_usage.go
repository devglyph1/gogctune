package example

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/devglyph1/gogctune"
)

// This example demonstrates integrating gogctune into a more realistic
// application: a simple web server that occasionally allocates memory.
func example() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.New(os.Stderr, "[gogctune-server] ", log.LstdFlags)

	// --- Start gogctune ---
	// Choose the policy that best fits the server's expected load.
	// For a general-purpose web server, 'Balanced' is a good start.
	policy := gogctune.NewBalancedPolicy(logger)
	stopTuner, err := gogctune.Start(ctx, policy)
	if err != nil {
		logger.Fatalf("Could not start gogctune: %v", err)
	}
	defer stopTuner()

	logger.Printf("gogctune started with '%s' policy.", policy.Name())

	// --- Configure and start the web server ---

	server := &http.Server{Addr: ":8080"}
	logger.Println("Starting web server on http://localhost:8080")

	// Start the server in a separate goroutine.
	go func() {
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("ListenAndServe(): %v", err)
		}
	}()

	// Wait for context cancellation (e.g., from an interrupt signal)
	<-ctx.Done()

	// Shutdown the server gracefully.
	logger.Println("Shutting down server...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Fatalf("Server shutdown failed: %v", err)
	}
	logger.Println("Server gracefully stopped.")
}
