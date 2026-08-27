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

package types

import "errors"

const (
	SDKSalt         = "LKFrameEncryptionKey"
	IVLength        = 12
	PBKDFIterations = 100000
	KeySizeBytes    = 16
	HKDFInfoBytes   = 128
)

var (
	ErrIncorrectKeyLength  = errors.New("incorrect key length for encryption/decryption")
	ErrUnableGenerateIV    = errors.New("unable to generate iv for encryption")
	ErrIncorrectIVLength   = errors.New("incorrect iv length")
	ErrBlockCipherRequired = errors.New("input block cipher cannot be nil")
)
