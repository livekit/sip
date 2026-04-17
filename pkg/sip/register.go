package sip

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/icholy/digest"
	"github.com/pkg/errors"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils/guid"
	"github.com/livekit/psrpc"
	"github.com/livekit/sipgo/sip"
)

const (
	defaultRegisterExpiration = 300 * time.Second
)

func (c *Client) ensureRegistered(ctx context.Context, sipConf sipOutboundConfig) error {
	if c == nil || c.sipCli == nil || c.sconf == nil {
		return nil
	}
	if sipConf.user == "" || sipConf.pass == "" || sipConf.address == "" {
		return nil
	}

	tr := TransportFrom(sipConf.transport)
	if tr == "" {
		tr = TransportUDP
	}
	registrar := CreateURIFromUserAndAddress("", sipConf.address, tr)
	regUser := sipConf.user
	contact := c.ContactURI(tr)
	contact.User = regUser

	return c.register(ctx, registrar, regUser, sipConf.pass, contact)
}

func (c *Client) register(ctx context.Context, registrar URI, user, pass string, contact URI) error {
	callID := sip.CallIDHeader(guid.New("reg_"))
	fromTag := sip.GenerateTagN(16)
	authHeaderName := ""
	authHeaderValue := ""

	for attempt := 0; attempt < 5; attempt++ {
		req := c.newRegisterRequest(registrar, contact, user, fromTag, uint32(attempt+1), callID, authHeaderName, authHeaderValue)
		tx, err := c.sipCli.TransactionRequest(req)
		if err != nil {
			return err
		}

		resp, err := sipResponse(ctx, tx, c.closing.Watch(), nil)
		tx.Terminate()
		if err != nil {
			return err
		}

		switch resp.StatusCode {
		case sip.StatusOK:
			return nil
		case sip.StatusUnauthorized:
			authHeaderName = "Authorization"
		case sip.StatusProxyAuthRequired:
			authHeaderName = "Proxy-Authorization"
		case sip.StatusMethodNotAllowed, sip.StatusForbidden, sip.StatusNotFound, sip.StatusNotImplemented:
			c.log.Infow("SIP provider does not require REGISTER or rejected it, continuing without registration",
				"status", int(resp.StatusCode),
				"reason", resp.Reason,
				"address", registrar.GetHostPort(),
				"transport", registrar.Transport,
			)
			return nil
		default:
			return fmt.Errorf("REGISTER failed: %w", &livekit.SIPStatus{
				Code:   livekit.SIPStatusCode(resp.StatusCode),
				Status: resp.Reason,
			})
		}

		challengeHeader := resp.GetHeader(stringsForAuthHeader(authHeaderName))
		if challengeHeader == nil {
			return psrpc.NewError(psrpc.FailedPrecondition, errors.New("no auth header in sip register response"))
		}
		challenge, err := digest.ParseChallenge(challengeHeader.Value())
		if err != nil {
			return fmt.Errorf("invalid register challenge %q: %w", challengeHeader.Value(), err)
		}
		cred, err := digest.Digest(challenge, digest.Options{
			Method:   sip.REGISTER.String(),
			URI:      registrar.GetURI().String(),
			Username: user,
			Password: pass,
		})
		if err != nil {
			return err
		}
		authHeaderValue = cred.String()
	}

	return psrpc.NewError(psrpc.FailedPrecondition, fmt.Errorf("max auth retry attempts reached for SIP register"))
}

func (c *Client) newRegisterRequest(registrar URI, contact URI, user string, fromTag string, cseq uint32, callID sip.CallIDHeader, authHeaderName, authHeaderValue string) *sip.Request {
	registerURI := registrar.GetURI()
	aorURI := *registrar.GetURI()
	aorURI.User = user
	contactURI := *contact.GetContactURI()
	maxForwards := sip.MaxForwardsHeader(70)

	req := sip.NewRequest(sip.REGISTER, *registerURI)
	req.SetDestination(registrar.GetDest())
	req.AppendHeader(&sip.ToHeader{Address: aorURI})
	req.AppendHeader(&sip.FromHeader{
		Address: aorURI,
		Params:  sip.HeaderParams{{K: "tag", V: fromTag}},
	})
	req.AppendHeader(&sip.ContactHeader{Address: contactURI})
	req.AppendHeader(&callID)
	req.AppendHeader(&sip.CSeqHeader{SeqNo: cseq, MethodName: sip.REGISTER})
	req.AppendHeader(&maxForwards)
	req.AppendHeader(sip.NewHeader("Expires", strconv.Itoa(int(defaultRegisterExpiration/time.Second))))
	req.AppendHeader(sip.NewHeader("User-Agent", UserAgent))
	req.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, CANCEL, BYE, NOTIFY, REFER, MESSAGE, OPTIONS, INFO, SUBSCRIBE, REGISTER"))
	if authHeaderName != "" && authHeaderValue != "" {
		req.AppendHeader(sip.NewHeader(authHeaderName, authHeaderValue))
	}
	return req
}

func stringsForAuthHeader(authHeaderName string) string {
	switch authHeaderName {
	case "Authorization":
		return "WWW-Authenticate"
	case "Proxy-Authorization":
		return "Proxy-Authenticate"
	default:
		return ""
	}
}
