package gogctune

import (
	"context"
	"log"
	"math"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"github.com/shirou/gopsutil/mem"
)

//
// ==== Interfaces & Common Structures ====
//

// Policy defines the interface all tuning policies must implement.
type Policy interface {
	NextGOGC(currentGOGC int, m runtime.MemStats, g debug.GCStats) int
	Name() string
}

// State holds historical data and EMA-smoothed metrics.
type State struct {
	prevMemStats runtime.MemStats
	prevGCStats  debug.GCStats

	emaGCCPUFraction float64
	emaHeapGrowth    float64

	lastAction     string // "increase", "decrease", or ""
	lastActionTime time.Time

	// For memory/cpu safety valves
	highCPUCounter int
	lastGCTime     time.Time
}

//
// ==== Utility functions ====
//

// ema updates the exponential moving average.
func ema(oldEMA, value, alpha float64) float64 {
	return (alpha * value) + ((1 - alpha) * oldEMA)
}

// clamp bounds x between min and max.
func clamp(x, min, max int) int {
	if x < min {
		return min
	}
	if x > max {
		return max
	}
	return x
}

// gcCPUFraction calculates the CPU fraction spent in GC for the interval.
func gcCPUFraction(prev, curr debug.GCStats) float64 {
	dur := curr.PauseTotal - prev.PauseTotal
	elapsed := curr.LastGC.Sub(prev.LastGC)
	if elapsed <= 0 {
		return 0
	}
	return float64(dur) / float64(elapsed)
}

// heapGrowth calculates heap growth percentage.
func heapGrowth(prev, curr runtime.MemStats) float64 {
	if prev.HeapAlloc == 0 {
		return 0
	}
	return float64(curr.HeapAlloc-prev.HeapAlloc) / float64(prev.HeapAlloc)
}

//
// ==== Balanced Policy ====
//

type BalancedPolicy struct {
	logger *log.Logger
	state  *State

	TargetCPUFractionLow  float64
	TargetCPUFractionHigh float64
	HighHeapGrowthThresh  float64
	GOGCIncreaseFactor    float64
	GOGCModerateDecStep   int
	GOGCSlowDecStep       int
	CooldownCycles        int
	MinGOGC               int
	MaxGOGC               int

	cycleCount int
}

func NewBalancedPolicy(logger *log.Logger) *BalancedPolicy {
	return &BalancedPolicy{
		logger:                logger,
		state:                 &State{},
		TargetCPUFractionLow:  0.02,
		TargetCPUFractionHigh: 0.04,
		HighHeapGrowthThresh:  0.20,
		GOGCIncreaseFactor:    200.0,
		GOGCModerateDecStep:   10,
		GOGCSlowDecStep:       2,
		CooldownCycles:        3,
		MinGOGC:               20,
		MaxGOGC:               250,
	}
}

func (p *BalancedPolicy) Name() string { return "BalancedPolicy" }

func (p *BalancedPolicy) NextGOGC(currentGOGC int, m runtime.MemStats, g debug.GCStats) int {
	// Calculate metrics
	cpuFrac := gcCPUFraction(p.state.prevGCStats, g)
	heapGrowth := heapGrowth(p.state.prevMemStats, m)

	// Smooth
	p.state.emaGCCPUFraction = ema(p.state.emaGCCPUFraction, cpuFrac, 0.2)
	p.state.emaHeapGrowth = ema(p.state.emaHeapGrowth, heapGrowth, 0.2)

	newGOGC := currentGOGC
	action := ""

	// CPU-First Response
	if p.state.emaGCCPUFraction > p.TargetCPUFractionHigh {
		overshoot := p.state.emaGCCPUFraction - p.TargetCPUFractionHigh
		newGOGC = currentGOGC + int(overshoot*p.GOGCIncreaseFactor)
		action = "increase"
		p.cycleCount = 0
	} else if p.state.emaHeapGrowth > p.HighHeapGrowthThresh {
		// Memory response
		if !(p.state.lastAction == "increase" && p.cycleCount < p.CooldownCycles) {
			newGOGC = currentGOGC - p.GOGCModerateDecStep
			action = "decrease"
			p.cycleCount = 0
		}
	} else if p.state.emaGCCPUFraction < p.TargetCPUFractionLow {
		// Idle optimization
		if !(p.state.lastAction == "increase" && p.cycleCount < p.CooldownCycles) {
			newGOGC = currentGOGC - p.GOGCSlowDecStep
			action = "decrease"
			p.cycleCount = 0
		}
	} else {
		p.cycleCount++
	}

	// Save state
	p.state.prevMemStats = m
	p.state.prevGCStats = g
	if action != "" {
		p.state.lastAction = action
		p.state.lastActionTime = time.Now()
	}

	return clamp(newGOGC, p.MinGOGC, p.MaxGOGC)
}

//
// ==== FavorMemory Policy ====
//

type FavorMemoryPolicy struct {
	logger *log.Logger
	state  *State

	MinGOGC            int
	AggressiveDecStep  int
	EmergencyCPUThresh float64
	EmergencyGOGCValue int
	EmergencySustain   int
}

func NewFavorMemoryPolicy(logger *log.Logger) *FavorMemoryPolicy {
	return &FavorMemoryPolicy{
		logger:             logger,
		state:              &State{},
		MinGOGC:            20,
		AggressiveDecStep:  15,
		EmergencyCPUThresh: 0.15,
		EmergencyGOGCValue: 150,
		EmergencySustain:   3,
	}
}

func (p *FavorMemoryPolicy) Name() string { return "FavorMemoryPolicy" }

func (p *FavorMemoryPolicy) NextGOGC(currentGOGC int, m runtime.MemStats, g debug.GCStats) int {
	cpuFrac := gcCPUFraction(p.state.prevGCStats, g)
	p.state.emaGCCPUFraction = ema(p.state.emaGCCPUFraction, cpuFrac, 0.2)

	newGOGC := currentGOGC

	if p.state.emaGCCPUFraction > p.EmergencyCPUThresh {
		p.state.highCPUCounter++
		if p.state.highCPUCounter >= p.EmergencySustain {
			newGOGC = p.EmergencyGOGCValue
			p.state.highCPUCounter = 0
		}
	} else {
		p.state.highCPUCounter = 0
		newGOGC = currentGOGC - p.AggressiveDecStep
	}

	p.state.prevMemStats = m
	p.state.prevGCStats = g
	return clamp(newGOGC, p.MinGOGC, math.MaxInt32)
}

//
// ==== FavorCPU Policy ====
//

type FavorCPUPolicy struct {
	logger *log.Logger
	state  *State

	MaxGOGC           int
	AggressiveIncStep int
	MemoryCapPercent  float64
	OOMPreventionGOGC int
	MaxGCInterval     time.Duration

	totalMem uint64
	once     sync.Once
}

func NewFavorCPUPolicy(logger *log.Logger) *FavorCPUPolicy {
	return &FavorCPUPolicy{
		logger:            logger,
		state:             &State{},
		MaxGOGC:           250,
		AggressiveIncStep: 20,
		MemoryCapPercent:  0.80,
		OOMPreventionGOGC: 25,
		MaxGCInterval:     5 * time.Minute,
	}
}

func (p *FavorCPUPolicy) Name() string { return "FavorCPUPolicy" }

func (p *FavorCPUPolicy) NextGOGC(currentGOGC int, m runtime.MemStats, g debug.GCStats) int {
	// Get total system memory once
	p.once.Do(func() {
		if vm, err := mem.VirtualMemory(); err == nil {
			p.totalMem = vm.Total
		} else {
			p.totalMem = 8 << 30 // fallback 8GB
		}
	})

	newGOGC := currentGOGC
	memoryCap := uint64(float64(p.totalMem) * p.MemoryCapPercent)

	if m.HeapAlloc > memoryCap {
		// OOM prevention
		newGOGC = p.OOMPreventionGOGC
	} else if !p.state.lastGCTime.IsZero() && time.Since(p.state.lastGCTime) > p.MaxGCInterval {
		newGOGC = 100
	} else {
		newGOGC = currentGOGC + p.AggressiveIncStep
	}

	if len(g.PauseEnd) > 0 {
		p.state.lastGCTime = g.LastGC
	}
	p.state.prevMemStats = m
	p.state.prevGCStats = g
	return clamp(newGOGC, 0, p.MaxGOGC)
}

//
// ==== Runner ====
//

// Start begins the GC tuning loop with the given policy.
func Start(ctx context.Context, policy Policy) (func(), error) {
	ticker := time.NewTicker(5 * time.Second)
	done := make(chan struct{})

	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				close(done)
				return
			case <-ticker.C:
				var m runtime.MemStats
				var g debug.GCStats
				runtime.ReadMemStats(&m)
				debug.ReadGCStats(&g)

				currentGOGC := debug.SetGCPercent(-1) // read current
				newGOGC := policy.NextGOGC(currentGOGC, m, g)

				if newGOGC != currentGOGC {
					debug.SetGCPercent(newGOGC)
				}
			}
		}
	}()

	stop := func() { <-done }
	return stop, nil
}
