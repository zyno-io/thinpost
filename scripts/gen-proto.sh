#!/bin/sh
# Regenerates the control protocol Go code. Pinned to the generator versions
# recorded in the generated file headers; CI fails if the output drifts.
#   protoc (libprotoc) 36.2, protoc-gen-go v1.36.12, protoc-gen-go-grpc 1.6.2
set -eu
cd "$(dirname "$0")/.."
protoc --go_out=. --go_opt=paths=source_relative \
  --go-grpc_out=. --go-grpc_opt=paths=source_relative \
  api/control/v1/control.proto
