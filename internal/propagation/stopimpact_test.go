package propagation

import (
	"context"
	"testing"
)

func TestStopImpactStore_RecordAndAverage(t *testing.T) {
	rdb := skipIfNoRedis(t)
	defer rdb.Close()
	ctx := context.Background()
	rdb.FlushDB(ctx)

	s := NewStopImpactStore(rdb)

	for _, v := range []int{100, 200, 300} {
		if err := s.RecordStopOutcome(ctx, "L", "S3", 8, v); err != nil {
			t.Fatal(err)
		}
	}

	avg, count, err := s.GetAverageStopImpact(ctx, "L", "S3", 8)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 || avg != 200 {
		t.Errorf("got (%v, %d), want (200, 3)", avg, count)
	}

	// Key shape.
	if n, _ := rdb.Exists(ctx, "stopimpact:L:S3:8").Result(); n != 1 {
		t.Error("expected key stopimpact:L:S3:8")
	}

	// Different hour/route/station buckets are independent.
	if _, count, _ := s.GetAverageStopImpact(ctx, "L", "S3", 9); count != 0 {
		t.Error("hour bucket leaked")
	}
	if _, count, _ := s.GetAverageStopImpact(ctx, "G", "S3", 8); count != 0 {
		t.Error("route bucket leaked")
	}
}
