package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestChannelDeliveryMetricsRegisterAndRecord(t *testing.T) {
	m := NewChannelDeliveryMetrics()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(m.Collectors()...)

	m.RecordReplyGenerated("discord")
	m.RecordReplyGenerated("discord")
	m.RecordReplyDelivered("discord", ChannelDeliveryOutcomeDelivered)
	m.RecordReplyDelivered("discord", ChannelDeliveryOutcomeFailed)

	if got := testutil.ToFloat64(m.Generated.WithLabelValues("discord")); got != 2 {
		t.Fatalf("generated = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.Delivered.WithLabelValues("discord", ChannelDeliveryOutcomeDelivered)); got != 1 {
		t.Fatalf("delivered{delivered} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.Delivered.WithLabelValues("discord", ChannelDeliveryOutcomeFailed)); got != 1 {
		t.Fatalf("delivered{failed} = %v, want 1", got)
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"multica_channel_delivery_reply_generated_total": false,
		"multica_channel_delivery_reply_delivered_total": false,
	}
	for _, family := range families {
		if _, ok := want[family.GetName()]; ok {
			want[family.GetName()] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("metric %s was not gathered", name)
		}
	}
}

// TestChannelDeliveryMetricsNilSafe mirrors ChannelLeaseMetrics' nil-receiver
// contract: a caller that skips wiring metrics (e.g. a unit test) must never
// crash on Record*.
func TestChannelDeliveryMetricsNilSafe(t *testing.T) {
	var m *ChannelDeliveryMetrics
	m.RecordReplyGenerated("discord")
	m.RecordReplyDelivered("discord", ChannelDeliveryOutcomeDropped)
}
