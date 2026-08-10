package protocol

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFrameRoundTrip(t *testing.T) {
	want := Frame{Kind: KindSnapshotUser, Revision: 42, Payload: []byte("payload")}
	got, err := Unmarshal(Marshal(want))
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestFrameRejectsTruncatedPayload(t *testing.T) {
	raw := Marshal(Frame{Kind: KindEvent, Revision: 7, Payload: []byte("payload")})
	_, err := Unmarshal(raw[:len(raw)-1])
	require.Error(t, err)
}
