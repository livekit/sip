package stats

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// RegisterActiveCalls registers a snapshot of the calls tracked by the SIP service.
// Call it during startup, after Start and before serving metrics. The callback
// must support concurrent scrapes. Stop unregisters the collector.
func (m *Monitor) RegisterActiveCalls(snapshot func() (inbound, outbound int)) error {
	if snapshot == nil {
		return fmt.Errorf("active calls snapshot callback is nil")
	}
	c := newActiveCallsCollector(m.nodeID, snapshot)
	if err := prometheus.Register(c); err != nil {
		return fmt.Errorf("register SIP calls handled collector: %w", err)
	}
	m.metrics = append(m.metrics, c)
	return nil
}

type activeCallsCollector struct {
	desc     *prometheus.Desc
	snapshot func() (inbound, outbound int)
}

func newActiveCallsCollector(nodeID string, snapshot func() (inbound, outbound int)) *activeCallsCollector {
	return &activeCallsCollector{
		desc: prometheus.NewDesc(
			"livekit_sip_calls_inprogress",
			"Number of SIP calls currently tracked by this SIP process, including setup and connected calls until cleanup removes them.",
			[]string{"dir"},
			prometheus.Labels{"node_id": nodeID},
		),
		snapshot: snapshot,
	}
}

func (c *activeCallsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

func (c *activeCallsCollector) Collect(ch chan<- prometheus.Metric) {
	// ActiveCalls releases the call-map locks before returning this snapshot.
	inbound, outbound := c.snapshot()
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(inbound), Inbound.String())
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(outbound), Outbound.String())
}
