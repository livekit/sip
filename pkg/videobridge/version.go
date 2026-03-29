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

package videobridge

const (
	// APIVersion is the current API version of the SIP Video Bridge.
	// Increment Major for breaking changes, Minor for additive, Patch for fixes.
	//
	// Compatibility contract:
	//   - Config YAML: backward compatible within same Major version.
	//     New fields get defaults; removed fields are ignored with a warning.
	//   - Health/Sessions API: backward compatible within same Major version.
	//     New fields may be added; existing fields are never removed or renamed.
	//   - SDP negotiation: follows RFC standards; codec support only grows.
	//   - Redis session records: versioned via "v" field; old records are ignored
	//     if version is incompatible.
	//   - Feature flags API: additive only; new flags default to enabled.
	//   - Prometheus metrics: metric names never change within same Major version.
	//     New metrics may be added.
	APIVersion = "0.2.0"

	// APIVersionMajor is the major version component.
	APIVersionMajor = 0
	// APIVersionMinor is the minor version component.
	APIVersionMinor = 2
	// APIVersionPatch is the patch version component.
	APIVersionPatch = 0
)

// VersionInfo holds build and version metadata.
type VersionInfo struct {
	Version  string `json:"version"`
	API      string `json:"api_version"`
	GoModule string `json:"go_module"`
}

// GetVersion returns the current version info.
func GetVersion() VersionInfo {
	return VersionInfo{
		Version:  APIVersion,
		API:      APIVersion,
		GoModule: "github.com/livekit/sip/pkg/videobridge",
	}
}
