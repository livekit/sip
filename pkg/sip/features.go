package sip

import "strconv"

const (
	signalLoggingFeatureFlag        = "sip.signal_logging"
	outboundRouteHeadersFeatureFlag = "sip.outbound_route_headers"
	lateOfferFeatureFlag            = "sip.late_offer"
)

// featureFlagEnabled reports whether a boolean feature flag is set to true.
// Missing or unparsable values count as disabled.
func featureFlagEnabled(flags map[string]string, flag string) bool {
	enabled, _ := strconv.ParseBool(flags[flag])
	return enabled
}
