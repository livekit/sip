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

package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestInitRegistrations(t *testing.T) {
	newConf := func(regs ...SIPRegistrationConfig) *Config {
		return &Config{SIPRegistrations: regs}
	}

	t.Run("defaults", func(t *testing.T) {
		c := newConf(SIPRegistrationConfig{Registrar: "sip.example.com", Username: "alice"})
		require.NoError(t, c.Init())
		require.Equal(t, DefaultSIPRegistrationExpiry, c.SIPRegistrations[0].Expiry)
		require.Equal(t, DefaultSIPRegistrationKeepalive, c.SIPRegistrations[0].Keepalive)
	})
	t.Run("keepalive disabled", func(t *testing.T) {
		c := newConf(SIPRegistrationConfig{Registrar: "sip.example.com", Username: "alice", Keepalive: -1})
		require.NoError(t, c.Init())
		require.Zero(t, c.SIPRegistrations[0].Keepalive)
	})
	t.Run("registrar required", func(t *testing.T) {
		require.Error(t, newConf(SIPRegistrationConfig{Username: "alice"}).Init())
	})
	t.Run("username required", func(t *testing.T) {
		require.Error(t, newConf(SIPRegistrationConfig{Registrar: "sip.example.com"}).Init())
	})
	t.Run("expiry out of range", func(t *testing.T) {
		require.Error(t, newConf(SIPRegistrationConfig{
			Registrar: "sip.example.com", Username: "alice", Expiry: time.Second,
		}).Init())
		require.Error(t, newConf(SIPRegistrationConfig{
			Registrar: "sip.example.com", Username: "alice", Expiry: 48 * time.Hour,
		}).Init())
	})
	t.Run("keepalive too short", func(t *testing.T) {
		require.Error(t, newConf(SIPRegistrationConfig{
			Registrar: "sip.example.com", Username: "alice", Keepalive: time.Second,
		}).Init())
	})
}
