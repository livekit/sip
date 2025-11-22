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
