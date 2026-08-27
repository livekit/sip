// Copyright 2023 LiveKit, Inc.
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
	"strconv"
	"strings"
)

func byteLength(str string) int {
	return len([]byte(str))
}

func truncateBytes(str string, maxBytes int) string {
	if byteLength(str) <= maxBytes {
		return str
	} else {
		byteStr := []byte(str)
		return string(byteStr[:maxBytes])
	}
}

func compareVersions(v1, v2 string) int {
	parts1 := strings.Split(v1, ".")
	parts2 := strings.Split(v2, ".")

	k := min(len(parts1), len(parts2))

	for i := range k {
		p1, _ := strconv.Atoi(parts1[i])
		p2, _ := strconv.Atoi(parts2[i])

		if p1 < p2 {
			return -1
		} else if p1 > p2 {
			return 1
		} else if (i == k-1) && (p1 == p2) {
			return 0
		}
	}

	if len(parts1) < len(parts2) {
		return -1
	} else if len(parts1) > len(parts2) {
		return 1
	} else {
		return 0
	}
}
