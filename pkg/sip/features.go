package sip

const (
	signalLoggingFeatureFlag        = "sip.signal_logging"
	outboundRouteHeadersFeatureFlag = "sip.outbound_route_headers"

	// registration features
	registrationProfileFeatureFlag          = "sip.registration.profile"
	registrationEnabledFeatureFlag          = "sip.registration.enabled"
	registrationRegistrarFeatureFlag        = "sip.registration.registrar_uri"
	registrationAuthURIFeatureFlag          = "sip.registration.auth_uri"
	registrationAORUserFeatureFlag          = "sip.registration.aor_user"
	registrationAuthUserFeatureFlag         = "sip.registration.auth_username"
	registrationContactUserFeatureFlag      = "sip.registration.contact_user"
	registrationFromDomainFeatureFlag       = "sip.registration.from_domain"
	registrationTransportFeatureFlag        = "sip.registration.transport"
	registrationExpiresSecFeatureFlag       = "sip.registration.expires_sec"
	registrationIncludePortFeatureFlag      = "sip.registration.include_port_in_auth_uri"
	registrationIncludeTransportFeatureFlag = "sip.registration.include_transport_in_auth_uri"
	registrationInviteFallbackFeatureFlag   = "sip.registration.invite_on_failure"
	registrationAlwaysRefreshFeatureFlag    = "sip.registration.always_refresh_before_invite"
)
