package sip

import (
	"errors"
	"sync"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/psrpc"
	"golang.org/x/exp/maps"
)

type CallCache struct {
	log logger.Logger

	cmu           sync.RWMutex
	countInbound  int
	countOutbound int
	calls         map[LocalTag]Signaling
}

func NewCallCache(log logger.Logger) *CallCache {
	return &CallCache{
		log:   log,
		calls: make(map[LocalTag]Signaling),
	}
}

func (c *CallCache) Get(localTag LocalTag) Signaling {
	c.cmu.RLock()
	call := c.calls[localTag]
	c.cmu.RUnlock()
	return call
}

func (c *CallCache) Add(call Signaling) error {
	localTag := call.ID()
	c.cmu.Lock()
	defer c.cmu.Unlock()
	if c.calls[localTag] != nil {
		return psrpc.NewErrorf(psrpc.AlreadyExists, "call already exists")
	}
	if call.IsOutbound() {
		c.countOutbound++
	} else {
		c.countInbound++
	}
	c.calls[localTag] = call
	return nil
}

func (c *CallCache) Remove(localTag LocalTag) bool {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	call := c.calls[localTag]
	if call == nil {
		return false
	}
	if call.IsOutbound() {
		c.countOutbound--
	} else {
		c.countInbound--
	}
	delete(c.calls, localTag)
	return true
}

func (c *CallCache) Total() int {
	c.cmu.RLock()
	defer c.cmu.RUnlock()
	return len(c.calls)
}

// Requires lock to be held
func (c *CallCache) sample(limit int) ([]string, int) {
	total := len(c.calls)
	var out []string
	for _, v := range c.calls {
		if limit <= 0 {
			break
		}
		id := "nil"
		if v != nil {
			id = string(v.ID())
		}
		out = append(out, id)
		limit--
	}
	return out, total
}

func (c *CallCache) ActiveCalls() ActiveCalls {
	st := ActiveCalls{}
	c.cmu.RLock()
	defer c.cmu.RUnlock()
	st.Inbound = c.countInbound
	st.Outbound = c.countOutbound
	st.SampleIDs, _ = c.sample(5)
	return st
}

func (c *CallCache) closeAll(isOutbound bool) error {
	// TODO: remove when we take care of the cleanup path
	c.cmu.RLock()
	calls := maps.Values(c.calls)
	c.cmu.RUnlock()
	var errs []error
	for _, call := range calls {
		if call.IsOutbound() != isOutbound {
			continue
		}
		errs = append(errs, call.Close())
	}
	return errors.Join(errs...)
}

func (c *CallCache) CloseAllOutbound() error {
	return c.closeAll(true)
}
func (c *CallCache) CloseAllInbound() error {
	return c.closeAll(false)
}

func (c *CallCache) Close() error {
	c.cmu.RLock()
	defer c.cmu.RUnlock()
	samples, total := c.sample(5)
	if total != 0 {
		c.log.Infow("closing call cache with active calls", "calls", total, "sampleIDs", samples)
	}
	return nil
}

func (c *CallCache) PrevioudReinviteHandling(call Signaling) bool {
	/* Dumped moved thhing here:
	s.cmu.RLock()
	existing := s.byLocalTag[cc.ID()]
	s.cmu.RUnlock()
	if existing != nil && existing.cc.InviteCSeq() < cc.InviteCSeq() {
		log.Infow("accepting reinvite", "content-type", req.ContentType(), "content-length", req.ContentLength())
		existing.log().Infow("reinvite", "content-type", req.ContentType(), "content-length", req.ContentLength(), "cseq", cc.InviteCSeq())
		cc.AcceptAsKeepAlive(existing.cc.OwnSDP())
		return nil
	}
	*/
	return false
}

func (c *CallCache) PreviousUnhandledCallback() {
	/* s.cli.OnRequest */
}

func (c *CallCache) PreviousCallInfoCount() {
	/*
				getCallInfo

		func (c *inboundCallInfo) countInvite(log logger.Logger, req *sip.Request) {
			hasAuth := inviteHasAuth(req)
			cseq := req.CSeq()
			if cseq == nil {
				return
			}
			c.Lock()
			defer c.Unlock()
			cseqPtr := &c.cseq
			countPtr := &c.invites
			name := "invite"
			if hasAuth {
				cseqPtr = &c.cseqAuth
				countPtr = &c.invitesAuth
				name = "invite with auth"
			}
			if *cseqPtr == 0 {
				*cseqPtr = cseq.SeqNo
			}
			if cseq.SeqNo > *cseqPtr {
				return // reinvite
			}
			*countPtr++
			if *countPtr > 1 {
				log.Warnw("remote appears to be retrying an "+name, nil, "invites", *countPtr, "cseq", *cseqPtr)
			}
		}
	*/
}
