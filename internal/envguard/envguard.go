// Package envguard refuses process environments that would write message data
// to logs.
package envguard

import "errors"

// ErrGRPCBinaryLog is returned when gRPC binary logging is configured.
var ErrGRPCBinaryLog = errors.New("GRPC_BINARY_LOG_FILTER must not be set: binary logs record recipients and message bodies")

// RefuseGRPCBinaryLog fails when GRPC_BINARY_LOG_FILTER is set to any value,
// because grpc-go would then log DeliveryStart recipients and MIME payloads.
func RefuseGRPCBinaryLog(getenv func(string) string) error {
	if getenv("GRPC_BINARY_LOG_FILTER") != "" {
		return ErrGRPCBinaryLog
	}
	return nil
}
