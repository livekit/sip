package service

import (
	"context"
	"fmt"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"

	"github.com/livekit/sip/pkg/sip"
)

func GetAuthCredentials(ctx context.Context, psrpcClient rpc.IOInfoClient, from, to, toHost, srcAddress string) (username, password string, drop bool, err error) {
	resp, err := psrpcClient.GetSIPTrunkAuthentication(ctx, &rpc.GetSIPTrunkAuthenticationRequest{
		From:       from,
		To:         to,
		ToHost:     toHost,
		SrcAddress: srcAddress,
	})

	if err != nil {
		return "", "", false, err
	}

	return resp.Username, resp.Password, resp.Drop, nil
}

func DispatchCall(ctx context.Context, psrpcClient rpc.IOInfoClient, log logger.Logger, info *sip.CallInfo) sip.CallDispatch {
	resp, err := psrpcClient.EvaluateSIPDispatchRules(ctx, &rpc.EvaluateSIPDispatchRulesRequest{
		SipCallId:     info.ID,
		CallingNumber: info.FromUser,
		CalledNumber:  info.ToUser,
		CalledHost:    info.ToHost,
		SrcAddress:    info.SrcAddress,
		Pin:           info.Pin,
		NoPin:         info.NoPin,
	})

	if err != nil {
		log.Warnw("SIP handle dispatch rule error", err)
		return sip.CallDispatch{Result: sip.DispatchNoRuleReject}
	}
	switch resp.Result {
	default:
		log.Errorw("SIP handle dispatch rule error", fmt.Errorf("unexpected dispatch result: %v", resp.Result))
		return sip.CallDispatch{Result: sip.DispatchNoRuleReject}
	case rpc.SIPDispatchResult_LEGACY_ACCEPT_OR_PIN:
		if resp.RequestPin {
			return sip.CallDispatch{Result: sip.DispatchRequestPin}
		}
		// TODO: finally deprecate and drop
		return sip.CallDispatch{
			Result: sip.DispatchAccept,
			Room: sip.RoomConfig{
				WsUrl:    resp.WsUrl,
				Token:    resp.Token,
				RoomName: resp.RoomName,
				Participant: sip.ParticipantConfig{
					Identity:   resp.ParticipantIdentity,
					Name:       resp.ParticipantName,
					Metadata:   resp.ParticipantMetadata,
					Attributes: resp.ParticipantAttributes,
				},
			},
			TrunkID:        resp.SipTrunkId,
			DispatchRuleID: resp.SipDispatchRuleId,
		}
	case rpc.SIPDispatchResult_ACCEPT:
		return sip.CallDispatch{
			Result: sip.DispatchAccept,
			Room: sip.RoomConfig{
				WsUrl:    resp.WsUrl,
				Token:    resp.Token,
				RoomName: resp.RoomName,
				Participant: sip.ParticipantConfig{
					Identity:   resp.ParticipantIdentity,
					Name:       resp.ParticipantName,
					Metadata:   resp.ParticipantMetadata,
					Attributes: resp.ParticipantAttributes,
				},
			},
			TrunkID:        resp.SipTrunkId,
			DispatchRuleID: resp.SipDispatchRuleId,
		}
	case rpc.SIPDispatchResult_REQUEST_PIN:
		return sip.CallDispatch{
			Result:  sip.DispatchRequestPin,
			TrunkID: resp.SipTrunkId,
		}
	case rpc.SIPDispatchResult_REJECT:
		return sip.CallDispatch{Result: sip.DispatchNoRuleReject}
	case rpc.SIPDispatchResult_DROP:
		return sip.CallDispatch{Result: sip.DispatchNoRuleDrop}
	}
}
