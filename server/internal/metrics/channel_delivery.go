package metrics

import "github.com/prometheus/client_golang/prometheus"

// ChannelDeliveryMetrics measures the PRD's ">=95% delivered/generated"
// success criterion for a channel adapter's OUTBOUND reply path (Discord is
// the first caller, added for the streaming outbound worker — see
// internal/integrations/discord/outbound.go's package doc). It is
// channel-agnostic on purpose, mirroring ChannelLeaseMetrics: any adapter
// with a Router-driven or agent-driven reply can call it without a new
// metric per platform.
//
// Generated counts every reply Multica decided to send (a non-empty
// EventChatDone payload, or a verdict the Router asked a replier to post).
// Delivered counts what actually reached the platform, split by outcome so a
// silent-drop regression like multica-ai/multica#7215 (WeCom's outbound
// requires the Gateway lease holder; on another replica the reply is
// swallowed with nothing but a debug log) is a visible ratio here instead of
// a support ticket. Comparing sum(Delivered{outcome="delivered"}) against
// sum(Generated) is exactly the PRD ratio; the other outcomes explain the
// gap without anyone reading logs.
//
// Both vectors are labeled ONLY by channel_type and outcome — see
// forbiddenMetricLabels in labels.go. installation_id is the natural label
// to reach for here (every outbound call site already carries one) and is
// exactly the unbounded-cardinality mistake that map exists to prevent: one
// series per tenant instead of one per (channel, outcome). Attribution for a
// single failing installation belongs to structured logs, not this metric.
type ChannelDeliveryMetrics struct {
	Generated *prometheus.CounterVec
	Delivered *prometheus.CounterVec
}

// Delivery outcomes recorded on ChannelDeliveryMetrics.Delivered. Additive:
// a future adapter may record a new value without touching this list, since
// Prometheus labels are open-ended strings, not a closed enum here.
const (
	ChannelDeliveryOutcomeDelivered = "delivered"
	ChannelDeliveryOutcomeFailed    = "failed"
	ChannelDeliveryOutcomeDropped   = "dropped"
)

func NewChannelDeliveryMetrics() *ChannelDeliveryMetrics {
	return &ChannelDeliveryMetrics{
		Generated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "multica",
			Subsystem: "channel_delivery",
			Name:      "reply_generated_total",
			Help:      "Outbound channel replies Multica decided to send (agent chat replies and Router verdict notices), before any network attempt.",
		}, []string{"channel_type"}),
		Delivered: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "multica",
			Subsystem: "channel_delivery",
			Name:      "reply_delivered_total",
			Help:      "Outbound channel reply delivery attempts by outcome (delivered, failed, dropped). Compare sum(delivered) against reply_generated_total for the >=95% delivered/generated success ratio.",
		}, []string{"channel_type", "outcome"}),
	}
}

func (m *ChannelDeliveryMetrics) RecordReplyGenerated(channelType string) {
	if m == nil {
		return
	}
	m.Generated.WithLabelValues(channelType).Inc()
}

func (m *ChannelDeliveryMetrics) RecordReplyDelivered(channelType, outcome string) {
	if m == nil {
		return
	}
	m.Delivered.WithLabelValues(channelType, outcome).Inc()
}

func (m *ChannelDeliveryMetrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.Generated, m.Delivered}
}
