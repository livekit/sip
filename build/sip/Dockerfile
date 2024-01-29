# Copyright 2023 LiveKit, Inc.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

ARG GOVERSION

FROM golang:$GOVERSION

ARG TARGETPLATFORM

WORKDIR /workspace

# install libopus
RUN apt-get update && apt-get install -y pkg-config libopus-dev libopusfile-dev

# download go modules
COPY go.mod .
COPY go.sum .
RUN go mod download

# copy source
COPY res/ res/
COPY cmd/ cmd/
COPY pkg/ pkg/
COPY version/ version/

# build
RUN if [ "$TARGETPLATFORM" = "linux/arm64" ]; then GOARCH=arm64; else GOARCH=amd64; fi && \
    CGO_ENABLED=1 GOOS=linux GOARCH=${GOARCH} GO111MODULE=on go build -a -o livekit-sip ./cmd/livekit-sip

FROM golang:$GOVERSION

# install wget for health check
RUN apt-get update && \
    apt-get install -y libopus0 libopusfile0 && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

# copy binary
COPY --from=0 /workspace/livekit-sip /bin/

WORKDIR /sip

# run
ENTRYPOINT ["livekit-sip", "--config=/sip/config.yaml"]
