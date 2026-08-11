// Package protocol contains the small, versioned binary framing used inside
// the protobuf gRPC envelope. Keeping the frame encoding explicit prevents a
// large JSON document from becoming the replication transport by accident.
package protocol

import (
	"encoding/binary"
	"fmt"
)

const Version byte = 1

type Kind byte

const (
	KindHello Kind = iota + 1
	KindEvent
	KindSnapshotBegin
	KindSnapshotUser
	KindSnapshotArtifact
	KindSnapshotEnd
	KindAck
	KindStatus
	KindError
	KindFraudEvents
	KindFraudAck
	KindStats
)

type Frame struct {
	Kind     Kind
	Revision int64
	Payload  []byte
}

func Marshal(frame Frame) []byte {
	buf := make([]byte, 0, 2+binary.MaxVarintLen64*2+len(frame.Payload))
	buf = append(buf, Version, byte(frame.Kind))
	buf = binary.AppendVarint(buf, frame.Revision)
	buf = binary.AppendUvarint(buf, uint64(len(frame.Payload)))
	buf = append(buf, frame.Payload...)
	return buf
}

func Unmarshal(raw []byte) (Frame, error) {
	if len(raw) < 2 {
		return Frame{}, fmt.Errorf("replication frame is too short")
	}
	if raw[0] != Version {
		return Frame{}, fmt.Errorf("unsupported replication frame version %d", raw[0])
	}
	revision, n := binary.Varint(raw[2:])
	if n <= 0 {
		return Frame{}, fmt.Errorf("invalid replication frame revision")
	}
	offset := 2 + n
	length, n := binary.Uvarint(raw[offset:])
	if n <= 0 {
		return Frame{}, fmt.Errorf("invalid replication frame payload length")
	}
	offset += n
	if length > uint64(len(raw)-offset) {
		return Frame{}, fmt.Errorf("truncated replication frame payload")
	}
	if length != uint64(len(raw)-offset) {
		return Frame{}, fmt.Errorf("unexpected trailing bytes in replication frame")
	}
	return Frame{Kind: Kind(raw[1]), Revision: revision, Payload: append([]byte(nil), raw[offset:]...)}, nil
}
