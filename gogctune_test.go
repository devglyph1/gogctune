package gogctune

import (
	"context"
	"log"
	"os"
	"testing"
	"time"
)

// TestClamp ensures the clamp function works as expected.
func TestClamp(t *testing.T) {
	testCases := []struct {
		name     string
		value    int
		min      int
		max      int
		expected int
	}{
		{"Value below min", 10, 20, 100, 20},
		{"Value above max", 120, 20, 100, 100},
		{"Value within range", 50, 20, 100, 50},
		{"Value at min", 20, 20, 100, 20},
		{"Value at max", 100, 20, 100, 100},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := clamp(tc.value, tc.min, tc.max)
			if result != tc.expected {
				t.Errorf("Expected clamp(%d, %d, %d) to be %d, but got %d", tc.value, tc.min, tc.max, tc.expected, result)
			}
		})
	}
}

// TestNewTuner validates the creation of a Tuner with default values.
func TestNewTuner(t *testing.T) {
	// Test with nil config values
	cfg := &Config{
		Policy: NewBalancedPolicy(nil),
	}
	tuner := New(cfg)

	if tuner.config.Interval != 10*time.Second {
		t.Errorf("Expected default interval to be 10s, but got %v", tuner.config.Interval)
	}

	if tuner.config.Logger != defaultLogger {
		t.Errorf("Expected default logger to be set")
	}

	// Test with specified values
	customLogger := log.New(os.Stdout, "", 0)
	cfg = &Config{
		Policy:   NewBalancedPolicy(nil),
		Interval: 5 * time.Second,
		Logger:   customLogger,
	}
	tuner = New(cfg)

	if tuner.config.Interval != 5*time.Second {
		t.Errorf("Expected interval to be 5s, but got %v", tuner.config.Interval)
	}

	if tuner.config.Logger != customLogger {
		t.Errorf("Expected custom logger to be set")
	}
}

// TestTunerLifecycle ensures the tuner can start and stop gracefully.
func TestTunerLifecycle(t *testing.T) {
	policy := NewBalancedPolicy(nil)
	tuner := New(&Config{
		Policy:   policy,
		Interval: 100 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := tuner.Tune(ctx)

	// Ensure it's running
	tuner.mu.Lock()
	if !tuner.isRunning {
		t.Fatal("Tuner should be running after Tune() is called")
	}
	tuner.mu.Unlock()

	// Stop the tuner
	stop()

	// Ensure it's stopped
	tuner.mu.Lock()
	if tuner.isRunning {
		t.Fatal("Tuner should be stopped after stop() is called")
	}
	tuner.mu.Unlock()
}

// TestPoliciesSanityCheck performs a basic sanity check on each policy.
func TestPoliciesSanityCheck(t *testing.T) {
	logger := log.New(os.Stderr, "[gogctune-test] ", log.LstdFlags)

	policies := map[string]struct {
		policy   Policy
		minClamp int
		maxClamp int
	}{
		"Balanced":    {NewBalancedPolicy(logger), 50, 200},
		"FavorCPU":    {NewFavorCPUPolicy(logger), 50, 500},
		"FavorMemory": {NewFavorMemoryPolicy(logger), 20, 150},
	}

	for name, data := range policies {
		t.Run(name, func(t *testing.T) {
			data.policy.Initialize()
			rec := data.policy.Recommend()
			if rec < data.minClamp || rec > data.maxClamp {
				t.Errorf("Policy '%s' recommended GOGC %d, which is outside its clamp range [%d, %d]",
					name, rec, data.minClamp, data.maxClamp)
			}
		})
	}
}
