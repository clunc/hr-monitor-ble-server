// Package wire handles Confluent schema-registry framing.
//
// The lakehouse path needs it: Kafka Connect's ProtobufConverter reads the
// schema id out of the frame to resolve the message type, and refuses bare
// protobuf. Same helper as ble-tape-gateway's internal/measurepb.
package wire

import (
	"encoding/binary"
	"fmt"
)

// Wrap prepends Confluent wire-format framing to a raw protobuf payload:
//
//	[0x00] [schema_id: 4 bytes BE] [0x00 message_index] [payload]
//
// message_index 0x00 selects the first (and only) message in the .proto.
func Wrap(schemaID int32, payload []byte) []byte {
	buf := make([]byte, 6+len(payload))
	buf[0] = 0x00
	binary.BigEndian.PutUint32(buf[1:5], uint32(schemaID))
	buf[5] = 0x00
	copy(buf[6:], payload)
	return buf
}

// Unwrap strips the framing, passing through messages that don't carry it so a
// consumer can read both old and new records during a rollout.
func Unwrap(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty message")
	}
	if data[0] != 0x00 {
		return data, nil
	}
	if len(data) < 6 {
		return nil, fmt.Errorf("confluent wire message too short (%d bytes)", len(data))
	}
	return data[6:], nil
}
