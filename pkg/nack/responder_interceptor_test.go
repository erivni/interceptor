package nack

import (
	"os"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/internal/test"
	"github.com/pion/logging"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
)

func TestResponderInterceptor(t *testing.T) {
	f, err := NewResponderInterceptor(
		ResponderSize(8),
		ResponderLog(logging.NewDefaultLoggerFactory().NewLogger("test")),
	)
	assert.NoError(t, err)

	i, err := f.NewInterceptor("")
	assert.NoError(t, err)

	stream := test.NewMockStream(&interceptor.StreamInfo{
		SSRC:         1,
		RTCPFeedback: []interceptor.RTCPFeedback{{Type: "nack"}},
	}, i)
	defer func() {
		assert.NoError(t, stream.Close())
	}()

	for _, seqNum := range []uint16{10, 11, 12, 14, 15} {
		assert.NoError(t, stream.WriteRTP(&rtp.Packet{Header: rtp.Header{SequenceNumber: seqNum}}))

		select {
		case p := <-stream.WrittenRTP():
			assert.Equal(t, seqNum, p.SequenceNumber)
		case <-time.After(10 * time.Millisecond):
			t.Fatal("written rtp packet not found")
		}
	}

	stream.ReceiveRTCP([]rtcp.Packet{
		&rtcp.TransportLayerNack{
			MediaSSRC:  1,
			SenderSSRC: 2,
			Nacks: []rtcp.NackPair{
				{PacketID: 11, LostPackets: 0b1011}, // sequence numbers: 11, 12, 13, 15
			},
		},
	})

	// seq number 13 was never sent, so it can't be resent
	for _, seqNum := range []uint16{11, 12, 15} {
		select {
		case p := <-stream.WrittenRTP():
			assert.Equal(t, seqNum, p.SequenceNumber)
		case <-time.After(10 * time.Millisecond):
			t.Fatal("written rtp packet not found")
		}
	}

	select {
	case p := <-stream.WrittenRTP():
		t.Errorf("no more rtp packets expected, found sequence number: %v", p.SequenceNumber)
	case <-time.After(10 * time.Millisecond):
	}
}

func TestResponderInterceptorNacksSpreadEnabled(t *testing.T) {
	f, err := NewResponderInterceptor(
		ResponderSize(64),
		ResponderLog(logging.NewDefaultLoggerFactory().NewLogger("test")),
	)
	assert.NoError(t, err)

	i, err := f.NewInterceptor("")
	assert.NoError(t, err)

	stream := test.NewMockStream(&interceptor.StreamInfo{
		SSRC:         1,
		RTCPFeedback: []interceptor.RTCPFeedback{{Type: "nack"}},
	}, i)
	defer func() {
		assert.NoError(t, stream.Close())
	}()

	manyPackets := []uint16{
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20,
		21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40,
		41, 42, 43, 44, 45, 46, 47, 48, 49, 50, 51, 52, 53, 54, 55, 56, 57, 58, 59, 60,
		61, 62, 63, 64}
	for _, seqNum := range manyPackets {
		assert.NoError(t, stream.WriteRTP(&rtp.Packet{Header: rtp.Header{SequenceNumber: seqNum}}))

		select {
		case p := <-stream.WrittenRTP():
			assert.Equal(t, seqNum, p.SequenceNumber)
		case <-time.After(10 * time.Millisecond):
			t.Fatal("written rtp packet not found")
		}
	}

	os.Setenv("HYPERSCALE_NACKS_MAX_PACKET_BURST", "10")
	defer os.Unsetenv("HYPERSCALE_NACKS_MAX_PACKET_BURST")
	os.Setenv("HYPERSCALE_NACKS_SPREAD_PACKET_DELAY_MS", "1000")
	defer os.Unsetenv("HYPERSCALE_NACKS_SPREAD_PACKET_DELAY_MS")

	nackWriteTimePhasesSecsCount := []int{0, 0, 0, 0, 0, 0, 0} // Stores 7s count of recieved nacks in 1s buckets
	timeNacksStart := time.Now()
	// Send nack report for 64 missing packets. with max burst of 10 packets and 1s delay, we expect all 64 nacks to be send after +6s
	stream.ReceiveRTCP([]rtcp.Packet{
		&rtcp.TransportLayerNack{
			MediaSSRC:  1,
			SenderSSRC: 2,
			Nacks: []rtcp.NackPair{
				{PacketID: 1, LostPackets: 0xFFFF},  // packets missing: 1-17
				{PacketID: 18, LostPackets: 0xFFFF}, // packets missing: 18-34
				{PacketID: 35, LostPackets: 0xFFFF}, // packets missing: 35-51
				{PacketID: 52, LostPackets: 0xFFFF}, // packets missing: 52-64
			},
		},
	})

	for _, seqNum := range manyPackets {
		select {
		case p := <-stream.WrittenRTP():
			assert.Equal(t, seqNum, p.SequenceNumber)
			secsWindow := int(time.Since(timeNacksStart).Seconds())
			nackWriteTimePhasesSecsCount[secsWindow]++
		case <-time.After(10000 * time.Millisecond):
			t.Fatal("Not all nack'ed packets were written after 7s")
		}
	}

	select {
	case p := <-stream.WrittenRTP():
		t.Errorf("no more rtp packets expected, found sequence number: %v", p.SequenceNumber)
	case <-time.After(1000 * time.Millisecond): // Wait +1s to ensure we don't get any more nack'ed packets
	}
	assert.Equal(t, 10, nackWriteTimePhasesSecsCount[0]) // Expect 10 nacks were sent in 0s-1s
	assert.Equal(t, 10, nackWriteTimePhasesSecsCount[1]) // Expect 10 nacks were sent in 1s-2s
	assert.Equal(t, 10, nackWriteTimePhasesSecsCount[2]) // Expect 10 nacks were sent in 2s-3s
	assert.Equal(t, 10, nackWriteTimePhasesSecsCount[3]) // Expect 10 nacks were sent in 3s-4s
	assert.Equal(t, 10, nackWriteTimePhasesSecsCount[4]) // Expect 10 nacks were sent in 4s-5s
	assert.Equal(t, 10, nackWriteTimePhasesSecsCount[5]) // Expect 10 nacks were sent in 5s-6s
	assert.Equal(t, 4, nackWriteTimePhasesSecsCount[6])  // Expect 4 nacks were sent in 6s-7s
}

func TestResponderInterceptorNacksSpreadDisabled(t *testing.T) {
	f, err := NewResponderInterceptor(
		ResponderSize(64),
		ResponderLog(logging.NewDefaultLoggerFactory().NewLogger("test")),
	)
	assert.NoError(t, err)

	i, err := f.NewInterceptor("")
	assert.NoError(t, err)

	stream := test.NewMockStream(&interceptor.StreamInfo{
		SSRC:         1,
		RTCPFeedback: []interceptor.RTCPFeedback{{Type: "nack"}},
	}, i)
	defer func() {
		assert.NoError(t, stream.Close())
	}()

	manyPackets := []uint16{
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20,
		21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40,
		41, 42, 43, 44, 45, 46, 47, 48, 49, 50, 51, 52, 53, 54, 55, 56, 57, 58, 59, 60,
		61, 62, 63, 64}
	for _, seqNum := range manyPackets {
		assert.NoError(t, stream.WriteRTP(&rtp.Packet{Header: rtp.Header{SequenceNumber: seqNum}}))

		select {
		case p := <-stream.WrittenRTP():
			assert.Equal(t, seqNum, p.SequenceNumber)
		case <-time.After(10 * time.Millisecond):
			t.Fatal("written rtp packet not found")
		}
	}

	os.Setenv("HYPERSCALE_NACKS_MAX_PACKET_BURST", "0")
	defer os.Unsetenv("HYPERSCALE_NACKS_MAX_PACKET_BURST")
	os.Setenv("HYPERSCALE_NACKS_SPREAD_PACKET_DELAY_MS", "0")
	defer os.Unsetenv("HYPERSCALE_NACKS_SPREAD_PACKET_DELAY_MS")

	// Send nack report for 64 missing packets
	stream.ReceiveRTCP([]rtcp.Packet{
		&rtcp.TransportLayerNack{
			MediaSSRC:  1,
			SenderSSRC: 2,
			Nacks: []rtcp.NackPair{
				{PacketID: 1, LostPackets: 0xFFFF},  // packets missing: 1-17
				{PacketID: 18, LostPackets: 0xFFFF}, // packets missing: 18-34
				{PacketID: 35, LostPackets: 0xFFFF}, // packets missing: 35-51
				{PacketID: 52, LostPackets: 0xFFFF}, // packets missing: 52-64
			},
		},
	})
	// with nack spreads disabled we can expect all of 64 nack'ed packets to arrive in less than 200ms
	for _, seqNum := range manyPackets {
		select {
		case p := <-stream.WrittenRTP():
			assert.Equal(t, seqNum, p.SequenceNumber)
		case <-time.After(200 * time.Millisecond):
			t.Fatal("Not all nack'ed packets were written after 200ms")
		}
	}

	select {
	case p := <-stream.WrittenRTP():
		t.Errorf("no more rtp packets expected, found sequence number: %v", p.SequenceNumber)
	case <-time.After(1000 * time.Millisecond): // Wait +1s to ensure we don't get any more nack'ed packets
	}
}

func TestResponderInterceptor_InvalidSize(t *testing.T) {
	f, _ := NewResponderInterceptor(ResponderSize(5))

	_, err := f.NewInterceptor("")
	assert.Error(t, err, ErrInvalidSize)
}

// this test is only useful when being run with the race detector, it won't fail otherwise:
//
//     go test -race ./pkg/nack/
func TestResponderInterceptor_Race(t *testing.T) {
	f, err := NewResponderInterceptor(
		ResponderSize(32768),
		ResponderLog(logging.NewDefaultLoggerFactory().NewLogger("test")),
	)
	assert.NoError(t, err)

	i, err := f.NewInterceptor("")
	assert.NoError(t, err)

	stream := test.NewMockStream(&interceptor.StreamInfo{
		SSRC:         1,
		RTCPFeedback: []interceptor.RTCPFeedback{{Type: "nack"}},
	}, i)

	for seqNum := uint16(0); seqNum < 500; seqNum++ {
		assert.NoError(t, stream.WriteRTP(&rtp.Packet{Header: rtp.Header{SequenceNumber: seqNum}}))

		// 25% packet loss
		if seqNum%4 == 0 {
			time.Sleep(time.Duration(seqNum%23) * time.Millisecond)
			stream.ReceiveRTCP([]rtcp.Packet{
				&rtcp.TransportLayerNack{
					MediaSSRC:  1,
					SenderSSRC: 2,
					Nacks: []rtcp.NackPair{
						{PacketID: seqNum, LostPackets: 0},
					},
				},
			})
		}
	}
}
