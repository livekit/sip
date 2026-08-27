package interceptor

import (
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"

	"github.com/livekit/mediatransportutil"
)

type RTTInterceptorFactory struct {
	onRttUpdate func(rtt uint32)
}

func NewRTTInterceptorFactory(onRttUpdate func(rtt uint32)) *RTTInterceptorFactory {
	return &RTTInterceptorFactory{
		onRttUpdate: onRttUpdate,
	}
}

func (r *RTTInterceptorFactory) NewInterceptor(_ string) (interceptor.Interceptor, error) {
	return NewRTTInterceptor(r.onRttUpdate), nil
}

type RTTInterceptor struct {
	interceptor.NoOp

	onRttUpdate func(rtt uint32)
}

func NewRTTInterceptor(onRttUpdate func(rtt uint32)) *RTTInterceptor {
	return &RTTInterceptor{
		onRttUpdate: onRttUpdate,
	}
}

func (r *RTTInterceptor) BindRTCPReader(reader interceptor.RTCPReader) interceptor.RTCPReader {
	return interceptor.RTCPReaderFunc(func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
		i, attr, err := reader.Read(b, a)
		if err != nil {
			return 0, nil, err
		}

		if attr == nil {
			attr = make(interceptor.Attributes)
		}
		pkts, err := attr.GetRTCPPackets(b[:i])
		if err != nil {
			return 0, nil, err
		}

	rttCaculate:
		for _, packet := range pkts {
			if rr, ok := packet.(*rtcp.ReceiverReport); ok {
				for _, report := range rr.Reports {
					rtt, err := mediatransportutil.GetRttMsFromReceiverReportOnly(&report)
					if err == nil && rtt != 0 {
						r.onRttUpdate(rtt)
					}

					break rttCaculate
				}
			}
		}

		return i, attr, err
	})
}
