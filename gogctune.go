// Package gogctune provides a dynamic, adaptive mechanism for automatically tuning
// the GOGC environment variable in real-time. It monitors application and garbage
// collector (GC) metrics to balance memory usage and CPU overhead according to
// user-defined policies. This helps in optimizing application performance without
// manual GOGC tuning.
package gogctune

import (
	"context"
	"log"
	"os"
	"runtime"
	"runtime/debug"
	"sync"
	"time"
)

// Logger is a simple interface for logging. It's compatible with the standard library's log.Logger.
type Logger interface {
	Printf(format string, v ...interface{})
}

// DefaultLogger is used if no logger is provided in the configuration.
var defaultLogger = log.New(os.Stderr, "[gogctune] ", log.LstdFlags)

// Tuner manages the GOGC tuning lifecycle.
type Tuner struct {
	// config holds the tuner's configuration.
	config *Config
	// stopChan is used to signal the tuning goroutine to stop.
	stopChan chan struct{}
	// wg is used to wait for the tuning goroutine to finish.
	wg sync.WaitGroup
	// mu protects against concurrent calls to Tune and Stop.
	mu sync.Mutex
	// isRunning tracks if the tuner is currently active.
	isRunning bool
}

// Config holds the configuration for the Tuner.
type Config struct {
	// Policy is the tuning strategy to use. Required.
	Policy Policy
	// Interval is the time between tuning adjustments.
	// A smaller interval is more responsive but may have more overhead.
	// Default: 10 seconds.
	Interval time.Duration
	// Logger is the logger to use for output.
	// If nil, a default logger writing to stderr is used.
	Logger Logger
}

// New creates a new Tuner with the given configuration.
func New(config *Config) *Tuner {
	// Set default values for configuration if not provided.
	if config.Interval <= 0 {
		config.Interval = 10 * time.Second
	}
	if config.Logger == nil {
		config.Logger = defaultLogger
	}
	return &Tuner{
		config:   config,
		stopChan: make(chan struct{}),
	}
}

// Tune starts the GOGC tuning process in a new goroutine.
// It returns a function that can be called to stop the tuner.
func (t *Tuner) Tune(ctx context.Context) (stop func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// If already running, do nothing.
	if t.isRunning {
		t.config.Logger.Printf("Tuner is already running.")
		return func() {}
	}

	t.config.Logger.Printf("Starting GOGC tuner with a %s policy and %.1fs interval",
		t.config.Policy.Name(), t.config.Interval.Seconds())

	t.isRunning = true
	t.wg.Add(1)
	t.stopChan = make(chan struct{})

	go t.run(ctx)

	// Return a stop function that is safe to call multiple times.
	return sync.OnceFunc(t.Stop)
}

// Stop gracefully stops the tuner and waits for it to exit.
func (t *Tuner) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.isRunning {
		return
	}

	t.config.Logger.Printf("Stopping GOGC tuner...")
	close(t.stopChan)
	t.wg.Wait()
	t.isRunning = false
	t.config.Logger.Printf("GOGC tuner stopped.")
}

// run is the main loop for the tuner. It periodically calls the policy to adjust GOGC.
func (t *Tuner) run(ctx context.Context) {
	defer t.wg.Done()

	// Initialize the policy with current stats.
	t.config.Policy.Initialize()

	ticker := time.NewTicker(t.config.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Calculate the recommended GOGC value.
			newGOGC := t.config.Policy.Recommend()
			// Get the current GOGC value.
			currentGOGC := debug.SetGCPercent(-1)
			debug.SetGCPercent(currentGOGC)

			// Apply the new value only if it's different from the current one.
			if newGOGC != currentGOGC {
				debug.SetGCPercent(newGOGC)
				t.config.Logger.Printf("GOGC adjusted from %d to %d", currentGOGC, newGOGC)
			}
		case <-t.stopChan:
			// Exit loop if Stop() was called.
			return
		case <-ctx.Done():
			// Exit loop if the parent context is cancelled.
			t.Stop()
			return
		}
	}
}

// --- Tuning Policies ---

// Policy defines the interface for a GOGC tuning strategy.
type Policy interface {
	// Initialize is called once before the tuning loop starts to set up initial state.
	Initialize()
	// Recommend calculates and returns the recommended GOGC value based on current metrics.
	Recommend() int
	// Name returns the name of the policy.
	Name() string
}

// statefulPolicy provides a base for policies that need to track metrics over time.
type statefulPolicy struct {
	// logger for policy-specific logging.
	logger Logger
	// lastGCStats stores the GC stats from the previous cycle.
	lastGCStats debug.GCStats
	// lastMemStats stores the memory stats from the previous cycle.
	lastMemStats runtime.MemStats
	// gcCPUFractionEMA is the Exponential Moving Average of the GC CPU fraction.
	// It helps smooth out short-term spikes.
	gcCPUFractionEMA float64
	// alpha is the smoothing factor for the EMA. A smaller alpha results in smoother values.
	alpha float64
}

// Initialize captures the initial GC and memory statistics.
func (p *statefulPolicy) Initialize() {
	debug.ReadGCStats(&p.lastGCStats)
	runtime.ReadMemStats(&p.lastMemStats)
	// Seed EMA with an initial reasonable value
	p.gcCPUFractionEMA = 0.03 // Assume 3% to start
}

// updateMetrics reads the latest stats, calculates deltas, and updates the EMA.
func (p *statefulPolicy) updateMetrics() (heapGrowthRatio float64) {
	var currentGCStats debug.GCStats
	debug.ReadGCStats(&currentGCStats)

	var currentMemStats runtime.MemStats
	runtime.ReadMemStats(&currentMemStats)

	// Calculate GC CPU fraction for the latest cycle if a GC has occurred.
	if len(currentGCStats.Pause) > 0 && len(p.lastGCStats.Pause) > 0 && currentGCStats.NumGC > p.lastGCStats.NumGC {
		lastGCEndTime := p.lastGCStats.LastGC
		currentGCEndTime := currentGCStats.LastGC

		if currentGCEndTime.After(lastGCEndTime) {
			// Total time elapsed since the last GC finished.
			totalTime := currentGCEndTime.Sub(lastGCEndTime).Seconds()
			// CPU time spent in GC during the last cycle.
			gcCPUTime := currentGCStats.PauseTotal.Seconds() - p.lastGCStats.PauseTotal.Seconds()

			if totalTime > 0 {
				gcCPUFraction := gcCPUTime / totalTime
				// Update EMA
				p.gcCPUFractionEMA = p.alpha*gcCPUFraction + (1-p.alpha)*p.gcCPUFractionEMA
			}
		}
	}

	// Calculate heap growth ratio.
	if p.lastMemStats.HeapAlloc > 0 {
		heapGrowthRatio = float64(currentMemStats.HeapAlloc-p.lastMemStats.HeapAlloc) / float64(p.lastMemStats.HeapAlloc)
	}

	// Update last known stats for the next cycle.
	p.lastGCStats = currentGCStats
	p.lastMemStats = currentMemStats
	return heapGrowthRatio
}

// clamp ensures the GOGC value stays within a safe, reasonable range.
func clamp(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

// --- FavorCPU Policy ---

// FavorCPUPolicy prioritizes reducing GC CPU usage.
// It will aggressively increase GOGC if GC CPU usage is high,
// even at the cost of higher memory consumption.
type FavorCPUPolicy struct {
	statefulPolicy
}

// NewFavorCPUPolicy creates a policy that prioritizes CPU.
func NewFavorCPUPolicy(logger Logger) Policy {
	return &FavorCPUPolicy{
		statefulPolicy: statefulPolicy{logger: logger, alpha: 0.2},
	}
}

func (p *FavorCPUPolicy) Name() string { return "FavorCPU" }

func (p *FavorCPUPolicy) Recommend() int {
	p.updateMetrics()

	currentGOGC := debug.SetGCPercent(-1)
	debug.SetGCPercent(currentGOGC)

	targetCPUFraction := 0.03 // Target a low 3% GC CPU fraction
	adjustment := 0

	// Proportional adjustment: the further we are from the target, the larger the adjustment.
	if p.gcCPUFractionEMA > targetCPUFraction {
		// If CPU usage is high, increase GOGC aggressively.
		adjustment = int((p.gcCPUFractionEMA - targetCPUFraction) * 500) // High proportional constant
	} else {
		// If CPU usage is low, we can slightly decrease GOGC to reclaim memory.
		adjustment = -5
	}

	return clamp(currentGOGC+adjustment, 50, 500) // Allow high GOGC values
}

// --- FavorMemory Policy ---

// FavorMemoryPolicy prioritizes keeping memory usage low.
// It will aggressively decrease GOGC if the heap is growing,
// even if it means higher GC CPU usage.
type FavorMemoryPolicy struct {
	statefulPolicy
}

// NewFavorMemoryPolicy creates a policy that prioritizes Memory.
func NewFavorMemoryPolicy(logger Logger) Policy {
	return &FavorMemoryPolicy{
		statefulPolicy: statefulPolicy{logger: logger, alpha: 0.2},
	}
}

func (p *FavorMemoryPolicy) Name() string { return "FavorMemory" }

func (p *FavorMemoryPolicy) Recommend() int {
	heapGrowthRatio := p.updateMetrics()

	currentGOGC := debug.SetGCPercent(-1)
	debug.SetGCPercent(currentGOGC)

	targetHeapGrowth := 0.10 // Target a modest 10% heap growth between cycles
	adjustment := 0

	if heapGrowthRatio > targetHeapGrowth {
		// If heap is growing too fast, decrease GOGC aggressively.
		adjustment = -int((heapGrowthRatio - targetHeapGrowth) * 100) // High proportional constant
	} else if p.gcCPUFractionEMA < 0.05 { // Only increase if CPU is not under pressure
		// If heap growth is under control, we can slightly increase GOGC.
		adjustment = 5
	}

	return clamp(currentGOGC+adjustment, 20, 150) // Keep GOGC values low
}

// --- Balanced Policy ---

// BalancedPolicy tries to find a sweet spot between CPU and memory usage.
// It prioritizes keeping GC CPU usage stable, and only then tries to
// optimize memory usage.
type BalancedPolicy struct {
	statefulPolicy
}

// NewBalancedPolicy creates a balanced policy.
func NewBalancedPolicy(logger Logger) Policy {
	return &BalancedPolicy{
		statefulPolicy: statefulPolicy{logger: logger, alpha: 0.2},
	}
}

func (p *BalancedPolicy) Name() string { return "Balanced" }

func (p *BalancedPolicy) Recommend() int {
	heapGrowthRatio := p.updateMetrics()

	currentGOGC := debug.SetGCPercent(-1)
	debug.SetGCPercent(currentGOGC)

	// Define the target "sweet spot" for GC CPU Fraction
	targetCPUFractionMin := 0.03 // 3%
	targetCPUFractionMax := 0.05 // 5%

	adjustment := 0

	// Priority 1: Respond to CPU pressure first.
	if p.gcCPUFractionEMA > targetCPUFractionMax {
		// CPU usage is too high, increase GOGC to reduce it.
		adjustment = int((p.gcCPUFractionEMA - targetCPUFractionMax) * 200) // Moderate proportional constant
	} else if p.gcCPUFractionEMA < targetCPUFractionMin {
		// CPU usage is low, we have room to reclaim memory.
		// Priority 2: Respond to memory pressure.
		if heapGrowthRatio > 0.15 { // 15% growth
			// Heap is growing, but CPU is fine. Decrease GOGC moderately.
			adjustment = -10
		} else {
			// Both CPU and Memory are fine, gently decrease GOGC to be more memory-efficient.
			adjustment = -5
		}
	}
	// If inside the sweet spot, do nothing and let the system stabilize.

	return clamp(currentGOGC+adjustment, 50, 200) // Standard GOGC range
}

// Start is a convenience function to create and start a tuner with a default policy.
// It returns a function that can be called to stop the tuner.
func Start(ctx context.Context, policy Policy) (stop func(), err error) {
	if policy == nil {
		policy = NewBalancedPolicy(defaultLogger)
	}

	tuner := New(&Config{
		Policy: policy,
	})

	return tuner.Tune(ctx), nil
}
