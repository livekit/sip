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

package media

import (
	"slices"
	"strings"
)

type CodecInfo struct {
	SDPName     string
	RTPDefType  byte
	RTPIsStatic bool
	Priority    int
	Disabled    bool
}

type Codec interface {
	Info() CodecInfo
}

var (
	disabled        = make(map[string]struct{})
	codecs          []Codec
	codecOnRegister []func(c Codec)
)

func CodecSetEnabled(name string, enabled bool) {
	name = strings.ToLower(name)
	if enabled {
		delete(disabled, name)
	} else {
		disabled[name] = struct{}{}
	}
}

func CodecsSetEnabled(codecs map[string]bool) {
	disabled = make(map[string]struct{})
	for name, enabled := range codecs {
		CodecSetEnabled(name, enabled)
	}
}

func CodecEnabled(c Codec) bool {
	if c == nil {
		return false
	}
	name := strings.ToLower(c.Info().SDPName)
	_, dis := disabled[name]
	return !dis
}

func OnRegister(fnc func(c Codec)) {
	for _, c := range codecs {
		fnc(c)
	}
	codecOnRegister = append(codecOnRegister, fnc)
}

func Codecs() []Codec {
	return slices.Clone(codecs)
}

func EnabledCodecs() []Codec {
	out := make([]Codec, 0, len(codecs))
	for _, c := range codecs {
		name := strings.ToLower(c.Info().SDPName)
		if _, ok := disabled[name]; ok {
			continue
		}
		out = append(out, c)
	}
	return out
}

func RegisterCodec(c Codec) {
	codecs = append(codecs, c)
	if info := c.Info(); info.Disabled {
		CodecSetEnabled(info.SDPName, false)
	}
	for _, fnc := range codecOnRegister {
		fnc(c)
	}
}

func NewCodec(info CodecInfo) Codec {
	return &baseCodec{info: info}
}

type baseCodec struct {
	info CodecInfo
}

func (c *baseCodec) Info() CodecInfo {
	return c.info
}
