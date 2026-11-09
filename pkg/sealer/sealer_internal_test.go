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

package sealer

import (
	"errors"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/require"
)

func TestAESGCMSealRandFailure(t *testing.T) {
	s, err := NewAESGCM([]string{"k"})
	require.NoError(t, err)
	errRand := errors.New("no entropy")
	s.rand = iotest.ErrReader(errRand)
	out, err := s.Seal([]byte("dst"), []byte("p"), nil)
	require.ErrorIs(t, err, errRand)
	require.Nil(t, out)
}
