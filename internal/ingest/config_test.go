package ingest

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfig_PropagationDefaults(t *testing.T) {
	// No propagation block → all defaults.
	cfg, err := LoadConfig(writeConfig(t, "agency_id: mta\n"))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Propagation
	if p.BaseDwellSecs != 30 || p.DwellElasticity != 0.3 || p.LoadIncreasePct != 0.15 ||
		p.ConfidenceDecay != 0.7 || p.MinConfidence != 0.2 || p.MinDelayThreshold != 15 ||
		p.MaxDepth != 5 || p.MaxImpacts != 100 || p.PerStopMinCount != 5 {
		t.Errorf("defaults not applied: %+v", p)
	}
}

func TestLoadConfig_PropagationPartialOverride(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, `agency_id: mta
propagation:
  max_impacts: 10
  confidence_decay: 0.8
`))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Propagation
	if p.MaxImpacts != 10 {
		t.Errorf("MaxImpacts = %d, want override 10", p.MaxImpacts)
	}
	if p.ConfidenceDecay != 0.8 {
		t.Errorf("ConfidenceDecay = %v, want override 0.8", p.ConfidenceDecay)
	}
	if p.BaseDwellSecs != 30 || p.MaxDepth != 5 {
		t.Errorf("unset fields should default: %+v", p)
	}
}
