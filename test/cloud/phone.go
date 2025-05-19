package cloud

import (
	"fmt"
	"os"
	"sync/atomic"

	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/g711"

	"github.com/livekit/sip/pkg/siptest"
)

const (
	to    = 15550100000
	codec = g711.ULawSDPName
)

var num atomic.Int64

type PhoneClient struct {
	*siptest.Client

	rec *os.File
}

func NewPhoneClient(record bool) (*PhoneClient, error) {
	n := num.Add(1)
	id := fmt.Sprintf("phone-%d", n)

	c, err := siptest.NewClient(id, siptest.ClientConfig{
		Number: fmt.Sprintf("+%d", to+n),
		Codec:  codec,
		OnBye:  func() {},
		OnDTMF: func(ev dtmf.Event) {
			fmt.Println("DTMF C:", ev.Code, " D:", string(ev.Digit))
		},
	})
	if err != nil {
		return nil, err
	}

	p := &PhoneClient{
		Client: c,
	}
	if record {
		p.rec, err = os.Create(fmt.Sprintf("%s.mkv", id))
		if err != nil {
			return nil, err
		}
		c.Record(p.rec)
	}

	if err = p.Client.Dial(p.LocalIP()+":5060", Uri, fmt.Sprintf("+%d", to), nil); err != nil {
		return nil, err
	}

	return p, nil
}

func (p *PhoneClient) Close() {
	p.Client.Close()
	if rec := p.rec; rec != nil {
		_ = p.rec.Close()
	}
}
