package gogctune

import (
	"context"
	"log"
	"runtime"
	"runtime/debug"
	"sort"
	"time"

	"github.com/shirou/gopsutil/v3/mem"
)

// --- Constants and Default Configuration ---
const (
	defaultMinGOGC = 20
	defaultMaxGOGC = 250
	// The interval at which we check if a new GC cycle has completed.
	gcPollInterval = 100 * time.Millisecond
)

// --- Core Tuner Components ---

// Policy defines the interface for a tuning strategy.
type Policy interface {
	Name() string
	// Decide is called after a GC cycle completes, providing the stats for that cycle.
	Decide(stats *CycleStats) (newGOGC int)
	SetLogger(logger *log.Logger)
}

// CycleStats contains the relevant metrics from a completed garbage collection cycle.
type CycleStats struct {
	CurrentGOGC   int
	Pause         time.Duration
	HeapAlloc     uint64        // Heap allocation after GC
	HeapGoal      uint64        // Heap goal for next GC
	CycleInterval time.Duration // Time since the start of the last GC
}

// tuner is the main control structure that runs the GC monitoring loop.
type tuner struct {
	policy Policy
	cancel context.CancelFunc
	done   chan struct{}
	logger *log.Logger
}

// Start initializes and starts the GOGC tuner with a given policy.
func Start(ctx context.Context, policy Policy) (stopFunc func(), err error) {
	t := &tuner{
		policy: policy,
		done:   make(chan struct{}),
		logger: log.New(log.Writer(), "[gogctune] ", log.LstdFlags),
	}
	policy.SetLogger(t.logger)

	var tCtx context.Context
	tCtx, t.cancel = context.WithCancel(ctx)

	go t.run(tCtx)

	return func() {
		t.cancel()
		<-t.done
	}, nil
}

// run is the main loop that monitors for completed GC cycles and triggers policy decisions.
func (t *tuner) run(ctx context.Context) {
	defer close(t.done)
	t.logger.Printf("Starting tuner with '%s' policy...", t.policy.Name())

	var lastGCStats debug.GCStats
	debug.ReadGCStats(&lastGCStats)

	ticker := time.NewTicker(gcPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			var currentGCStats debug.GCStats
			debug.ReadGCStats(&currentGCStats)

			// A new GC cycle has completed if NumGC has incremented.
			if currentGCStats.NumGC > lastGCStats.NumGC {
				var memStats runtime.MemStats
				runtime.ReadMemStats(&memStats)

				// Calculate stats for the cycle that just finished.
				cycleStats := &CycleStats{
					CurrentGOGC:   debug.SetGCPercent(-1),
					Pause:         currentGCStats.Pause[0], // The most recent pause
					HeapAlloc:     memStats.HeapAlloc,
					HeapGoal:      memStats.NextGC,
					CycleInterval: currentGCStats.LastGC.Sub(lastGCStats.LastGC),
				}

				newGOGC := t.policy.Decide(cycleStats)
				clampedGOGC := clamp(newGOGC, defaultMinGOGC, defaultMaxGOGC)

				if clampedGOGC != cycleStats.CurrentGOGC {
					debug.SetGCPercent(clampedGOGC)
					t.logger.Printf("GC #%d Completed. Pause: %s. Adjusting GOGC from %d to %d", currentGCStats.NumGC, cycleStats.Pause, cycleStats.CurrentGOGC, clampedGOGC)
				} else {
					t.logger.Printf("GC #%d Completed. Pause: %s. GOGC remains at %d", currentGCStats.NumGC, cycleStats.Pause, clampedGOGC)
				}

				lastGCStats = currentGCStats
			}
		case <-ctx.Done():
			t.logger.Println("Stopping tuner...")
			return
		}
	}
}

// --- PID Controller ---
// A simple PID controller implementation.
type pidController struct {
	Kp, Ki, Kd float64 // Proportional, Integral, Derivative gains
	integral   float64
	lastError  float64
}

func (c *pidController) update(err float64) float64 {
	c.integral += err
	derivative := err - c.lastError
	c.lastError = err
	return (c.Kp * err) + (c.Ki * c.integral) + (c.Kd * derivative)
}

// --- Balanced Policy (PID Controller on GC Pause Time) ---
type balancedPolicy struct {
	targetPause time.Duration
	pid         pidController
	pauses      []time.Duration // History of recent pause times
	historySize int
	logger      *log.Logger
}

func NewBalancedPolicy(logger *log.Logger) Policy {
	return &balancedPolicy{
		targetPause: 10 * time.Millisecond,
		pid: pidController{
			Kp: 0.5, // Responds moderately to current error
			Ki: 0.1, // Corrects slowly for sustained error
			Kd: 0.2, // Dampens oscillations
		},
		historySize: 20, // Keep the last 20 pause times for p95 calculation
		logger:      logger,
	}
}

func (p *balancedPolicy) Name() string            { return "Balanced" }
func (p *balancedPolicy) SetLogger(l *log.Logger) { p.logger = l }
func (p *balancedPolicy) Decide(stats *CycleStats) int {
	// Maintain a rolling history of pause times.
	if len(p.pauses) >= p.historySize {
		p.pauses = p.pauses[1:]
	}
	p.pauses = append(p.pauses, stats.Pause)

	// Calculate 95th percentile pause time.
	p95Pause := calculateP95(p.pauses)
	p.logger.Printf("p95 GC Pause: %s", p95Pause)

	// The error is how far we are from our target pause time, in milliseconds.
	err := float64(p.targetPause-p95Pause) / float64(time.Millisecond)

	// The PID controller calculates the necessary adjustment.
	adjustment := p.pid.update(err)

	// If pause is too high (err is negative), we need to INCREASE GOGC.
	// So we subtract the adjustment.
	newGOGC := float64(stats.CurrentGOGC) - adjustment
	return int(newGOGC)
}

// --- FavorMemory Policy (PID Controller on Heap Usage) ---
type favorMemoryPolicy struct {
	targetHeapUtilization float64 // Target heap usage right after a GC
	pid                   pidController
	emergencyPause        time.Duration
	logger                *log.Logger
}

func NewFavorMemoryPolicy(logger *log.Logger) Policy {
	return &favorMemoryPolicy{
		targetHeapUtilization: 0.6, // Aim for heap to be 60% of goal after GC
		pid: pidController{
			Kp: 50.0, // Respond aggressively to heap size error
			Ki: 5.0,
			Kd: 10.0,
		},
		emergencyPause: 100 * time.Millisecond,
		logger:         logger,
	}
}
func (p *favorMemoryPolicy) Name() string            { return "FavorMemory" }
func (p *favorMemoryPolicy) SetLogger(l *log.Logger) { p.logger = l }
func (p *favorMemoryPolicy) Decide(stats *CycleStats) int {
	// CPU Safety Valve: If GC pauses are getting too long, immediately increase GOGC.
	if stats.Pause > p.emergencyPause {
		p.logger.Printf("!! EMERGENCY: Pause time %s exceeded threshold. Temporarily increasing GOGC.", stats.Pause)
		return stats.CurrentGOGC + 50
	}

	currentUtilization := float64(stats.HeapAlloc) / float64(stats.HeapGoal)
	p.logger.Printf("Post-GC Heap Utilization: %.2f%%", currentUtilization*100)

	// The error is how far our utilization is from the target.
	err := p.targetHeapUtilization - currentUtilization

	// PID controller calculates the adjustment.
	adjustment := p.pid.update(err)

	// If utilization is too high (err is negative), we need to DECREASE GOGC.
	// So we add the adjustment (which will be negative).
	newGOGC := float64(stats.CurrentGOGC) + adjustment
	return int(newGOGC)
}

// --- FavorCPU Policy (PID Controller on GC Frequency) ---
type favorCPUPolicy struct {
	targetInterval    time.Duration
	pid               pidController
	memoryCapBytes    uint64
	oomPreventionGOGC int
	logger            *log.Logger
}

func NewFavorCPUPolicy(logger *log.Logger) Policy {
	v, err := mem.VirtualMemory()
	var capBytes uint64
	if err != nil {
		capBytes = 1 * 1024 * 1024 * 1024 // 1GB fallback
	} else {
		capBytes = uint64(float64(v.Total) * 0.80) // 80% of total system memory
	}
	logger.Printf("FavorCPU policy initialized with a memory cap of %d MB", capBytes/1024/1024)

	return &favorCPUPolicy{
		targetInterval: 1 * time.Minute, // Aim for GCs to be 1 minute apart
		pid: pidController{
			Kp: 0.1,
			Ki: 0.01,
			Kd: 0.05,
		},
		memoryCapBytes:    capBytes,
		oomPreventionGOGC: 25,
		logger:            logger,
	}
}

func (p *favorCPUPolicy) Name() string            { return "FavorCPU" }
func (p *favorCPUPolicy) SetLogger(l *log.Logger) { p.logger = l }
func (p *favorCPUPolicy) Decide(stats *CycleStats) int {
	// Memory Safety Valve: If we are approaching OOM, force a collection.
	if stats.HeapAlloc > p.memoryCapBytes {
		p.logger.Printf("!! EMERGENCY: Heap allocation %d MB has exceeded memory cap. Forcing GC.", stats.HeapAlloc/1024/1024)
		return p.oomPreventionGOGC
	}

	p.logger.Printf("Time since last GC: %s", stats.CycleInterval)

	// Error is the difference between our target and actual interval, in seconds.
	err := float64(p.targetInterval-stats.CycleInterval) / float64(time.Second)

	adjustment := p.pid.update(err)

	// If interval is too short (err is positive), we need to INCREASE GOGC.
	// So we add the positive adjustment.
	newGOGC := float64(stats.CurrentGOGC) + adjustment
	return int(newGOGC)
}

// --- Utility Functions ---
func clamp(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func calculateP95(data []time.Duration) time.Duration {
	if len(data) == 0 {
		return 0
	}
	// Create a copy to avoid modifying the original slice.
	sorted := make([]time.Duration, len(data))
	copy(sorted, data)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(float64(len(sorted)) * 0.95)
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}
