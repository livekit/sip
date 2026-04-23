package sip

type SDPError struct {
	Err error
}

func (e SDPError) Error() string {
	return e.Err.Error()
}

func (e SDPError) Unwrap() error {
	return e.Err
}

// SDPOfferParseError is returned when the remote offer SDP could not be parsed
// (malformed or missing required media). The caller may reject the INVITE with 4xx.
type SDPOfferParseError struct {
	Err error
}

func (e SDPOfferParseError) Error() string {
	return e.Err.Error()
}

func (e SDPOfferParseError) Unwrap() error {
	return e.Err
}
