package heartratepb

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestHeartRateMeasurementRoundTrip(t *testing.T) {
	ts := time.Date(2026, 6, 4, 19, 0, 0, 0, time.UTC)
	in := &HeartRateMeasurement{
		HeartRate:   72,
		RrIntervals: []uint32{812, 798, 805},
		Timestamp:   timestamppb.New(ts),
	}

	wire, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(wire) == 0 {
		t.Fatal("marshalled to zero bytes")
	}

	var out HeartRateMeasurement
	if err := proto.Unmarshal(wire, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if out.GetHeartRate() != 72 {
		t.Errorf("heart_rate = %d, want 72", out.GetHeartRate())
	}
	if got := out.GetRrIntervals(); len(got) != 3 || got[0] != 812 || got[2] != 805 {
		t.Errorf("rr_intervals = %v, want [812 798 805]", got)
	}
	if !out.GetTimestamp().AsTime().Equal(ts) {
		t.Errorf("timestamp = %v, want %v", out.GetTimestamp().AsTime(), ts)
	}
}
