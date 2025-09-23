package example

import (
	"context"
	"log"
	"os"
	"runtime"
	"time"

	"github.com/devglyph1/gogctune" // Use your actual module path
)

// This example program demonstrates how to use the gogctune package.
// It starts a tuner with a chosen policy and then runs a simulated workload
// that allocates memory in spikes to show how the tuner adjusts GOGC in real-time.
func example() {
	// Create a context that can be cancelled to stop the application gracefully.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// It's recommended to pass a logger to the policy to see its decisions.
	logger := log.New(os.Stderr, "[gogctune-example] ", log.LstdFlags)
	logger.Println("Starting gogctune example application...")

	// --- Choose and start the tuner ---
	// You can switch between different policies to see how they behave.
	// A real application would configure this via flags or a config file.
	policy := gogctune.NewBalancedPolicy(logger)
	// policy := gogctune.NewFavorMemoryPolicy(logger)
	// policy := gogctune.NewFavorCPUPolicy(logger)

	// Start the tuner. It runs in a background goroutine.
	stopTuner, err := gogctune.Start(ctx, policy)
	if err != nil {
		logger.Fatalf("Failed to start gogctune: %v", err)
	}
	// It's crucial to defer the stop function to ensure a graceful shutdown.
	defer stopTuner()

	logger.Printf("gogctune started with '%s' policy. Simulating workload...", policy.Name())

	// Start a goroutine to print memory stats periodically.
	go printMemStats(ctx)

	// Simulate a workload that allocates memory in bursts for a few minutes.
	simulateWorkload(ctx, 2*time.Minute)

	logger.Println("Workload finished. Application will exit.")
}

// simulateWorkload creates memory pressure to trigger GC and demonstrate the tuner's behavior.
func simulateWorkload(ctx context.Context, duration time.Duration) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	// Stop the simulation after the specified duration.
	simulationDone := time.After(duration)

	for {
		select {
		case <-ticker.C:
			log.Println("--- Allocating memory spike ---")
			// Allocate a large slice to increase heap size and trigger GC.
			// We hold onto it for a short time to simulate a workload.
			spike := make([][]byte, 1000)
			for i := 0; i < 1000; i++ {
				spike[i] = make([]byte, 50*1024) // 50 KB * 1000 = 50 MB
			}
			_ = spike // Keep the variable alive

			// Sleep for a bit to simulate processing time
			time.Sleep(1 * time.Second)
			// The 'spike' slice goes out of scope here and can be garbage collected.

		case <-ctx.Done():
			log.Println("Workload simulation cancelled.")
			return
		case <-simulationDone:
			log.Println("Workload simulation finished.")
			return
		}
	}
}

// printMemStats logs relevant memory statistics every few seconds.
func printMemStats(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			log.Printf("HeapAlloc = %d MiB, NumGC = %d", m.HeapAlloc/1024/1024, m.NumGC)
		case <-ctx.Done():
			return
		}
	}
}
