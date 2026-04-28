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

package stats

type TerminationResult string

const (
	ResultSuccess     TerminationResult = "success"
	ResultServerError TerminationResult = "server_error"
	ResultClientError TerminationResult = "client_error"
)

type Termination struct {
	Result TerminationResult
	Reason string
}

func Success(reason string) Termination { return Termination{Result: ResultSuccess, Reason: reason} }
func ServerError(reason string) Termination {
	return Termination{Result: ResultServerError, Reason: reason}
}
func ClientError(reason string) Termination {
	return Termination{Result: ResultClientError, Reason: reason}
}
