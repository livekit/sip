// Copyright 2026 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sip

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/psrpc"
	"github.com/livekit/sipgo/sip"
)

// respPump is the only reader of an INVITE client transaction's response
// channel, for the whole life of the transaction. It moves every response into
// an unbounded queue at once. The transaction layer blocks while it passes a
// response up to a full channel, and tx.Cancel and tx.Terminate wait for the
// transaction's locks, so a reader that is itself inside tx.Cancel or
// tx.Terminate must never be the one that drains the channel.
type respPump struct {
	mu     sync.Mutex
	q      []*sip.Response
	notify chan struct{} // signalled after a push (capacity 1)
	closed chan struct{} // closed once the transaction ended and all its responses are queued
}

func startRespPump(resps <-chan *sip.Response, done <-chan struct{}) *respPump {
	p := &respPump{notify: make(chan struct{}, 1), closed: make(chan struct{})}
	go func() {
		defer close(p.closed)
		for {
			select {
			case r, ok := <-resps:
				if !ok || r == nil {
					return
				}
				p.push(r)
			case <-done:
				// The transaction ended: queue what it passed up before that.
				for {
					select {
					case r, ok := <-resps:
						if !ok || r == nil {
							return
						}
						p.push(r)
					default:
						return
					}
				}
			}
		}
	}()
	return p
}

func (p *respPump) push(r *sip.Response) {
	p.mu.Lock()
	p.q = append(p.q, r)
	p.mu.Unlock()
	select {
	case p.notify <- struct{}{}:
	default:
	}
}

// pop returns the next queued response, or nil.
func (p *respPump) pop() *sip.Response {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.q) == 0 {
		return nil
	}
	r := p.q[0]
	p.q[0] = nil
	p.q = p.q[1:]
	return r
}

func (p *respPump) empty() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.q) == 0
}

// closedSignal is always ready; used to keep a select from blocking.
var closedSignal = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

// waitInviteResponse waits for the final response to the INVITE transaction tx.
// It is the only place that acts on a CancelIfUnanswered request:
//   - before any 1xx, the CANCEL is deferred until the first 1xx (RFC 3261 §9.1);
//   - once a request is recorded, the ringing timeout and room disconnects (ctx)
//     no longer end the wait (the outcome arbitrates request against abort), so a
//     crossing 2xx is returned and ACKed;
//   - after the CANCEL, the wait ends with a final response, the end of tx, or
//     cancelTimeout (64*T1, §9.1); on unreliable transports the CANCEL is
//     retransmitted (Timer E) until then;
//   - at cancelTimeout the transaction is terminated first, so every response it
//     passed up before is in the queue (sipgo's delete excludes passUp), and only
//     then is the result decided: a queued final response still wins.
//
// Requests, timers and shutdown are serviced between queued responses, so a
// stream of responses cannot starve them.
//
// Without a cancel request it behaves like sipResponse. cancelled reports why
// the wait ended after a CANCEL on a live transaction without a final response
// ("timeout", "hangup" or "shutdown", as sipResponse); the caller then hands tx
// to watchCancelledInviteFrom, which ACKs and BYEs a late 2xx. cseq is the
// INVITE's, for the signaling events.
func (c *sipOutbound) waitInviteResponse(ctx context.Context, tx sip.ClientTransaction, pump *respPump, reliable bool, cseq uint32, setState sipRespFunc) (_ *sip.Response, cancelled string, _ error) {
	var (
		cnt        = 0
		ctxDone    = ctx.Done()
		stop       = c.closing.Watch()
		reqCh      = c.outcome.requestedCh()
		deferred   bool // cancel requested before any 1xx
		cancelSent bool
		deadline   *time.Timer
		deadlineC  <-chan time.Time
		retrans    *time.Timer
		retransC   <-chan time.Time
		interval   time.Duration
	)
	defer func() {
		if deadline != nil {
			deadline.Stop()
		}
		if retrans != nil {
			retrans.Stop()
		}
	}()
	sendCancel := func() {
		cancelSent = true
		_ = tx.Cancel()
		c.log.Infow("CancelIfUnanswered: CANCEL requested")
		// sipgo ignores Cancel once the INVITE has a final response, so this
		// reports the request handed to the transaction, not a CANCEL on the wire.
		c.emitEvent(SignalingEvent{Type: EventCancelRequested, Time: time.Now(), SIPMethod: string(sip.CANCEL), CSeq: cseq, Reason: "cancel_if_unanswered"})
		deadline = time.NewTimer(c.timers.cancelTimeout)
		deadlineC = deadline.C
		if !reliable && c.timers.retransmit > 0 {
			interval = c.timers.retransmit
			retrans = time.NewTimer(interval)
			retransC = retrans.C
		}
	}
	onRequest := func() {
		reqCh = nil
		ctxDone = nil // from now on only SIP evidence or cancelTimeout end the wait
		if cancelSent {
			return
		}
		if cnt == 0 {
			deferred = true
			c.log.Infow("CancelIfUnanswered: no provisional response yet, CANCEL deferred")
		} else {
			sendCancel()
		}
	}
	// handle processes one response; it returns true for a final response.
	handle := func(res *sip.Response) bool {
		if setState != nil {
			setState(res.StatusCode, res.Headers())
		}
		if res.StatusCode/100 != 1 {
			return true
		}
		cnt++
		if deferred && !cancelSent {
			deferred = false
			sendCancel()
		}
		return false
	}
	for {
		res := pump.pop()
		if res != nil && handle(res) {
			return res, "", nil
		}
		next := pump.notify
		if res != nil {
			next = closedSignal // more may be queued: do not block, but let ready signals in first
		}
		select {
		case <-next:
		case <-pump.closed:
			if pump.empty() {
				return nil, "", psrpc.NewError(psrpc.Canceled, transactionTimeoutError{responses: cnt})
			}
		case <-ctxDone:
			if !c.outcome.abort() {
				onRequest() // a request was recorded first; it wins over the abort
				continue
			}
			_ = tx.Cancel()
			reason := "hangup"
			if errors.Is(context.Cause(ctx), ErrSIPRequestTimeout) {
				reason = "timeout"
			}
			// NOTE: psrpc.Canceled does not auto-retry, whereas psrpc.DeadlineExceeded does
			// As long as that is the case, avoid psrpc.DeadlineExceeded to prevent hammering of destination.
			return nil, reason, psrpc.NewError(psrpc.Canceled, ErrSIPRequestTimeout)
		case <-stop:
			_ = tx.Cancel()
			return nil, "shutdown", psrpc.NewErrorf(psrpc.Canceled, "service shutting down")
		case <-reqCh:
			onRequest()
		case <-retransC:
			_ = tx.Cancel()
			interval *= 2
			if interval > c.timers.retransmitMax {
				interval = c.timers.retransmitMax
			}
			retrans.Reset(interval)
		case <-deadlineC:
			// Linearize at the end of the transaction: after Terminate the pump
			// holds every response the transaction passed up, and nothing follows.
			tx.Terminate()
			<-pump.closed
			for r := pump.pop(); r != nil; r = pump.pop() {
				if handle(r) {
					return r, "", nil
				}
			}
			c.log.Infow("CancelIfUnanswered: no final response after CANCEL")
			return nil, "", cancelIfUnansweredError{result: CancelResultUnconfirmed}
		}
	}
}

// watchAcceptedInvite owns the INVITE client transaction after its first 2xx.
// Each 2xx retransmission of dialog 1 is ACKed again with ack (built when the
// first 2xx arrived, so also before the call sends its own first ACK). A 2xx
// from another dialog (a fork) is ACKed and that dialog alone is hung up
// (RFC 3261 §13.2.2.4). It runs until the transaction ends (Timer M) or
// acceptedWatch passes, and handles every response the transaction passed up
// before it ended.
func (c *sipOutbound) watchAcceptedInvite(cli SIPClient, tx sip.ClientTransaction, pump *respPump, req *sip.Request, first *sip.Response, ack *sip.Request) {
	timer := time.NewTimer(c.timers.acceptedWatch)
	defer timer.Stop()
	timerC := timer.C
	tag1 := toTagOf(first)
	forks := make(map[string]*sip.Request)
	for {
		// One response per round, so the timer is served under any load.
		res := pump.pop()
		if res != nil && res.StatusCode/100 == 2 {
			tag := toTagOf(res)
			if tag == tag1 {
				if err := cli.WriteRequest(ack); err != nil {
					c.log.Warnw("failed to re-send ACK for INVITE 2xx retransmission", err)
				}
			} else if fack, ok := forks[tag]; ok {
				if err := cli.WriteRequest(fack); err != nil {
					c.log.Warnw("failed to re-send ACK for a forked INVITE 2xx", err)
				}
			} else {
				c.log.Infow("INVITE answered by another dialog; sending ACK and BYE for it", "toTag", tag)
				forks[tag] = ackAndBye(c.log, cli, nil, req, res)
			}
		}
		next := pump.notify
		if res != nil {
			next = closedSignal // more may be queued: do not block, but let the timer in first
		}
		select {
		case <-next:
		case <-pump.closed:
			if pump.empty() {
				return
			}
		case <-timerC:
			timerC = nil
			tx.Terminate() // the pump then queues what is left and closes
		}
	}
}

// dialogACK builds the ACK for the dialog that the 2xx res establishes.
func dialogACK(invite *sip.Request, res *sip.Response) *sip.Request {
	dreq := invite.Clone()
	applyInviteResponse(dreq, res) // route to that dialog's Contact and route set
	return sip.NewAckRequest(dreq, res, nil)
}

// ackAndBye ACKs the 2xx res and hangs up the dialog it established. It returns the ACK.
// Write errors are logged; the next retransmission is ACKed again.
func ackAndBye(log logger.Logger, cli SIPClient, getHeaders setHeadersFunc, invite *sip.Request, res *sip.Response) *sip.Request {
	dreq := invite.Clone()
	applyInviteResponse(dreq, res) // route ACK/BYE to the answering Contact
	ack := sip.NewAckRequest(dreq, res, nil)
	if err := cli.WriteRequest(ack); err != nil {
		log.Warnw("failed to send ACK for an INVITE 2xx that is hung up", err)
	}
	bye := sip.NewByeRequest(dreq, res, nil)
	bye.AppendHeader(sip.NewHeader("User-Agent", "LiveKit"))
	if getHeaders != nil {
		for k, v := range getHeaders(nil) {
			bye.AppendHeader(sip.NewHeader(k, v))
		}
	}
	if err := cli.WriteRequest(bye); err != nil {
		log.Warnw("failed to send BYE for an INVITE 2xx", err)
	}
	return ack
}

// watchCancelledInviteFrom catches 2xx responses racing with our CANCEL: each
// new dialog is ACKed and BYEd, so no answered call is orphaned; a
// retransmission of a handled dialog is ACKed again. It owns tx: after
// cancelResponseGrace it terminates tx and still handles every response that
// was passed up before. It reads the transaction through pump, its only reader.
func watchCancelledInviteFrom(log logger.Logger, cli SIPClient, getHeaders setHeadersFunc, req *sip.Request, tx sip.ClientTransaction, pump *respPump, onAnswered func(*sip.Response)) {
	defer tx.Terminate()
	timer := time.NewTimer(cancelResponseGrace)
	defer timer.Stop()
	timerC := timer.C
	acks := make(map[string]*sip.Request)
	for {
		// One response per round, so the grace timer is served under any load.
		// Provisionals and non-2xx finals (ACKed by the transaction layer) need nothing.
		res := pump.pop()
		if res != nil && res.StatusCode/100 == 2 {
			tag := toTagOf(res)
			if ack, ok := acks[tag]; ok {
				if err := cli.WriteRequest(ack); err != nil {
					log.Warnw("failed to re-send ACK for a cancelled INVITE 2xx", err)
				}
			} else {
				log.Infow("cancelled INVITE answered; sending ACK and BYE")
				if onAnswered != nil {
					onAnswered(res)
				}
				acks[tag] = ackAndBye(log, cli, getHeaders, req, res)
			}
		}
		next := pump.notify
		if res != nil {
			next = closedSignal // more may be queued: do not block, but let the timer in first
		}
		select {
		case <-next:
		case <-pump.closed:
			if pump.empty() {
				return
			}
		case <-timerC:
			timerC = nil
			tx.Terminate() // the pump then queues what is left and closes
		}
	}
}
