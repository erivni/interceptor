package nack

import (
	"fmt"
	"os"
	"strconv"
	"sync"

	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	log "github.com/sirupsen/logrus"
)

// ResponderInterceptorFactory is a interceptor.Factory for a ResponderInterceptor
type ResponderInterceptorFactory struct {
	opts []ResponderOption
}

// NewInterceptor constructs a new ResponderInterceptor
func (r *ResponderInterceptorFactory) NewInterceptor(id string) (interceptor.Interceptor, error) {
	i := &ResponderInterceptor{
		size:    8192,
		log:     logging.NewDefaultLoggerFactory().NewLogger("nack_responder"),
		streams: map[uint32]*localStream{},
	}

	for _, opt := range r.opts {
		if err := opt(i); err != nil {
			return nil, err
		}
	}

	if _, err := newSendBuffer(i.size); err != nil {
		return nil, err
	}

	return i, nil
}

// ResponderInterceptor responds to nack feedback messages
type ResponderInterceptor struct {
	interceptor.NoOp
	size uint16
	log  logging.LeveledLogger

	streams   map[uint32]*localStream
	streamsMu sync.Mutex
}

type localStream struct {
	sendBuffer *sendBuffer
	rtpWriter  interceptor.RTPWriter
}

// NewResponderInterceptor returns a new ResponderInterceptorFactor
func NewResponderInterceptor(opts ...ResponderOption) (*ResponderInterceptorFactory, error) {
	return &ResponderInterceptorFactory{opts}, nil
}

// BindRTCPReader lets you modify any incoming RTCP packets. It is called once per sender/receiver, however this might
// change in the future. The returned method will be called once per packet batch.
func (n *ResponderInterceptor) BindRTCPReader(reader interceptor.RTCPReader) interceptor.RTCPReader {
	return interceptor.RTCPReaderFunc(func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
		i, attr, err := reader.Read(b, a)
		if err != nil {
			return 0, nil, err
		}

		if attr == nil {
			attr = make(interceptor.Attributes)
		}
		pkts, err := attr.GetRTCPPackets(b[:i])
		if err != nil {
			return 0, nil, err
		}
		for _, rtcpPacket := range pkts {
			nack, ok := rtcpPacket.(*rtcp.TransportLayerNack)
			if !ok {
				continue
			}

			go n.resendPackets(nack)
		}

		return i, attr, err
	})
}

// BindLocalStream lets you modify any outgoing RTP packets. It is called once for per LocalStream. The returned method
// will be called once per rtp packet.
func (n *ResponderInterceptor) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	if !streamSupportNack(info) {
		return writer
	}

	// error is already checked in NewGeneratorInterceptor
	sendBuffer, _ := newSendBuffer(n.size)
	n.streamsMu.Lock()
	n.streams[info.SSRC] = &localStream{sendBuffer: sendBuffer, rtpWriter: writer}
	n.streamsMu.Unlock()

	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
		sendBuffer.add(&rtp.Packet{Header: *header, Payload: payload})

		percentage, _ := strconv.ParseFloat(os.Getenv("HYPERSCALE_DROPPED_PACKET_PERCENTAGE"), 64)
		if percentage > 0 && 100/percentage > 0 &&
			header.SequenceNumber%(uint16(100/percentage)) == 0 {
			log.WithFields(
				log.Fields{
					"subcomponent":            "interceptor",
					"sequenceNumber":          header.SequenceNumber,
					"droppedPacketPercentage": percentage,
					"type":                    "INTENSIVE",
				}).Warnf("dropping rtp packet with SN %d intentially..", header.SequenceNumber)
			return 0, nil
		}

		return writer.Write(header, payload, attributes)
	})
}

// UnbindLocalStream is called when the Stream is removed. It can be used to clean up any data related to that track.
func (n *ResponderInterceptor) UnbindLocalStream(info *interceptor.StreamInfo) {
	n.streamsMu.Lock()
	delete(n.streams, info.SSRC)
	n.streamsMu.Unlock()
}

func (n *ResponderInterceptor) resendPackets(nack *rtcp.TransportLayerNack) {
	n.streamsMu.Lock()
	stream, ok := n.streams[nack.MediaSSRC]
	n.streamsMu.Unlock()
	if !ok {
		return
	}
	extensionId, idErr := getEnvVal("HYPERSCALE_RTP_EXTENSION_SAMPLE_ATTR_ID")
	retransmitPos, posErr := getEnvVal("HYPERSCALE_RTP_EXTENSION_RETRANSMIT_ATTR_POS")
	logNacks := os.Getenv("HYPERSCALE_LOG_NACKED_PACKETS") == "true"

	for i := range nack.Nacks {
		if logNacks {
			log.WithFields(
				log.Fields{
					"subcomponent": "interceptor",
					"action":       "nackRetransmit",
					"senderSsrc":   nack.SenderSSRC,
					"mediaSsrc":    nack.MediaSSRC,
					"lostPackets":  fmt.Sprintf("%d: %b", nack.Nacks[i].PacketID, nack.Nacks[i].LostPackets),
					"type":         "INTENSIVE",
				}).Debugf("responsing to nack %v..", nack.Nacks[i])
		}
		nack.Nacks[i].Range(func(seq uint16) bool {
			var shouldEncrypt uint8 = 0
			if p := stream.sendBuffer.get(seq); p != nil {
				// setting the retransmit bit in extension
				if idErr == nil && posErr == nil {
					var b byte = 1 << retransmitPos
					ex := p.GetExtension(extensionId)
					if ex != nil {
						b |= ex[0]
						shouldEncrypt = b & 32
					}
					p.SetExtension(extensionId, []byte{b})
				}
				line := log.WithFields(
					log.Fields{
						"subcomponent":   "interceptor",
						"action":         "nackRetransmit",
						"payloadType":    p.PayloadType,
						"ssrc":           p.SSRC,
						"sequenceNumber": seq,
						"shouldEncrypt": shouldEncrypt,
						"type":           "INTENSIVE",
					})
				if _, err := stream.rtpWriter.Write(&p.Header, p.Payload, interceptor.Attributes{}); err != nil {
					n.log.Warnf("failed resending nacked packet: %+v", err)
					if logNacks {
						line.Errorf("failed to retransmit rtp packet %d.", seq)
					}
				} else {
					if logNacks {
						line.Debugf("retransmit rtp packet %d..", seq)
					}
				}
			} else {
				if os.Getenv("HYPERSCALE_WARN_ON_MISSING_NACKED_PACKETS") == "true" {
					log.WithFields(
						log.Fields{
							"subcomponent":   "interceptor",
							"sequenceNumber": seq,
							"type":           "INTENSIVE",
						}).Warnf("failed to get packet with SN %d from internal buffer..", seq)
				}
			}
			return true
		})
	}
}

func getEnvVal(envVariable string) (uint8, error) {
	envValue := os.Getenv(envVariable)
	if envValue != "" {
		parsed, err := strconv.ParseUint(envValue, 10, 8)
		if err == nil {
			return uint8(parsed), nil
		}
		return 0, err
	}
	return 0, fmt.Errorf("env variable %s does not exist", envValue)
}
