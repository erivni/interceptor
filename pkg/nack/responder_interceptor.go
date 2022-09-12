package nack

import (
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"time"

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

type packetFactory interface {
	NewPacket(header *rtp.Header, payload []byte) (*retainablePacket, error)
}

// NewInterceptor constructs a new ResponderInterceptor
func (r *ResponderInterceptorFactory) NewInterceptor(id string) (interceptor.Interceptor, error) {
	i := &ResponderInterceptor{
		size:    1024,
		log:     logging.NewDefaultLoggerFactory().NewLogger("nack_responder"),
		streams: map[uint32]*localStream{},
	}

	for _, opt := range r.opts {
		if err := opt(i); err != nil {
			return nil, err
		}
	}

	if i.packetFactory == nil {
		i.packetFactory = newPacketManager()
	}

	if _, err := newSendBuffer(i.size); err != nil {
		return nil, err
	}

	return i, nil
}

// ResponderInterceptor responds to nack feedback messages
type ResponderInterceptor struct {
	interceptor.NoOp
	size          uint16
	log           logging.LeveledLogger
	packetFactory packetFactory

	streams     map[uint32]*localStream
	streamsMu   sync.Mutex
	resendMutex *sync.Mutex
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
		go func() {
			lastBurstPackets := 0
			nacksLock, _ := strconv.ParseBool(os.Getenv("HYPERSCALE_NACKS_LOCK"))
			if nacksLock && n.resendMutex != nil && len(pkts) > 0 {
				n.resendMutex.Lock()
				defer n.resendMutex.Unlock()
			}

			for _, rtcpPacket := range pkts {
				nack, ok := rtcpPacket.(*rtcp.TransportLayerNack)
				if !ok {
					continue
				}
				n.resendPackets(nack, &lastBurstPackets)
			}
		}()

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
		pkt, err := n.packetFactory.NewPacket(header, payload)
		if err != nil {
			return 0, err
		}
		sendBuffer.add(pkt)

		percentage, _ := strconv.ParseFloat(os.Getenv("HYPERSCALE_DROPPED_PACKET_PERCENTAGE"), 64)
		if percentage > 0 && percentage <= 100 {
			drop := false
			// Switch between 3 modes of packet dropping every 20 second. (periods are 0s-19s, 20s-39s, 40s-59s)
			mode := int(time.Now().Unix() % 60 / 20)
			switch mode {
			case 0:
				// Drop random percentage of packets
				drop = float64(rand.Intn(100)+1) <= percentage
			case 1:
				// Drop sequencial percentage of packets
				drop = header.SequenceNumber%100 < uint16(percentage)
			case 2:
				// Drop a packet every Nth packet percentage (e.g. 30% will drop every 30th packet)
				drop = (int(header.SequenceNumber) % int(100/percentage)) == 0
			}

			if drop {
				log.WithFields(
					log.Fields{
						"subcomponent":            "interceptor",
						"sequenceNumber":          header.SequenceNumber,
						"droppedPacketPercentage": percentage,
						"mode":                    mode,
						"type":                    "INTENSIVE",
					}).Warnf("dropping rtp packet with SN %d intentionally..", header.SequenceNumber)
				return 0, nil
			}
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

func (n *ResponderInterceptor) resendPackets(nack *rtcp.TransportLayerNack, lastBurstPackets *int) {
	var nacksSpreadDelayMs int
	var nacksMaxPacketsBurst int
	var packetsSentWithoutDelay int
	n.streamsMu.Lock()
	stream, ok := n.streams[nack.MediaSSRC]
	n.streamsMu.Unlock()
	if !ok {
		return
	}
	extensionId, idErr := getEnvVal("HYPERSCALE_RTP_EXTENSION_SAMPLE_ATTR_ID")
	retransmitPos, posErr := getEnvVal("HYPERSCALE_RTP_EXTENSION_RETRANSMIT_ATTR_POS")
	logNacks := os.Getenv("HYPERSCALE_LOG_NACKED_PACKETS") == "true"
	nacksSpreadDelayMs, _ = strconv.Atoi(os.Getenv("HYPERSCALE_NACKS_SPREAD_PACKET_DELAY_MS")) // 0 is returned on error, in which case feature will be ignored later on
	nacksMaxPacketsBurst, _ = strconv.Atoi(os.Getenv("HYPERSCALE_NACKS_MAX_PACKET_BURST"))     // 0 is returned on error, in which case feature will be ignored later on
	dropPercentage, _ := strconv.ParseFloat(os.Getenv("HYPERSCALE_NACKS_DROPPED_PACKET_PERCENTAGE"), 64)

	packetsSentWithoutDelay = *lastBurstPackets
	pairsCount := len(nack.Nacks)
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
				}).Debugf("responsing to nack pair %d/%d..", i+1, pairsCount)
		}
		nack.Nacks[i].Range(func(seq uint16) bool {
			if p := stream.sendBuffer.get(seq); p != nil {
				// setting the retransmit bit in extension
				if idErr == nil && posErr == nil {
					var b byte = 1 << retransmitPos
					ex := p.GetExtension(extensionId)
					if ex != nil {
						b |= ex[0]
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
						"nackPairIdx":    i,
						"type":           "INTENSIVE",
					})

				if dropPercentage > 0 && dropPercentage <= 100 && float64(rand.Intn(100)+1) <= dropPercentage {
					line.Warnf("dropping rtp nack packet with SN %d intentionally..", seq)
					packetsSentWithoutDelay++ // From testing spread prespective, count this dropped nack packet as it was re-transmitted
					*lastBurstPackets = packetsSentWithoutDelay
					return true
				}

				if nacksMaxPacketsBurst > 0 && nacksSpreadDelayMs > 0 && packetsSentWithoutDelay >= nacksMaxPacketsBurst {
					packetsSentWithoutDelay = 0
					time.Sleep(time.Duration(nacksSpreadDelayMs) * time.Millisecond)
				}
				if _, err := stream.rtpWriter.Write(p.Header(), p.Payload(), interceptor.Attributes{}); err != nil {
					n.log.Warnf("failed resending nacked packet: %+v", err)
					if logNacks {
						line.Errorf("failed to retransmit rtp packet %d.", seq)
					}
				} else {
					packetsSentWithoutDelay++
					if logNacks {
						line.Debugf("retransmitted rtp packet %d..", seq)
					}
				}
				p.Release()
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
			*lastBurstPackets = packetsSentWithoutDelay
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
