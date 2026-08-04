package propagation

// Config holds the tunable propagation-model parameters. All fields treat
// zero as "use the default" — none of them are meaningfully zero — so a
// partially-specified (or absent) yaml block merges cleanly with defaults.
type Config struct {
	// BaseDwellSecs is the assumed base dwell time per stop.
	BaseDwellSecs int `yaml:"base_dwell_secs"`
	// DwellElasticity scales extra dwell: extra = base * load_pct * elasticity.
	DwellElasticity float64 `yaml:"dwell_elasticity"`
	// LoadIncreasePct is the fraction of riders affected by a broken transfer.
	LoadIncreasePct float64 `yaml:"load_increase_pct"`
	// ConfidenceDecay is the per-hop confidence multiplier.
	ConfidenceDecay float64 `yaml:"confidence_decay"`
	// MinConfidence stops propagation below this confidence.
	MinConfidence float64 `yaml:"min_confidence"`
	// MinDelayThreshold stops propagation below this many seconds.
	MinDelayThreshold int32 `yaml:"min_delay_threshold_secs"`
	// MaxDepth limits recursive cascade depth in hops.
	MaxDepth int `yaml:"max_depth"`
	// MaxImpacts caps the total downstream impacts per propagation.
	MaxImpacts int `yaml:"max_impacts"`
	// PerStopMinCount is the minimum observed outcomes at a stop before the
	// engine blends learned per-stop impacts into its dwell model.
	PerStopMinCount int `yaml:"per_stop_min_count"`
}

// DefaultConfig returns the propagation defaults.
func DefaultConfig() Config {
	return Config{
		BaseDwellSecs:     30,
		DwellElasticity:   0.3,
		LoadIncreasePct:   0.15,
		ConfidenceDecay:   0.7,
		MinConfidence:     0.2,
		MinDelayThreshold: 15,
		MaxDepth:          5,
		MaxImpacts:        100,
		PerStopMinCount:   5,
	}
}

// WithDefaults returns a copy of c with zero-valued fields replaced by
// defaults.
func (c Config) WithDefaults() Config {
	d := DefaultConfig()
	if c.BaseDwellSecs == 0 {
		c.BaseDwellSecs = d.BaseDwellSecs
	}
	if c.DwellElasticity == 0 {
		c.DwellElasticity = d.DwellElasticity
	}
	if c.LoadIncreasePct == 0 {
		c.LoadIncreasePct = d.LoadIncreasePct
	}
	if c.ConfidenceDecay == 0 {
		c.ConfidenceDecay = d.ConfidenceDecay
	}
	if c.MinConfidence == 0 {
		c.MinConfidence = d.MinConfidence
	}
	if c.MinDelayThreshold == 0 {
		c.MinDelayThreshold = d.MinDelayThreshold
	}
	if c.MaxDepth == 0 {
		c.MaxDepth = d.MaxDepth
	}
	if c.MaxImpacts == 0 {
		c.MaxImpacts = d.MaxImpacts
	}
	if c.PerStopMinCount == 0 {
		c.PerStopMinCount = d.PerStopMinCount
	}
	return c
}
