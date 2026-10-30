// Copyright 2026 LiveKit, Inc.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy at http://www.apache.org/licenses/LICENSE-2.0

package sip

import "github.com/livekit/protocol/logger"

// Invite response headers can enrich the logger while BYE and room callbacks run.
// Keep this lock independent of the call state lock used by those callbacks.
func (c *outboundCall) getLogger() logger.Logger {
	c.logMu.RLock()
	defer c.logMu.RUnlock()
	return c.log
}

func (c *outboundCall) setLogger(log logger.Logger) {
	c.logMu.Lock()
	defer c.logMu.Unlock()
	c.log = log
}
