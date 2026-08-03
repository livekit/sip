package sip

const (
	signalLoggingFeatureFlag        = "sip.signal_logging"
	outboundRouteHeadersFeatureFlag = "sip.outbound_route_headers"
	// uriUserPhoneFeatureFlag appends ;user=phone to outbound Request-URI, From, and To
	// (needed by some carriers such as Airtel; see livekit/sip#615).
	uriUserPhoneFeatureFlag = "sip.uri_user_phone"
)
