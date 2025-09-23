package gogctune

import (
	"log"
	"os"
	"testing"
	"time"
)

// --- Test Utility Functions ---

func TestClamp(t *testing.T) {
	testCases := []struct {
		name     string
		value    int
		min      int
		max      int
		expected int
	}{
		{"ValueBelowMin", 10, 20, 200, 20},
		{"ValueAboveMax", 300, 20, 200, 200},
		{"ValueWithinRange", 100, 20, 200, 100},
		{"ValueAtMin", 20, 20, 200, 20},
		{"ValueAtMax", 200, 20, 200, 200},
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

func TestCalculateP95(t *testing.T) {
	testCases := []struct {
		name     string
		data     []time.Duration
		expected time.Duration
	}{
		{"EmptySlice", []time.Duration{}, 0},
		{"SingleElement", []time.Duration{100}, 100},
		{"MultipleElements", []time.Duration{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}, 20},
		{"UnsortedElements", []time.Duration{10, 5, 15, 20, 1}, 20},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := calculateP95(tc.data)
			if result != tc.expected {
				t.Errorf("Expected p95 to be %v, but got %v", tc.expected, result)
			}
		})
	}
}

// --- Test Policies ---

func TestBalancedPolicy(t *testing.T) {
	logger := log.New(os.Stderr, "[test-balanced] ", log.LstdFlags)
	policy := NewBalancedPolicy(logger).(*balancedPolicy)
	policy.targetPause = 10 * time.Millisecond

	t.Run("PauseTooHigh_ShouldIncreaseGOGC", func(t *testing.T) {
		stats := &CycleStats{
			CurrentGOGC: 100,
			Pause:       20 * time.Millisecond,
		}
		// Fill history with high pauses
		for i := 0; i < policy.historySize; i++ {
			policy.pauses = append(policy.pauses, 20*time.Millisecond)
		}

		newGOGC := policy.Decide(stats)
		if newGOGC <= 100 {
			t.Errorf("Expected GOGC to increase from 100 when pause is high, but got %d", newGOGC)
		}
	})

	t.Run("PauseTooLow_ShouldDecreaseGOGC", func(t *testing.T) {
		stats := &CycleStats{
			CurrentGOGC: 100,
			Pause:       5 * time.Millisecond,
		}
		// Reset PID and history
		policy.pid = pidController{Kp: 0.5, Ki: 0.1, Kd: 0.2}
		policy.pauses = []time.Duration{}
		// Fill history with low pauses
		for i := 0; i < policy.historySize; i++ {
			policy.pauses = append(policy.pauses, 5*time.Millisecond)
		}

		newGOGC := policy.Decide(stats)
		if newGOGC >= 100 {
			t.Errorf("Expected GOGC to decrease from 100 when pause is low, but got %d", newGOGC)
		}
	})
}

func TestFavorMemoryPolicy(t *testing.T) {
	logger := log.New(os.Stderr, "[test-favormemory] ", log.LstdFlags)
	policy := NewFavorMemoryPolicy(logger).(*favorMemoryPolicy)
	policy.targetHeapUtilization = 0.6
	policy.emergencyPause = 100 * time.Millisecond

	t.Run("EmergencyPause_ShouldIncreaseGOGC", func(t *testing.T) {
		stats := &CycleStats{
			CurrentGOGC: 100,
			Pause:       150 * time.Millisecond, // Exceeds emergency threshold
		}
		newGOGC := policy.Decide(stats)
		if newGOGC != 150 {
			t.Errorf("Expected GOGC to be set to emergency value 150, but got %d", newGOGC)
		}
	})

	t.Run("HeapUtilizationTooHigh_ShouldDecreaseGOGC", func(t *testing.T) {
		stats := &CycleStats{
			CurrentGOGC: 100,
			Pause:       50 * time.Millisecond,
			HeapAlloc:   80, // 80% utilization
			HeapGoal:    100,
		}
		newGOGC := policy.Decide(stats)
		if newGOGC >= 100 {
			t.Errorf("Expected GOGC to decrease when heap utilization is high, but got %d", newGOGC)
		}
	})

	t.Run("HeapUtilizationTooLow_ShouldIncreaseGOGC", func(t *testing.T) {
		stats := &CycleStats{
			CurrentGOGC: 100,
			Pause:       50 * time.Millisecond,
			HeapAlloc:   40, // 40% utilization
			HeapGoal:    100,
		}
		// Reset PID
		policy.pid = pidController{Kp: 50.0, Ki: 5.0, Kd: 10.0}
		newGOGC := policy.Decide(stats)
		if newGOGC <= 100 {
			t.Errorf("Expected GOGC to increase when heap utilization is low, but got %d", newGOGC)
		}
	})
}

func TestFavorCPUPolicy(t *testing.T) {
	logger := log.New(os.Stderr, "[test-favorcpu] ", log.LstdFlags)
	policy := NewFavorCPUPolicy(logger).(*favorCPUPolicy)
	policy.memoryCapBytes = 1000
	policy.oomPreventionGOGC = 25
	policy.targetInterval = 1 * time.Minute

	t.Run("MemoryCapExceeded_ShouldForceGC", func(t *testing.T) {
		stats := &CycleStats{
			CurrentGOGC: 200,
			HeapAlloc:   1200, // Exceeds memory cap
		}
		newGOGC := policy.Decide(stats)
		if newGOGC != policy.oomPreventionGOGC {
			t.Errorf("Expected GOGC to be set to OOM prevention value %d, but got %d", policy.oomPreventionGOGC, newGOGC)
		}
	})

	t.Run("GCIntervalTooShort_ShouldIncreaseGOGC", func(t *testing.T) {
		stats := &CycleStats{
			CurrentGOGC:   100,
			HeapAlloc:     500,
			CycleInterval: 30 * time.Second, // Shorter than target
		}
		newGOGC := policy.Decide(stats)
		if newGOGC <= 100 {
			t.Errorf("Expected GOGC to increase when GC interval is too short, but got %d", newGOGC)
		}
	})

	t.Run("GCIntervalTooLong_ShouldDecreaseGOGC", func(t *testing.T) {
		stats := &CycleStats{
			CurrentGOGC:   100,
			HeapAlloc:     500,
			CycleInterval: 90 * time.Second, // Longer than target
		}
		// Reset PID
		policy.pid = pidController{Kp: 0.1, Ki: 0.01, Kd: 0.05}
		newGOGC := policy.Decide(stats)
		if newGOGC >= 100 {
			t.Errorf("Expected GOGC to decrease when GC interval is too long, but got %d", newGOGC)
		}
	})
}
