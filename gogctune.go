package gogctune

import (
	"context"
	"log"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/shirou/gopsutil/v3/mem" // Required for FavorCPU policy's memory cap
)

// --- Constants and Default Configuration ---

const (
	// Default interval for the tuner to make decisions.
	defaultDecisionInterval = 5 * time.Second
	// Default smoothing factor (alpha) for Exponential Moving Average.
	defaultEMASmoothingFactor = 0.2
	// Default safe minimum and maximum for GOGC values.
	defaultMinGOGC = 20
	defaultMaxGOGC = 500
)

// --- Core Tuner Components ---

// Policy defines the interface for a tuning strategy.
type Policy interface {
	// Name returns the name of the policy.
	Name() string
	// Decide makes a tuning decision based on the current and previous stats.
	Decide(currentGOGC int, interval time.Duration) int
	// SetLogger sets the logger for the policy.
	SetLogger(logger *log.Logger)
}

// tuner is the main control structure that runs the decision loop.
type tuner struct {
	policy   Policy
	interval time.Duration
	cancel   context.CancelFunc
	done     chan struct{}
	logger   *log.Logger
}

// Start initializes and starts the GOGC tuner with a given policy.
// It returns a function that can be called to stop the tuner gracefully.
func Start(ctx context.Context, policy Policy) (stopFunc func(), err error) {
	t := &tuner{
		policy:   policy,
		interval: defaultDecisionInterval,
		done:     make(chan struct{}),
		logger:   log.New(log.Writer(), "[gogctune] ", log.LstdFlags),
	}
	policy.SetLogger(t.logger)

	var tCtx context.Context
	tCtx, t.cancel = context.WithCancel(ctx)

	go t.run(tCtx)

	// Return a function to gracefully stop the tuner.
	return func() {
		t.cancel()
		<-t.done
	}, nil
}

// run is the main loop that periodically triggers the policy's decision logic.
func (t *tuner) run(ctx context.Context) {
	defer close(t.done)
	t.logger.Printf("Starting tuner with '%s' policy (interval: %s)...", t.policy.Name(), t.interval)

	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			currentGOGC := debug.SetGCPercent(-1) // Get current GOGC
			newGOGC := t.policy.Decide(currentGOGC, t.interval)
			clampedGOGC := clamp(newGOGC, defaultMinGOGC, defaultMaxGOGC)

			if clampedGOGC != currentGOGC {
				debug.SetGCPercent(clampedGOGC)
				t.logger.Printf("Adjusted GOGC from %d to %d", currentGOGC, clampedGOGC)
			}
		case <-ctx.Done():
			t.logger.Println("Stopping tuner...")
			return
		}
	}
}

// --- Balanced Policy ---

type balancedPolicy struct {
	// Configuration
	targetCPUFractionLow     float64
	targetCPUFractionHigh    float64
	highHeapGrowthThreshold  float64
	gogcIncreaseFactor       float64
	gogcModerateDecreaseStep int
	gogcSlowDecreaseStep     int
	cooldownIntervals        int

	// State
	lastGCStats       debug.GCStats
	lastMemStats      runtime.MemStats
	emaGCCPUFraction  float64
	emaHeapGrowthRate float64
	lastAction        int // -1 for decrease, 1 for increase
	cooldownCounter   int
	isFirstRun        bool
	logger            *log.Logger
}

const (
	actionDecrease = -1
	actionIncrease = 1
)

// NewBalancedPolicy creates a new policy that balances CPU and memory.
func NewBalancedPolicy(logger *log.Logger) Policy {
	return &balancedPolicy{
		targetCPUFractionLow:     0.02,
		targetCPUFractionHigh:    0.04,
		highHeapGrowthThreshold:  0.20,
		gogcIncreaseFactor:       200.0,
		gogcModerateDecreaseStep: 10,
		gogcSlowDecreaseStep:     2,
		cooldownIntervals:        3,
		isFirstRun:               true,
		logger:                   logger,
	}
}

func (p *balancedPolicy) Name() string { return "Balanced" }

func (p *balancedPolicy) SetLogger(l *log.Logger) { p.logger = l }

func (p *balancedPolicy) Decide(currentGOGC int, interval time.Duration) int {
	var currentGCStats debug.GCStats
	var currentMemStats runtime.MemStats
	debug.ReadGCStats(&currentGCStats)
	runtime.ReadMemStats(&currentMemStats)

	if p.isFirstRun {
		p.isFirstRun = false
		p.lastGCStats = currentGCStats
		p.lastMemStats = currentMemStats
		return currentGOGC
	}

	// Calculate metrics for the specific interval
	deltaPauseNS := currentGCStats.PauseTotal - p.lastGCStats.PauseTotal
	intervalCPUFraction := float64(deltaPauseNS) / float64(interval.Nanoseconds())

	var heapGrowthRate float64
	if p.lastMemStats.HeapAlloc > 0 {
		heapGrowthRate = float64(currentMemStats.HeapAlloc-p.lastMemStats.HeapAlloc) / float64(p.lastMemStats.HeapAlloc)
	}

	// Update smoothed metrics
	p.emaGCCPUFraction = calculateEMA(intervalCPUFraction, p.emaGCCPUFraction, defaultEMASmoothingFactor)
	p.emaHeapGrowthRate = calculateEMA(heapGrowthRate, p.emaHeapGrowthRate, defaultEMASmoothingFactor)

	p.logger.Printf("CPU Fraction (EMA): %.4f, Heap Growth (EMA): %.4f", p.emaGCCPUFraction, p.emaHeapGrowthRate)

	newGOGC := currentGOGC

	// Update cooldown counter
	if p.cooldownCounter > 0 {
		p.cooldownCounter--
	}

	// Decision Tree
	if p.emaGCCPUFraction > p.targetCPUFractionHigh {
		// CPU-First Response: GC is too expensive
		cpuOvershoot := p.emaGCCPUFraction - p.targetCPUFractionHigh
		increaseAmount := int(cpuOvershoot * p.gogcIncreaseFactor)
		newGOGC = currentGOGC + increaseAmount
		p.lastAction = actionIncrease
		p.cooldownCounter = p.cooldownIntervals
		p.logger.Printf("Action: CPU-First Response. Increasing GOGC by %d.", increaseAmount)
	} else if p.emaGCCPUFraction <= p.targetCPUFractionHigh && p.emaHeapGrowthRate > p.highHeapGrowthThreshold {
		// Memory Response: CPU is fine, but memory is growing fast
		if p.lastAction != actionIncrease || p.cooldownCounter == 0 {
			newGOGC = currentGOGC - p.gogcModerateDecreaseStep
			p.lastAction = actionDecrease
			p.logger.Printf("Action: Memory Response. Decreasing GOGC by %d.", p.gogcModerateDecreaseStep)
		}
	} else if p.emaGCCPUFraction < p.targetCPUFractionLow {
		// Optimization Mode: System is idle, reclaim memory
		if p.lastAction != actionIncrease || p.cooldownCounter == 0 {
			newGOGC = currentGOGC - p.gogcSlowDecreaseStep
			p.lastAction = actionDecrease
			p.logger.Printf("Action: Optimization Mode. Decreasing GOGC by %d.", p.gogcSlowDecreaseStep)
		}
	}

	// Update state for next cycle
	p.lastGCStats = currentGCStats
	p.lastMemStats = currentMemStats

	return newGOGC
}

// --- FavorMemory Policy ---

type favorMemoryPolicy struct {
	minGOGC                  int
	aggressiveDecreaseStep   int
	emergencyCPUThreshold    float64
	emergencyGOGCValue       int
	emergencySustainDuration int

	lastGCStats           debug.GCStats
	emaGCCPUFraction      float64
	highCPUSustainCounter int
	isFirstRun            bool
	logger                *log.Logger
}

// NewFavorMemoryPolicy creates a policy that aggressively saves memory.
func NewFavorMemoryPolicy(logger *log.Logger) Policy {
	return &favorMemoryPolicy{
		minGOGC:                  20,
		aggressiveDecreaseStep:   15,
		emergencyCPUThreshold:    0.15,
		emergencyGOGCValue:       150,
		emergencySustainDuration: 3,
		isFirstRun:               true,
		logger:                   logger,
	}
}

func (p *favorMemoryPolicy) Name() string { return "FavorMemory" }

func (p *favorMemoryPolicy) SetLogger(l *log.Logger) { p.logger = l }

func (p *favorMemoryPolicy) Decide(currentGOGC int, interval time.Duration) int {
	var currentGCStats debug.GCStats
	debug.ReadGCStats(&currentGCStats)

	if p.isFirstRun {
		p.isFirstRun = false
		p.lastGCStats = currentGCStats
		return currentGOGC
	}

	deltaPauseNS := currentGCStats.PauseTotal - p.lastGCStats.PauseTotal
	intervalCPUFraction := float64(deltaPauseNS) / float64(interval.Nanoseconds())
	p.emaGCCPUFraction = calculateEMA(intervalCPUFraction, p.emaGCCPUFraction, defaultEMASmoothingFactor)

	// CPU Safety Valve
	if p.emaGCCPUFraction > p.emergencyCPUThreshold {
		p.highCPUSustainCounter++
	} else {
		p.highCPUSustainCounter = 0 // Reset if below threshold
	}

	if p.highCPUSustainCounter >= p.emergencySustainDuration {
		p.logger.Printf("Action: CPU Safety Valve Triggered. Setting GOGC to emergency value %d.", p.emergencyGOGCValue)
		p.highCPUSustainCounter = 0 // Reset after triggering
		p.lastGCStats = currentGCStats
		return p.emergencyGOGCValue
	}

	// Default Aggressive Reduction
	newGOGC := currentGOGC - p.aggressiveDecreaseStep
	p.logger.Printf("Action: Aggressive Reduction. Decreasing GOGC by %d.", p.aggressiveDecreaseStep)

	p.lastGCStats = currentGCStats
	return newGOGC
}

// --- FavorCPU Policy ---

type favorCPUPolicy struct {
	maxGOGC                int
	aggressiveIncreaseStep int
	oomPreventionGOGC      int
	memoryCapBytes         uint64

	logger *log.Logger
}

// NewFavorCPUPolicy creates a policy that aggressively minimizes GC pauses.
func NewFavorCPUPolicy(logger *log.Logger) Policy {
	// Determine system memory cap once at startup.
	v, err := mem.VirtualMemory()
	var capBytes uint64
	if err != nil {
		logger.Printf("Could not get system memory, using fallback 1GB cap: %v", err)
		capBytes = 1 * 1024 * 1024 * 1024 // 1GB fallback
	} else {
		capBytes = uint64(float64(v.Total) * 0.80) // 80% of total system memory
	}

	logger.Printf("FavorCPU policy initialized with a memory cap of %d MB", capBytes/1024/1024)

	return &favorCPUPolicy{
		maxGOGC:                250,
		aggressiveIncreaseStep: 20,
		oomPreventionGOGC:      25,
		memoryCapBytes:         capBytes,
		logger:                 logger,
	}
}

func (p *favorCPUPolicy) Name() string { return "FavorCPU" }

func (p *favorCPUPolicy) SetLogger(l *log.Logger) { p.logger = l }

func (p *favorCPUPolicy) Decide(currentGOGC int, interval time.Duration) int {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	// Memory Safety Valve
	if m.HeapAlloc > p.memoryCapBytes {
		p.logger.Printf("Action: Memory Safety Valve Triggered (HeapAlloc: %d MB). Forcing GC.", m.HeapAlloc/1024/1024)
		return p.oomPreventionGOGC
	}

	// Default Aggressive Increase
	newGOGC := currentGOGC + p.aggressiveIncreaseStep
	p.logger.Printf("Action: Aggressive Increase. Increasing GOGC by %d.", p.aggressiveIncreaseStep)
	return newGOGC
}

// --- Utility Functions ---

// clamp ensures a value stays within a min/max range.
func clamp(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

// calculateEMA computes the new Exponential Moving Average.
func calculateEMA(currentValue, previousEMA, alpha float64) float64 {
	return (alpha * currentValue) + ((1 - alpha) * previousEMA)
}
