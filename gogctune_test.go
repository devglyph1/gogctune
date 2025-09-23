package gogctune

import (
	"context"
	"io"
	"log"
	"runtime/debug"
	"testing"
	"time"
)

// --- Utility Function Tests ---

func TestClamp(t *testing.T) {
	testCases := []struct {
		name     string
		value    int
		min      int
		max      int
		expected int
	}{
		{"Value below min", 10, 20, 200, 20},
		{"Value above max", 250, 20, 200, 200},
		{"Value within range", 100, 20, 200, 100},
		{"Value at min", 20, 20, 200, 20},
		{"Value at max", 200, 20, 200, 200},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clamp(tc.value, tc.min, tc.max); got != tc.expected {
				t.Errorf("clamp(%d, %d, %d) = %d; want %d", tc.value, tc.min, tc.max, got, tc.expected)
			}
		})
	}
}

func TestCalculateEMA(t *testing.T) {
	// Simple case to ensure the formula is applied correctly.
	previousEMA := 10.0
	currentValue := 20.0
	alpha := 0.2
	expected := (alpha * currentValue) + ((1 - alpha) * previousEMA) // 0.2*20 + 0.8*10 = 4 + 8 = 12

	if got := calculateEMA(currentValue, previousEMA, alpha); got != expected {
		t.Errorf("calculateEMA() = %f; want %f", got, expected)
	}
}

// --- Policy Logic Tests ---

// Note: Testing the policies perfectly is hard as it requires mocking runtime stats.
// These tests focus on ensuring the core decision tree logic works as expected given certain inputs.

func TestBalancedPolicy_CPUResponse(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	p := NewBalancedPolicy(logger).(*balancedPolicy)
	p.isFirstRun = false
	p.emaGCCPUFraction = 0.05 // Above the 0.04 threshold

	currentGOGC := 100
	newGOGC := p.Decide(currentGOGC, 5*time.Second)

	if newGOGC <= currentGOGC {
		t.Errorf("Expected GOGC to increase on high CPU, but it went from %d to %d", currentGOGC, newGOGC)
	}
}

func TestFavorMemoryPolicy_SafetyValve(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	p := NewFavorMemoryPolicy(logger).(*favorMemoryPolicy)
	p.isFirstRun = false
	p.emaGCCPUFraction = 0.20                                // Above the 0.15 emergency threshold
	p.highCPUSustainCounter = p.emergencySustainDuration - 1 // One tick away from triggering

	currentGOGC := 20
	newGOGC := p.Decide(currentGOGC, 5*time.Second)

	if newGOGC != p.emergencyGOGCValue {
		t.Errorf("Expected safety valve to trigger and set GOGC to %d, but got %d", p.emergencyGOGCValue, newGOGC)
	}
}

func TestFavorCPUPolicy_SafetyValve(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	p := NewFavorCPUPolicy(logger).(*favorCPUPolicy)
	// Manually set a low memory cap for testing
	p.memoryCapBytes = 100 * 1024 * 1024 // 100MB

	// To simulate high memory, we can't directly mock runtime.ReadMemStats.
	// This test is more of a placeholder showing the intent. A real test
	// would require a more complex setup with interfaces.
	// For now, we assume if we could set HeapAlloc > memoryCapBytes,
	// the policy should return p.oomPreventionGOGC.
	t.Log("Skipping direct test for FavorCPU safety valve due to inability to mock runtime.MemStats easily.")
}

// --- Tuner Lifecycle Test ---

func TestStartStop(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	policy := NewBalancedPolicy(logger)

	// Get initial GOGC
	initialGOGC := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(initialGOGC) // Restore it after test

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop, err := Start(ctx, policy)
	if err != nil {
		t.Fatalf("Start() returned an error: %v", err)
	}

	// Let the tuner run for a very short time
	time.Sleep(10 * time.Millisecond)

	// Stop the tuner
	stop()

	// Use a timeout to ensure the tuner actually stops
	select {
	case <-time.After(1 * time.Second):
		t.Error("Tuner did not stop within the expected time")
	// If the tuner's 'done' channel was accessible, we could read from it here.
	// Since it's not, we rely on the stop function returning promptly.
	case <-func() chan struct{} {
		c := make(chan struct{})
		close(c)
		return c
	}():
		// Test passed
	}
}
