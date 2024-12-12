# Copyright (c) 2024 Hemi Labs, Inc.
# Use of this source code is governed by the MIT License,
# which can be found in the LICENSE file.

FROM golang:1.23.4-bookworm@sha256:ef30001eeadd12890c7737c26f3be5b3a8479ccdcdc553b999c84879875a27ce AS build_1

WORKDIR /git

ARG OP_GETH_CACHE_BREAK=12F
RUN git clone https://github.com/hemilabs/op-geth
WORKDIR /git/op-geth
RUN git checkout ebbdfc3

WORKDIR /git/op-geth

RUN make

RUN go build -o /tmp ./...

FROM golang:1.22.6-bookworm@sha256:f020456572fc292e9627b3fb435c6de5dfb8020fbcef1fd7b65dd092c0ac56bb

COPY --from=build_1 /tmp/geth /bin/geth

RUN apt-get update

RUN apt-get install -y jq nodejs npm

WORKDIR /git

RUN curl -L https://foundry.paradigm.xyz | bash

RUN . /root/.bashrc

ENV PATH="${PATH}:/root/.foundry/bin"

RUN foundryup

ARG OPTIMISM_CACHE_BREAK=1
RUN git clone https://github.com/hemilabs/optimism
WORKDIR /git/optimism
RUN git checkout 775b5a0

RUN git submodule update --init --recursive
# RUN pnpm install:abigen

WORKDIR /git/optimism/packages/contracts-bedrock
RUN sed -e '/build_info/d' -i ./foundry.toml
WORKDIR /git/optimism
RUN go mod tidy
WORKDIR /git/optimism/op-bindings
RUN go mod tidy
WORKDIR /git/optimism
RUN npm install -g pnpm
RUN pnpm install
RUN pnpm install:abigen
RUN make op-bindings op-node op-batcher op-proposer
RUN make -C ./op-conductor op-conductor

RUN pnpm build

WORKDIR /git/optimism/packages/contracts-bedrock
RUN forge install
RUN forge build

WORKDIR /git/optimism

RUN make devnet-allocs
