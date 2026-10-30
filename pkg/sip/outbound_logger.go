// Copyright 2026 LiveKit, Inc.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy at http://www.apache.org/licenses/LICENSE-2.0

package sip

import "github.com/livekit/protocol/logger"

func (c *outboundCall) setLog(log logger.Logger) {
	c.logPtr.Store(&log)
}

func (c *outboundCall) log() logger.Logger {
	ptr := c.logPtr.Load()
	if ptr == nil {
		return nil
	}
	return *ptr
}
