// Copyright 2026 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package lksdk

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	protoLogger "github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"go.uber.org/zap/zapcore"

	"github.com/livekit/server-sdk-go/v2/signalling"
)

const (
	cConnectTimeoutDefault = 15 * time.Second
	cValidateTimeout       = 3 * time.Second
	cOriginalRegion        = "__original__"
)

// -------------------------------------------

type connectionManagerState int

const (
	connectionManagerStateInitial connectionManagerState = iota
	connectionManagerStateConnected
	connectionManagerStateResuming
	connectionManagerStateReconnecting
	connectionManagerStateClosed
)

func (c connectionManagerState) String() string {
	switch c {
	case connectionManagerStateInitial:
		return "INITIAL"
	case connectionManagerStateConnected:
		return "CONNECTED"
	case connectionManagerStateResuming:
		return "RESUMING"
	case connectionManagerStateReconnecting:
		return "RECONNECTING"
	case connectionManagerStateClosed:
		return "CLOSED"
	default:
		return fmt.Sprintf("UNKNOWN (%d)", c)
	}
}

// -------------------------------------------

type connectionRequestParams struct {
	ctx                    context.Context
	url                    string
	token                  string
	connectParams          signalling.ConnectParams
	disableRegionDiscovery bool
}

// -------------------------------------------

type connectionAttemptParams struct {
	ctx             context.Context
	backoffWait     time.Duration
	region          *livekit.RegionInfo
	token           string
	validateTimeout time.Duration
}

func (c connectionAttemptParams) MarshalLogObject(e zapcore.ObjectEncoder) error {
	deadline, hasDeadline := c.ctx.Deadline()
	if hasDeadline {
		e.AddDuration("ctxDeadline", time.Until(deadline))
	} else {
		e.AddString("ctxDeadline", "not-set")
	}
	e.AddDuration("backoffWait", c.backoffWait)
	e.AddObject("region", protoLogger.Proto(c.region))
	e.AddDuration("validateTimeout", c.validateTimeout)
	return nil
}

// -------------------------------------------

type connectionManager struct {
	mu sync.RWMutex

	log            protoLogger.Logger
	regionProvider *regionURLProvider

	incomingRequestParams connectionRequestParams

	token string

	connectedRegion *livekit.RegionInfo

	regionSettings *livekit.RegionSettings

	state connectionManagerState

	// recovering reports whether a recovery (resume/reconnect) worker is currently
	// running. It is guarded by mu, the same lock as state, so that applying a
	// recovery transition and starting/stopping the worker are decided together in
	// a single critical section. The invariant this maintains is: a worker is
	// running if and only if state requires recovery.
	recovering bool
}

func newConnectionManager(regionProvider *regionURLProvider) *connectionManager {
	return &connectionManager{
		log:            logger,
		regionProvider: regionProvider,
		state:          connectionManagerStateInitial,
	}
}

func (c *connectionManager) setLogger(l protoLogger.Logger) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.log = l
}

func (c *connectionManager) setIncomingRequestParams(
	ctx context.Context,
	url string,
	token string,
	connectParams *signalling.ConnectParams,
	disableRegionDiscovery bool,
) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.incomingRequestParams = connectionRequestParams{
		ctx:                    ctx,
		url:                    url,
		token:                  token,
		connectParams:          *connectParams,
		disableRegionDiscovery: disableRegionDiscovery,
	}
	c.token = token
}

func (c *connectionManager) getConnectParams() signalling.ConnectParams {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.incomingRequestParams.connectParams
}

func (c *connectionManager) getConnectTimeout() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.incomingRequestParams.connectParams.ConnectTimeout <= 0 {
		return cConnectTimeoutDefault
	}

	return c.incomingRequestParams.connectParams.ConnectTimeout
}

func (c *connectionManager) setToken(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.token = token
}

func (c *connectionManager) setConnected(region *livekit.RegionInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Closed is terminal; never transition out of it
	if c.state == connectionManagerStateClosed {
		return
	}

	// reset on connection establishment to ensure region settings in leave request from a
	// previously connected server is not used past its validity,
	//
	// if a resume/reconnect is needed after this, region settings from the newly connected
	// server will be used if the new server provides one in the leave request
	c.regionSettings = nil
	c.connectedRegion = utils.CloneProto(region)
	c.updateState(connectionManagerStateConnected)
}

// setResumed restores the Connected state after a successful resume so the next
// resume starts fresh. It is a no-op unless still Resuming: if a reconnect was
// requested while the resume was in progress, the state is left Reconnecting so
// the pending full reconnect proceeds rather than being clobbered.
func (c *connectionManager) setResumed(region *livekit.RegionInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Closed is terminal; never transition out of it
	if c.state == connectionManagerStateClosed {
		return
	}

	if c.state != connectionManagerStateResuming {
		return
	}

	c.regionSettings = nil
	c.connectedRegion = utils.CloneProto(region)
	c.updateState(connectionManagerStateConnected)
}

func (c *connectionManager) setResuming(regionSettings *livekit.RegionSettings) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.setResumingLocked(regionSettings)
}

func (c *connectionManager) setResumingLocked(regionSettings *livekit.RegionSettings) {
	// Closed is terminal; never transition out of it
	if c.state == connectionManagerStateClosed {
		return
	}

	// if already reconnecting, resuming is a no-op till the reconnect finishes
	if c.state == connectionManagerStateReconnecting {
		return
	}

	// if already resuming, do not take settings that are nil as some internal paths might do a resume without regions
	if c.state == connectionManagerStateResuming {
		if regionSettings != nil {
			c.regionSettings = utils.CloneProto(regionSettings)
		}
		return
	}

	// if not connected, cannot resume, so no-op
	if c.state != connectionManagerStateConnected {
		return
	}

	c.regionSettings = utils.CloneProto(regionSettings)
	c.updateState(connectionManagerStateResuming)
}

func (c *connectionManager) setReconnecting(regionSettings *livekit.RegionSettings) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.setReconnectingLocked(regionSettings)
}

func (c *connectionManager) setReconnectingLocked(regionSettings *livekit.RegionSettings) {
	// Closed is terminal; never transition out of it
	if c.state == connectionManagerStateClosed {
		return
	}

	// during initial connection, a reconnection cannot trigger the reconnect loop
	// to prevent reconnect and initial join running in parallel.
	//
	// POSSIBLE IDEA: record regions if reconnect provided one and reload initial plan and execute,
	// that would need `regionSettings` to be included in the initial plan. One implementation would be something like
	//   - initial plan loaded
	//   - execute plan
	//   - a reconnect with regions happens while executing the plan, regions gets recorded
	//   - if the first plan fails, reload plan with reconnect regions included
	//   - re-execute the new plan
	//   - limit to one reload potentially to keep the initial attempt bounded
	if c.state == connectionManagerStateInitial {
		return
	}

	// reconnecting can trigger internally when a resume fails and that would not have regions,
	// so don't clobber regions list if one was received via leave reconnect
	if c.state == connectionManagerStateReconnecting && regionSettings == nil {
		return
	}
	c.regionSettings = utils.CloneProto(regionSettings)
	c.updateState(connectionManagerStateReconnecting)
}

func (c *connectionManager) isReconnectingState() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.state == connectionManagerStateReconnecting
}

// needsRecoveryLocked reports whether the current state requires a recovery
// worker (i.e. a resume or full reconnect is pending). Caller must hold mu.
func (c *connectionManager) needsRecoveryLocked() bool {
	return c.state == connectionManagerStateResuming || c.state == connectionManagerStateReconnecting
}

// requestRecovery applies the requested recovery transition (resume or full
// reconnect) and reports whether the caller must start a recovery worker. A
// worker is needed only when the resulting state requires recovery and none is
// already running; when one is already running it will act on the updated state.
// The transition and the worker-slot claim are performed under a single lock.
func (c *connectionManager) requestRecovery(fullReconnect bool, regionSettings *livekit.RegionSettings) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if fullReconnect {
		c.setReconnectingLocked(regionSettings)
	} else {
		c.setResumingLocked(regionSettings)
	}

	if !c.needsRecoveryLocked() || c.recovering {
		return false
	}
	c.recovering = true
	return true
}

// recoveryWorkerShouldContinue is called by the recovery worker once it has
// settled the connection (state back to Connected). If the state still requires
// recovery it returns true so the worker keeps running; otherwise it releases the
// worker slot and returns false so the worker exits. The state check and the slot
// release are performed under a single lock.
func (c *connectionManager) recoveryWorkerShouldContinue() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.needsRecoveryLocked() {
		return true
	}
	c.recovering = false
	return false
}

// stopRecoveryWorker releases the worker slot unconditionally. Used when the
// worker gives up; a later disconnect can start a new worker via requestRecovery.
func (c *connectionManager) stopRecoveryWorker() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.recovering = false
}

// isRecovering reports whether a recovery worker is currently running.
func (c *connectionManager) isRecovering() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.recovering
}

func (c *connectionManager) setClosed() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.regionSettings = nil
	c.updateState(connectionManagerStateClosed)
}

func (c *connectionManager) updateState(state connectionManagerState) {
	if c.state == state {
		return
	}

	c.log.Infow(
		"connection manager state change",
		"old", c.state,
		"new", state,
		"regionSettings", protoLogger.Proto(c.regionSettings),
		"connectedRegion", protoLogger.Proto(c.connectedRegion),
	)
	c.state = state
}

func (c *connectionManager) currentState() connectionManagerState {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.state
}

func (c *connectionManager) getConnectionPlan() ([]connectionAttemptParams, error) {
	c.mu.RLock()
	if c.incomingRequestParams.url == "" {
		c.mu.RUnlock()
		return nil, errors.New("original url not set")
	}

	state := c.state
	params := connectionPlanParams{
		incomingRequestParams: c.incomingRequestParams,
		regionSettings:        c.regionSettings,
		connectedRegion:       c.connectedRegion,
		token:                 c.token,
		regionURLProvider:     c.regionProvider,
		log:                   c.log,
	}
	c.mu.RUnlock()

	var planner func(params connectionPlanParams) ([]connectionAttemptParams, error)

	switch state {
	case connectionManagerStateInitial:
		planner = getConnectionPlanInitial
	case connectionManagerStateResuming:
		planner = getConnectionPlanResuming
	case connectionManagerStateReconnecting:
		planner = getConnectionPlanReconnecting
	}

	if planner == nil {
		return nil, errors.New("invalid state")
	}

	return planner(params)
}

// -------------------------------------------------------------

type connectionPlanParams struct {
	incomingRequestParams connectionRequestParams
	regionSettings        *livekit.RegionSettings
	connectedRegion       *livekit.RegionInfo
	token                 string

	regionURLProvider *regionURLProvider
	log               protoLogger.Logger
}

func getConnectionPlanInitial(params connectionPlanParams) ([]connectionAttemptParams, error) {
	var regionsToTry []*livekit.RegionInfo
	if !params.incomingRequestParams.disableRegionDiscovery {
		cloudHostname, _ := parseCloudURL(params.incomingRequestParams.url)
		if cloudHostname != "" {
			settings, err := params.regionURLProvider.RegionSettings(cloudHostname, params.token)
			if err == nil {
				regionsToTry = append(regionsToTry, settings.GetRegions()...)
			}
		}
	}

	// add the incoming request URL (i. e. original URL) just in case the region specific options did not work
	regionsToTry = append(regionsToTry, &livekit.RegionInfo{
		Region:   cOriginalRegion,
		Url:      params.incomingRequestParams.url,
		Distance: -1,
	})

	return buildConnectionPlan(params.incomingRequestParams.ctx, regionsToTry, params.token)
}

func getConnectionPlanResuming(params connectionPlanParams) ([]connectionAttemptParams, error) {
	var regionsToTry []*livekit.RegionInfo
	if params.regionSettings != nil {
		// server sent list if available, the first entry should match the connected region
		if params.connectedRegion != nil {
			regions := params.regionSettings.GetRegions()
			if len(regions) > 0 && regions[0].Url != params.connectedRegion.Url {
				params.log.Infow(
					"first region in settings does not match connected region for resume",
					"firstRegion", protoLogger.Proto(regions[0]),
					"connectedRegion", protoLogger.Proto(params.connectedRegion),
				)
			}
		}

		// server sent list via LeaveRequest, try those
		regionsToTry = append(regionsToTry, params.regionSettings.GetRegions()...)
	} else {
		// no server sent list, try the connected url again
		if params.connectedRegion != nil {
			regionsToTry = append(regionsToTry, params.connectedRegion)
		}
	}

	// add the incoming request URL (i. e. original URL) just in case the region specific options did not work
	regionsToTry = append(regionsToTry, &livekit.RegionInfo{
		Region:   cOriginalRegion,
		Url:      params.incomingRequestParams.url,
		Distance: -1,
	})

	return buildConnectionPlan(context.Background(), regionsToTry, params.token)
}

func getConnectionPlanReconnecting(params connectionPlanParams) ([]connectionAttemptParams, error) {
	var regionsToTry []*livekit.RegionInfo
	if params.regionSettings != nil {
		// server sent list via LeaveRequest, try those
		regionsToTry = append(regionsToTry, params.regionSettings.GetRegions()...)
	} else {
		// no server sent list, try the connected url again
		if params.connectedRegion != nil {
			regionsToTry = append(regionsToTry, params.connectedRegion)
		}
	}

	// layer on region provider regions if enabled
	if !params.incomingRequestParams.disableRegionDiscovery {
		cloudHostname, _ := parseCloudURL(params.incomingRequestParams.url)
		if cloudHostname != "" {
			settings, err := params.regionURLProvider.RegionSettings(cloudHostname, params.token)
			if err == nil {
				regionsToTry = append(regionsToTry, settings.GetRegions()...)
			}
		}
	}

	// add the incoming request URL (i. e. original URL) just in case the region specific options did not work
	regionsToTry = append(regionsToTry, &livekit.RegionInfo{
		Region:   cOriginalRegion,
		Url:      params.incomingRequestParams.url,
		Distance: -1,
	})

	return buildConnectionPlan(context.Background(), regionsToTry, params.token)
}

func buildConnectionPlan(ctx context.Context, regionsToTry []*livekit.RegionInfo, token string) ([]connectionAttemptParams, error) {
	seen := make(map[string]bool, len(regionsToTry))
	dedupedRegions := make([]*livekit.RegionInfo, 0, len(regionsToTry))
	for _, region := range regionsToTry {
		if !seen[region.Region] {
			seen[region.Region] = true
			dedupedRegions = append(dedupedRegions, region)
		}
	}

	var plan []connectionAttemptParams
	for idx, region := range dedupedRegions {
		backoffWait := time.Duration(0)
		if idx != 0 {
			backoffWait = time.Duration(1<<min(idx-1, 6)) * 100 * time.Millisecond // max 6.4 seconds
		}

		plan = append(plan, connectionAttemptParams{
			ctx:             ctx,
			backoffWait:     backoffWait,
			region:          region,
			token:           token,
			validateTimeout: cValidateTimeout,
		})
	}
	return plan, nil
}
