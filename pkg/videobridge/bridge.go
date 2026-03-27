// Copyright 2024 LiveKit, Inc.
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

// Package videobridge provides a SIP video bridge for LiveKit.
// It accepts SIP video calls (H.264), optionally transcodes to VP8,
// and publishes streams into LiveKit rooms as participants.
package videobridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/videobridge/config"
	"github.com/livekit/sip/pkg/videobridge/ingest"
	"github.com/livekit/sip/pkg/videobridge/observability"
	"github.com/livekit/sip/pkg/videobridge/publisher"
	"github.com/livekit/sip/pkg/videobridge/resilience"
	"github.com/livekit/sip/pkg/videobridge/security"
	"github.com/livekit/sip/pkg/videobridge/session"
	"github.com/livekit/sip/pkg/videobridge/signaling"
	"github.com/livekit/sip/pkg/videobridge/transcode"
)

// Bridge is the top-level coordinator for the SIP video bridge service.
type Bridge struct {
	log    logger.Logger
	conf   *config.Config
	nodeID string

	sipServer      *signaling.SIPServer
	sessionManager *session.Manager
	transcoderPool *transcode.Pool
	redisStore     *session.RedisStore

	// Safeguards
	guard     *resilience.SessionGuard
	flags     *resilience.FeatureFlags
	audit     *resilience.AuditLogger
	globalCB  *resilience.CircuitBreaker // trips across sessions → auto-disable video
	dynConfig *resilience.DynamicConfig  // hot-reloadable runtime config

	// Security
	srtpEnforcer *security.SRTPEnforcer
	authCfg      security.AuthConfig

	// Observability
	alertMgr       *observability.AlertManager
	tracerShutdown TracerShutdown

	healthServer  *http.Server
	metricsServer *http.Server
	cancelFn      context.CancelFunc
}

// NewBridge creates a new SIP video bridge.
func NewBridge(log logger.Logger, conf *config.Config) (*Bridge, error) {
	sipServer, err := signaling.NewSIPServer(log, conf)
	if err != nil {
		return nil, fmt.Errorf("creating SIP server: %w", err)
	}

	nodeID := generateNodeID()

	b := &Bridge{
		log:            log,
		conf:           conf,
		nodeID:         nodeID,
		sipServer:      sipServer,
		sessionManager: session.NewManager(log, conf),
		transcoderPool: transcode.NewPool(log, &conf.Transcode),
		guard: resilience.NewSessionGuard(log, resilience.SessionGuardConfig{
			MaxSessionsPerNode:   conf.Transcode.MaxConcurrent * 2,
			MaxSessionsPerCaller: 5,
			NewSessionRateLimit:  10.0,
			NewSessionBurst:      20,
		}),
		flags:        resilience.NewFeatureFlagsWithRegion(log, conf.Region),
		audit:        resilience.NewAuditLogger(log, nodeID, 1000),
		dynConfig:    resilience.NewDynamicConfig(log),
		srtpEnforcer: security.NewSRTPEnforcer(conf.SRTP),
		authCfg: security.AuthConfig{
			Enabled:   conf.Auth.Enabled,
			ApiKey:    conf.ApiKey,
			ApiSecret: conf.ApiSecret,
		},
		alertMgr: observability.NewAlertManager(log, observability.AlertManagerConfig{
			Enabled:        conf.Alerting.Enabled,
			WebhookURL:     conf.Alerting.WebhookURL,
			CooldownPeriod: conf.Alerting.CooldownPeriod,
			NodeID:         nodeID,
		}),
	}

	// Global circuit breaker: if many sessions fail, auto-disable video bridge
	b.globalCB = resilience.NewCircuitBreaker(log, resilience.CircuitBreakerConfig{
		Name:                "global_video",
		MaxFailures:         5, // 5 session-level circuit trips → disable video
		OpenDuration:        30 * time.Second,
		HalfOpenMaxAttempts: 2,
		OnStateChange: func(from, to resilience.CircuitState) {
			switch to {
			case resilience.StateOpen:
				b.flags.SetVideo(false)
				b.audit.Log(resilience.AuditEvent{
					Type:   resilience.AuditCircuitTripped,
					Detail: "global video auto-disabled due to high error rate",
				})
				b.alertMgr.FireCritical(observability.AlertCircuitBreakerTrip,
					"Global circuit breaker tripped: video auto-disabled", nil)
				log.Errorw("GLOBAL CIRCUIT BREAKER: video auto-disabled", nil)
			case resilience.StateClosed:
				b.flags.SetVideo(true)
				b.audit.Log(resilience.AuditEvent{
					Type:   resilience.AuditCircuitRecovered,
					Detail: "global video auto-re-enabled after recovery",
				})
				log.Infow("GLOBAL CIRCUIT BREAKER: video auto-re-enabled")
			}
		},
	})

	// Wire SIP call handler
	sipServer.SetCallHandler(b.handleInboundCall)

	// Default room resolver: derive room name from the To URI
	b.sessionManager.SetRoomResolver(func(call *signaling.InboundCall) string {
		return fmt.Sprintf("sip-video-%s", call.CallID)
	})

	return b, nil
}

// SetRoomResolver sets a custom function to map SIP calls to LiveKit room names.
func (b *Bridge) SetRoomResolver(resolver session.RoomResolver) {
	b.sessionManager.SetRoomResolver(resolver)
}

// Start begins the SIP video bridge service.
func (b *Bridge) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	b.cancelFn = cancel

	b.log.Infow("starting SIP video bridge",
		"nodeID", b.nodeID,
		"sipPort", b.conf.SIP.Port,
		"rtpPorts", fmt.Sprintf("%d-%d", b.conf.RTP.PortStart, b.conf.RTP.PortEnd),
		"defaultCodec", b.conf.Video.DefaultCodec,
		"transcodeEnabled", b.conf.Transcode.Enabled,
	)

	// Validate LiveKit credentials
	if b.conf.ApiKey == "" || b.conf.ApiSecret == "" || b.conf.WsUrl == "" {
		cancel()
		return fmt.Errorf("LiveKit credentials required: api_key, api_secret, ws_url")
	}

	// Connect to Redis if configured
	if b.conf.Redis.Address != "" {
		store, err := session.NewRedisStore(
			b.log, b.conf.Redis.Address, b.conf.Redis.Username, b.conf.Redis.Password,
			b.conf.Redis.DB, b.nodeID,
		)
		if err != nil {
			b.log.Warnw("Redis connection failed, running without distributed state", err)
		} else {
			b.redisStore = store
			store.StartHeartbeat(ctx)
			// Clean up sessions from dead nodes
			if cleaned, err := store.CleanupStale(ctx); err == nil && cleaned > 0 {
				b.log.Infow("cleaned stale sessions on startup", "count", cleaned)
			}
		}
	}

	// Start health check server
	if b.conf.HealthPort > 0 {
		b.startHealthServer()
	}

	// Start Prometheus metrics server
	if b.conf.PrometheusPort > 0 {
		b.startMetricsServer()
	}

	// Start SIP signaling server
	if err := b.sipServer.Start(); err != nil {
		cancel()
		return fmt.Errorf("starting SIP server: %w", err)
	}

	b.log.Infow("SIP video bridge started", "nodeID", b.nodeID)
	return nil
}

// Stop gracefully shuts down the bridge.
func (b *Bridge) Stop() {
	b.log.Infow("stopping SIP video bridge", "nodeID", b.nodeID)

	if b.cancelFn != nil {
		b.cancelFn()
	}

	// Stop accepting new calls first
	if b.sipServer != nil {
		b.sipServer.Close()
	}

	// Close all active sessions (graceful draining)
	b.sessionManager.CloseAll()

	// Disconnect Redis
	if b.redisStore != nil {
		b.redisStore.Close()
	}

	// Stop HTTP servers
	if b.healthServer != nil {
		b.healthServer.Close()
	}
	if b.metricsServer != nil {
		b.metricsServer.Close()
	}

	b.log.Infow("SIP video bridge stopped")
}

// handleInboundCall is called when a new SIP video INVITE is received.
// It creates a session, starts media bridging, and returns the SDP answer.
func (b *Bridge) handleInboundCall(ctx context.Context, call *signaling.InboundCall) error {
	ctx, span := Tracer.Start(ctx, "videobridge.handleInboundCall")
	defer span.End()
	span.SetAttributes(
		attribute.String("sip.call_id", call.CallID),
		attribute.String("sip.from", call.FromURI),
		attribute.String("sip.to", call.ToURI),
		attribute.String("video.codec", call.Media.VideoCodec),
	)

	log := b.log.WithValues("callID", call.CallID, "from", call.FromURI, "to", call.ToURI)
	log.Infow("handling inbound video call")

	// Global kill switch: is video bridge disabled?
	if b.flags.IsDisabled("video") {
		span.SetAttributes(attribute.String("reject_reason", "video_disabled"))
		log.Warnw("rejecting call: video bridge disabled via kill switch", nil)
		return fmt.Errorf("video bridge disabled via kill switch")
	}

	// Feature flag: are new sessions allowed?
	if b.flags.IsDisabled("new_sessions") {
		span.SetAttributes(attribute.String("reject_reason", "new_sessions_disabled"))
		return fmt.Errorf("new sessions disabled via feature flag")
	}

	// Session guard: rate limit + per-node + per-caller limits
	if err := b.guard.Admit(call.FromURI); err != nil {
		span.SetAttributes(attribute.String("reject_reason", "guard_rejected"))
		span.RecordError(err)
		return fmt.Errorf("session guard: %w", err)
	}

	// Rollback tracker: clean up on any partial failure
	rb := resilience.NewRollback(log)
	defer rb.Execute()

	// Guard will be released on session close, but if we fail before session starts:
	rb.Add("release_guard", func() error {
		b.guard.Release(call.FromURI)
		return nil
	})

	// Create session
	_, createSpan := Tracer.Start(ctx, "videobridge.createSession")
	sess, err := b.sessionManager.CreateSession(call)
	if err != nil {
		createSpan.RecordError(err)
		createSpan.SetStatus(codes.Error, err.Error())
		createSpan.End()
		span.RecordError(err)
		return fmt.Errorf("creating session: %w", err)
	}
	createSpan.SetAttributes(attribute.String("session.id", sess.ID))
	createSpan.End()

	// Create and wire components into the session
	_, startSpan := Tracer.Start(ctx, "videobridge.startSession")

	// Create RTP receiver
	receiver, err := createReceiver(log, b.conf, call)
	if err != nil {
		startSpan.RecordError(err)
		startSpan.SetStatus(codes.Error, err.Error())
		startSpan.End()
		span.RecordError(err)
		sess.Close()
		return fmt.Errorf("creating receiver: %w", err)
	}

	// Create publisher with retry
	pub := publisher.NewPublisher(log, publisher.PublisherConfig{
		WsURL:     b.conf.WsUrl,
		ApiKey:    b.conf.ApiKey,
		ApiSecret: b.conf.ApiSecret,
		RoomName:  sess.RoomName,
		Identity:  fmt.Sprintf("sip-video-%s", call.CallID),
		Name:      fmt.Sprintf("SIP Video (%s)", call.FromURI),
		Metadata:  fmt.Sprintf(`{"sip_call_id":"%s","from":"%s","to":"%s"}`, call.CallID, call.FromURI, call.ToURI),
		Attributes: map[string]string{
			"sip.callID": call.CallID,
			"sip.from":   call.FromURI,
			"sip.to":     call.ToURI,
		},
		VideoCodec: b.conf.Video.DefaultCodec,
		MaxBitrate: b.conf.Video.MaxBitrate,
	})

	// Connect to LiveKit room with retry
	if err := resilience.Do(ctx, log, "room_join", resilience.RoomJoinRetryConfig(), func(ctx context.Context) error {
		return pub.Connect(ctx)
	}); err != nil {
		receiver.Close()
		startSpan.RecordError(err)
		startSpan.SetStatus(codes.Error, err.Error())
		startSpan.End()
		span.RecordError(err)
		sess.Close()
		return fmt.Errorf("connecting publisher: %w", err)
	}

	// Create audio bridge
	audioCodecType := ingest.G711PCMU
	if call.Media.AudioCodec == "PCMA" {
		audioCodecType = ingest.G711PCMA
	}
	audioBridge := ingest.NewAudioBridge(log, audioCodecType)

	// Wire global circuit breaker: session CB trip → global CB failure
	sess.SetOnCircuitTrip(func(sessionID string) {
		b.globalCB.RecordFailure(fmt.Errorf("session %s publisher circuit tripped", sessionID))
		b.audit.Failure(sessionID, call.CallID, "publisher", "circuit breaker tripped")
	})

	// Audit: session start
	b.audit.SessionStart(sess.ID, call.CallID, sess.RoomName, call.FromURI)

	// Wire components into session (decoupled)
	sess.SetComponents(receiver, pub, audioBridge, nil)
	audioBridge.SetOutput(sess)
	receiver.SetHandler(sess)

	// Start the session state machine
	if err := sess.Start(ctx); err != nil {
		receiver.Close()
		pub.Close()
		startSpan.RecordError(err)
		startSpan.SetStatus(codes.Error, err.Error())
		startSpan.End()
		span.RecordError(err)
		return fmt.Errorf("starting session: %w", err)
	}

	// Start RTP receiver
	receiver.Start()

	startSpan.SetAttributes(
		attribute.Int("rtp.video_port", sess.VideoPort()),
		attribute.Int("rtp.audio_port", sess.AudioPort()),
	)
	startSpan.End()

	// Build SDP answer with our local RTP ports
	localIP := b.sipServer.LocalIP()
	call.LocalSDP = signaling.BuildVideoSDP(
		localIP,
		sess.VideoPort(),
		sess.AudioPort(),
		b.conf.Video.H264Profile,
	)

	span.SetAttributes(
		attribute.String("session.id", sess.ID),
		attribute.String("session.room", sess.RoomName),
	)

	log.Infow("session started, SDP answer ready",
		"videoPort", sess.VideoPort(),
		"audioPort", sess.AudioPort(),
		"sessionID", sess.ID,
	)

	return nil
}

// createReceiver creates an RTP receiver from the call's negotiated media.
func createReceiver(log logger.Logger, conf *config.Config, call *signaling.InboundCall) (*ingest.RTPReceiver, error) {
	receiver, err := ingest.NewRTPReceiver(log, ingest.RTPReceiverConfig{
		PortStart:           conf.RTP.PortStart,
		PortEnd:             conf.RTP.PortEnd,
		VideoPayloadType:    call.Media.VideoPayloadType,
		AudioPayloadType:    call.Media.AudioPayloadType,
		JitterBuffer:        conf.RTP.JitterBuffer,
		JitterLatency:       conf.RTP.JitterLatency,
		MediaTimeout:        conf.RTP.MediaTimeout,
		MediaTimeoutInitial: conf.RTP.MediaTimeoutInitial,
	})
	if err != nil {
		return nil, err
	}
	if call.Media.RemoteAddr.IsValid() {
		receiver.SetRemoteAddr(call.Media.RemoteAddr)
	}
	return receiver, nil
}

func (b *Bridge) startHealthServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		resp := map[string]interface{}{
			"status":          "ok",
			"version":         APIVersion,
			"node_id":         b.nodeID,
			"active_sessions": b.sessionManager.ActiveCount(),
			"active_calls":    b.sipServer.ActiveCalls(),
			"flags":           b.flags.Snapshot(),
		}
		json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/flags", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			var snap resilience.FlagSnapshot
			if err := json.NewDecoder(r.Body).Decode(&snap); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			b.flags.ApplySnapshot(snap)
			b.log.Infow("feature flags updated via API", "flags", snap)
		}
		json.NewEncoder(w).Encode(b.flags.Snapshot())
	})
	mux.HandleFunc("/kill", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		b.flags.DisableAll()
		b.audit.KillSwitch(r.RemoteAddr)
		b.log.Warnw("kill switch activated via API", nil, "remote", r.RemoteAddr)
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"killed":true,"node":"%s"}`, b.nodeID)
	})
	mux.HandleFunc("/revive", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		b.flags.EnableAll()
		b.audit.Revive(r.RemoteAddr)
		b.log.Infow("revive activated via API", "remote", r.RemoteAddr)
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"revived":true,"node":"%s"}`, b.nodeID)
	})
	mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode(b.dynConfig.Snapshot())
		case http.MethodPatch, http.MethodPost:
			var update resilience.DynamicConfigUpdate
			if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			errs := b.dynConfig.Apply(update, "api:"+r.RemoteAddr)
			resp := map[string]interface{}{"config": b.dynConfig.Snapshot()}
			if len(errs) > 0 {
				errStrs := make([]string, len(errs))
				for i, e := range errs {
					errStrs[i] = e.Error()
				}
				resp["errors"] = errStrs
			}
			json.NewEncoder(w).Encode(resp)
		default:
			http.Error(w, "GET, POST, or PATCH only", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/config/changes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(b.dynConfig.RecentChanges(50))
	})
	mux.HandleFunc("/audit", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(b.audit.Recent(100))
	})
	mux.HandleFunc("/sessions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(b.sessionManager.ListSessions())
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		// Readiness: reject traffic if at capacity
		active := b.sessionManager.ActiveCount()
		max := b.conf.Transcode.MaxConcurrent
		if max > 0 && active >= max {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, `{"ready":false,"reason":"at capacity","active":%d,"max":%d}`, active, max)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"ready":true,"active":%d,"max":%d,"node":"%s"}`, active, max, b.nodeID)
	})

	// Wrap with auth middleware if enabled
	writePaths := map[string]bool{
		"/kill":   true,
		"/revive": true,
		"/flags":  true,
		"/config": true,
	}
	authMW := security.AuthMiddleware(b.authCfg, writePaths)
	handler := authMW(mux)

	b.healthServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", b.conf.HealthPort),
		Handler: handler,
	}

	// TLS support
	tlsCfg, tlsErr := security.BuildTLSConfig(b.conf.TLS)
	if tlsErr != nil {
		b.log.Errorw("TLS config error, falling back to plain HTTP", tlsErr)
	}
	if tlsCfg != nil {
		b.healthServer.TLSConfig = tlsCfg
	}

	go func() {
		b.log.Infow("health server starting", "port", b.conf.HealthPort, "tls", tlsCfg != nil, "auth", b.authCfg.Enabled)
		var err error
		if tlsCfg != nil {
			err = b.healthServer.ListenAndServeTLS("", "") // certs already in TLSConfig
		} else {
			err = b.healthServer.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			b.log.Errorw("health server error", err)
		}
	}()
}

func generateNodeID() string {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	short := uuid.New().String()[:8]
	return fmt.Sprintf("%s-%s", hostname, short)
}

func (b *Bridge) startMetricsServer() {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	b.metricsServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", b.conf.PrometheusPort),
		Handler: mux,
	}
	go func() {
		b.log.Infow("metrics server starting", "port", b.conf.PrometheusPort)
		if err := b.metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			b.log.Errorw("metrics server error", err)
		}
	}()
}
