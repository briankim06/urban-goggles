package propagation

import (
	"context"
	"testing"
)

func TestGetTransferStats_ReturnsRecordedPairs(t *testing.T) {
	rdb := skipIfNoRedis(t)
	defer rdb.Close()
	ctx := context.Background()
	rdb.FlushDB(ctx)

	h := NewHistoricalStore(rdb)
	if err := h.RecordObservation(ctx, "A", "L", "S1", 8, 300, 120); err != nil {
		t.Fatal(err)
	}
	if err := h.RecordObservation(ctx, "A", "L", "S1", 8, 300, 180); err != nil {
		t.Fatal(err)
	}
	if err := h.RecordObservation(ctx, "B", "G", "S1", 8, 200, 60); err != nil {
		t.Fatal(err)
	}
	// Different station/hour must not appear.
	if err := h.RecordObservation(ctx, "A", "L", "S2", 8, 300, 999); err != nil {
		t.Fatal(err)
	}

	stats, err := h.GetTransferStats(ctx, "S1", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 2 {
		t.Fatalf("got %d stats, want 2 (was always 0 before the Sscanf fix)", len(stats))
	}
	byPair := map[string]TransferStat{}
	for _, s := range stats {
		byPair[s.FromRouteID+":"+s.ToRouteID] = s
	}
	if s, ok := byPair["A:L"]; !ok || s.SampleCount != 2 || s.AvgDelayWhenBroken != 150 {
		t.Errorf("A:L = %+v, want count 2 avg 150", s)
	}
	if s, ok := byPair["B:G"]; !ok || s.SampleCount != 1 || s.AvgDelayWhenBroken != 60 {
		t.Errorf("B:G = %+v, want count 1 avg 60", s)
	}
}
