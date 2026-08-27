package interceptor

import (
	"fmt"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

const (
	MaxPayloadSize = 1200
)

var ErrPayloadSizeTooLarge = fmt.Errorf("packetization payload size should not greater than %d bytes", MaxPayloadSize)

type LimitSizeInterceptorFactory struct {
}

func NewLimitSizeInterceptorFactory() *LimitSizeInterceptorFactory {
	return &LimitSizeInterceptorFactory{}
}

func (l *LimitSizeInterceptorFactory) NewInterceptor(id string) (interceptor.Interceptor, error) {
	return &LimitSizeInterceptor{}, nil
}

type LimitSizeInterceptor struct {
	interceptor.NoOp
}

func (l *LimitSizeInterceptor) BindLocalStream(stream *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
		if len(payload) > MaxPayloadSize {
			return 0, ErrPayloadSizeTooLarge
		}
		return writer.Write(header, payload, attributes)
	})
}
