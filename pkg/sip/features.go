package sip

import "strconv"

const (
	signalLoggingFeatureFlag        = "sip.signal_logging"
	outboundRouteHeadersFeatureFlag = "sip.outbound_route_headers"
	lateOfferFeatureFlag            = "sip.late_offer"
	// uriUserPhoneFeatureFlag appends ;user=phone to outbound Request-URI, From, and To
	// (needed by some carriers such as Airtel; see livekit/sip#615).
	uriUserPhoneFeatureFlag = "sip.uri_user_phone"
)

// featureFlagEnabled reports whether a boolean feature flag is set to true.
// Missing or unparsable values count as disabled.
func featureFlagEnabled(flags map[string]string, flag string) bool {
	enabled, _ := strconv.ParseBool(flags[flag])
	return enabled
}
