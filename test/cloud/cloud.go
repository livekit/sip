package cloud

import (
	"context"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/sip"
	"github.com/livekit/psrpc"
)

type CloudTestService struct {
	conf *IntegrationConfig

	psrpcClient rpc.SIPInternalClient
}

func NewCloudTestService(conf *IntegrationConfig, bus psrpc.MessageBus) (*CloudTestService, error) {
	c, err := rpc.NewSIPInternalClient(bus)
	if err != nil {
		return nil, err
	}

	return &CloudTestService{
		conf:        conf,
		psrpcClient: c,
	}, nil
}

func (s *CloudTestService) CreateSIPParticipant(ctx context.Context, req *livekit.CreateSIPParticipantRequest) (*livekit.SIPParticipantInfo, error) {
	token, err := sip.BuildSIPToken(sip.SIPTokenParams{
		APIKey:                s.conf.ApiKey,
		APISecret:             s.conf.ApiSecret,
		RoomName:              req.RoomName,
		ParticipantIdentity:   req.ParticipantIdentity,
		ParticipantName:       req.ParticipantName,
		ParticipantMetadata:   req.ParticipantMetadata,
		ParticipantAttributes: req.ParticipantAttributes,
	})
	if err != nil {
		logger.Errorw("failed to create SIP token", err)
		return nil, err
	}

	callID := sip.NewCallID()
	trunk := &livekit.SIPOutboundTrunkInfo{}
	r, err := rpc.NewCreateSIPParticipantRequest(ProjectID, callID, Host, s.conf.WsUrl, token, req, trunk)
	if err != nil {
		logger.Errorw("failed to build CreateSIPParticipantRequest", err)
		return nil, err
	}

	resp, err := s.psrpcClient.CreateSIPParticipant(ctx, s.conf.ClusterID, r, psrpc.WithRequestTimeout(time.Second*30))
	if err != nil {
		logger.Errorw("failed to create SIP participant", err)
		return nil, err
	}

	return &livekit.SIPParticipantInfo{
		ParticipantId:       resp.ParticipantId,
		ParticipantIdentity: resp.ParticipantIdentity,
		RoomName:            req.RoomName,
		SipCallId:           r.SipCallId,
	}, nil
}
