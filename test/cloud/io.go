package cloud

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/sip"
	"github.com/livekit/psrpc"
)

const (
	TrunkID        = "ST_INTEGRATION"
	DispatchRuleID = "SDR_INTEGRATION"
)

type IOTestClient struct {
	rpc.IOInfoClient

	conf *IntegrationConfig
}

func NewIOTestClient(conf *IntegrationConfig) rpc.IOInfoClient {
	return &IOTestClient{conf: conf}
}

func (c *IOTestClient) GetSIPTrunkAuthentication(_ context.Context, _ *rpc.GetSIPTrunkAuthenticationRequest, _ ...psrpc.RequestOption) (*rpc.GetSIPTrunkAuthenticationResponse, error) {
	return &rpc.GetSIPTrunkAuthenticationResponse{
		SipTrunkId: TrunkID,
		ProjectId:  ProjectID,
	}, nil
}

func (c *IOTestClient) EvaluateSIPDispatchRules(_ context.Context, _ *rpc.EvaluateSIPDispatchRulesRequest, _ ...psrpc.RequestOption) (*rpc.EvaluateSIPDispatchRulesResponse, error) {
	identity := fmt.Sprintf("phone-%d", num.Load())
	token, err := sip.BuildSIPToken(sip.SIPTokenParams{
		APIKey:              c.conf.ApiKey,
		APISecret:           c.conf.ApiSecret,
		RoomName:            c.conf.RoomName,
		ParticipantIdentity: identity,
		ParticipantName:     identity,
		RoomConfig:          nil,
	})
	if err != nil {
		logger.Errorw("failed to build SIP token", err)
		return nil, err
	}

	return &rpc.EvaluateSIPDispatchRulesResponse{
		RoomName:            c.conf.RoomName,
		ParticipantIdentity: identity,
		ParticipantName:     identity,
		Token:               token,
		WsUrl:               c.conf.WsUrl,
		Result:              rpc.SIPDispatchResult_ACCEPT,
		SipTrunkId:          TrunkID,
		SipDispatchRuleId:   DispatchRuleID,
		ProjectId:           ProjectID,
		RoomConfig:          nil,
	}, nil
}

func (c *IOTestClient) UpdateSIPCallState(_ context.Context, _ *rpc.UpdateSIPCallStateRequest, _ ...psrpc.RequestOption) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}
