package gogctune

import (
	"log"
	"os"
	"runtime"
	"runtime/debug"
	"testing"
	"time"
)

func newLogger() *log.Logger {
	return log.New(os.Stdout, "[test] ", log.LstdFlags)
}

//
// ==== BalancedPolicy Tests ====
//

func TestBalancedPolicy_CPUHigh_IncreasesGOGC(t *testing.T) {
	p := NewBalancedPolicy(newLogger())

	// Pretend previous stats
	p.state.prevGCStats = debug.GCStats{LastGC: time.Now().Add(-1 * time.Second), PauseTotal: 10 * time.Millisecond}
	// Current stats -> high CPU fraction
	currGC := debug.GCStats{LastGC: time.Now(), PauseTotal: 200 * time.Millisecond}

	newGOGC := p.NextGOGC(100, runtime.MemStats{HeapAlloc: 100 << 20}, currGC)
	if newGOGC <= 100 {
		t.Errorf("expected GOGC increase, got %d", newGOGC)
	}
}

func TestBalancedPolicy_HighHeapGrowth_DecreasesGOGC(t *testing.T) {
	p := NewBalancedPolicy(newLogger())

	p.state.prevMemStats = runtime.MemStats{HeapAlloc: 100 << 20}
	currMem := runtime.MemStats{HeapAlloc: 200 << 20}

	newGOGC := p.NextGOGC(100, currMem, debug.GCStats{})
	if newGOGC >= 100 {
		t.Errorf("expected GOGC decrease, got %d", newGOGC)
	}
}

func TestBalancedPolicy_Idle_DecreasesSlowly(t *testing.T) {
	p := NewBalancedPolicy(newLogger())

	// Simulate very low GC CPU fraction
	p.state.emaGCCPUFraction = 0.0
	p.state.prevGCStats = debug.GCStats{LastGC: time.Now().Add(-10 * time.Second)}

	newGOGC := p.NextGOGC(100, runtime.MemStats{}, debug.GCStats{LastGC: time.Now()})
	if newGOGC != 98 { // 100 - GOGCSlowDecStep
		t.Errorf("expected slow decrease, got %d", newGOGC)
	}
}

//
// ==== FavorMemoryPolicy Tests ====
//

func TestFavorMemoryPolicy_AggressiveDecrease(t *testing.T) {
	p := NewFavorMemoryPolicy(newLogger())

	newGOGC := p.NextGOGC(100, runtime.MemStats{}, debug.GCStats{})
	if newGOGC >= 100 {
		t.Errorf("expected GOGC decrease, got %d", newGOGC)
	}
}

func TestFavorMemoryPolicy_EmergencyCPU(t *testing.T) {
	p := NewFavorMemoryPolicy(newLogger())

	// Simulate high CPU fraction for multiple intervals
	for i := 0; i < p.EmergencySustain; i++ {
		p.state.prevGCStats = debug.GCStats{LastGC: time.Now().Add(-1 * time.Second), PauseTotal: 10 * time.Millisecond}
		currGC := debug.GCStats{LastGC: time.Now(), PauseTotal: 500 * time.Millisecond}
		newGOGC := p.NextGOGC(50, runtime.MemStats{}, currGC)

		if i == p.EmergencySustain-1 && newGOGC != p.EmergencyGOGCValue {
			t.Errorf("expected emergency GOGC=%d, got %d", p.EmergencyGOGCValue, newGOGC)
		}
	}
}

//
// ==== FavorCPUPolicy Tests ====
//

func TestFavorCPUPolicy_IncreaseAggressively(t *testing.T) {
	p := NewFavorCPUPolicy(newLogger())

	newGOGC := p.NextGOGC(100, runtime.MemStats{HeapAlloc: 10 << 20}, debug.GCStats{})
	if newGOGC <= 100 {
		t.Errorf("expected GOGC increase, got %d", newGOGC)
	}
}

func TestFavorCPUPolicy_OOMPrevention(t *testing.T) {
	p := NewFavorCPUPolicy(newLogger())
	p.totalMem = 100 << 20                     // 100 MB total memory
	m := runtime.MemStats{HeapAlloc: 90 << 20} // 90% usage

	newGOGC := p.NextGOGC(200, m, debug.GCStats{})
	if newGOGC != p.OOMPreventionGOGC {
		t.Errorf("expected OOM prevention GOGC=%d, got %d", p.OOMPreventionGOGC, newGOGC)
	}
}

func TestFavorCPUPolicy_TimeBasedTrigger(t *testing.T) {
	p := NewFavorCPUPolicy(newLogger())
	p.totalMem = 1 << 30 // 1GB
	p.state.lastGCTime = time.Now().Add(-10 * time.Minute)

	newGOGC := p.NextGOGC(200, runtime.MemStats{HeapAlloc: 100 << 20}, debug.GCStats{})
	if newGOGC != 100 {
		t.Errorf("expected time-based trigger GOGC=100, got %d", newGOGC)
	}
}
