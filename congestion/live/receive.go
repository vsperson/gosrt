package live

import (
	"container/list"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/datarhei/gosrt/circular"
	"github.com/datarhei/gosrt/congestion"
	"github.com/datarhei/gosrt/packet"
)

// ReceiveConfig is the configuration for the liveRecv congestion control
type ReceiveConfig struct {
	InitialSequenceNumber circular.Number
	PeriodicACKInterval   uint64 // microseconds
	PeriodicNAKInterval   uint64 // microseconds
	ReorderTolerance      uint32 // packets
	OnSendACK             func(seq circular.Number, light bool)
	OnSendNAK             func(list []circular.Number)
	OnDeliver             func(p packet.Packet)
}

// receiver implements the Receiver interface
type receiver struct {
	maxSeenSequenceNumber       circular.Number
	lastACKSequenceNumber       circular.Number
	lastDeliveredSequenceNumber circular.Number
	packetList                  *list.List
	dropped                     map[uint32]struct{} // sequence numbers of packets in packetList that are dropped
	lock                        sync.RWMutex

	nPackets uint

	periodicACKInterval uint64 // config
	periodicNAKInterval uint64 // config
	reorderTolerance    uint32 // config

	lastPeriodicACK uint64
	lastPeriodicNAK uint64

	avgPayloadSize  float64 // bytes
	avgLinkCapacity float64 // packets per second

	probeTime    time.Time
	probeNextSeq circular.Number

	statistics congestion.ReceiveStats

	rate struct {
		last   uint64 // microseconds
		period uint64

		packets      uint64
		bytes        uint64
		bytesRetrans uint64

		packetsPerSecond float64
		bytesPerSecond   float64

		pktLossRate float64
	}

	sendACK func(seq circular.Number, light bool)
	sendNAK func(list []circular.Number)
	deliver func(p packet.Packet)
}

// NewReceiver takes a ReceiveConfig and returns a new Receiver
func NewReceiver(config ReceiveConfig) congestion.Receiver {
	r := &receiver{
		maxSeenSequenceNumber:       config.InitialSequenceNumber.Dec(),
		lastACKSequenceNumber:       config.InitialSequenceNumber.Dec(),
		lastDeliveredSequenceNumber: config.InitialSequenceNumber.Dec(),
		packetList:                  list.New(),
		dropped:                     map[uint32]struct{}{},

		periodicACKInterval: config.PeriodicACKInterval,
		periodicNAKInterval: config.PeriodicNAKInterval,
		reorderTolerance:    config.ReorderTolerance,

		avgPayloadSize: 1456, //  5.1.2. SRT's Default LiveCC Algorithm

		sendACK: config.OnSendACK,
		sendNAK: config.OnSendNAK,
		deliver: config.OnDeliver,
	}

	if r.sendACK == nil {
		r.sendACK = func(seq circular.Number, light bool) {}
	}

	if r.sendNAK == nil {
		r.sendNAK = func(list []circular.Number) {}
	}

	if r.deliver == nil {
		r.deliver = func(p packet.Packet) {}
	}

	r.rate.last = 0
	r.rate.period = uint64(time.Second.Microseconds())

	return r
}

func (r *receiver) Stats() congestion.ReceiveStats {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.statistics.BytePayload = uint64(r.avgPayloadSize)
	r.statistics.MbpsEstimatedRecvBandwidth = r.rate.bytesPerSecond * 8 / 1024 / 1024
	r.statistics.MbpsEstimatedLinkCapacity = r.avgLinkCapacity * packet.MAX_PAYLOAD_SIZE * 8 / 1024 / 1024
	r.statistics.PktLossRate = r.rate.pktLossRate

	return r.statistics
}

func (r *receiver) PacketRate() (pps, bps, capacity float64) {
	r.lock.Lock()
	defer r.lock.Unlock()

	pps = r.rate.packetsPerSecond
	bps = r.rate.bytesPerSecond
	capacity = r.avgLinkCapacity

	return
}

func (r *receiver) Flush() {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.packetList = r.packetList.Init()
	clear(r.dropped)
}

func (r *receiver) Push(pkt packet.Packet) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.push(pkt, false)
}

func (r *receiver) PushDropped(pkt packet.Packet) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.push(pkt, true)
}

// push puts a packet into the receiver queue. If dropped is true, the packet takes its place in the
// sequence, such that it will not be reported as missing, but it will not be delivered. The caller
// must hold the lock.
func (r *receiver) push(pkt packet.Packet, dropped bool) {
	if pkt == nil {
		return
	}

	// This is not really well (not at all) described in the specs. See core.cpp and window.h
	// and search for PUMASK_SEQNO_PROBE (0xF). Every 16th and 17th packet are
	// sent in pairs. This is used as a probe for the theoretical capacity of the link.
	if !pkt.Header().RetransmittedPacketFlag {
		probe := pkt.Header().PacketSequenceNumber.Val() & 0xF
		if probe == 0 {
			r.probeTime = time.Now()
			r.probeNextSeq = pkt.Header().PacketSequenceNumber.Inc()
		} else if probe == 1 && pkt.Header().PacketSequenceNumber.Equals(r.probeNextSeq) && !r.probeTime.IsZero() && pkt.Len() != 0 {
			// The time between packets scaled to a fully loaded packet
			diff := float64(time.Since(r.probeTime).Microseconds()) * (packet.MAX_PAYLOAD_SIZE / float64(pkt.Len()))
			if diff != 0 {
				// Here we're doing an average of the measurements.
				r.avgLinkCapacity = 0.875*r.avgLinkCapacity + 0.125*1_000_000/diff
			}
		} else {
			r.probeTime = time.Time{}
		}
	} else {
		r.probeTime = time.Time{}
	}

	r.nPackets++

	pktLen := pkt.Len()

	r.rate.packets++
	r.rate.bytes += pktLen

	r.statistics.Pkt++
	r.statistics.Byte += pktLen

	//pkt.PktTsbpdTime = pkt.Timestamp + r.delay
	if pkt.Header().RetransmittedPacketFlag {
		r.statistics.PktRetrans++
		r.statistics.ByteRetrans += pktLen

		r.rate.bytesRetrans += pktLen
	}

	//  5.1.2. SRT's Default LiveCC Algorithm
	r.avgPayloadSize = 0.875*r.avgPayloadSize + 0.125*float64(pktLen)

	if pkt.Header().PacketSequenceNumber.Lte(r.lastDeliveredSequenceNumber) {
		// Too old, because up until r.lastDeliveredSequenceNumber, we already delivered
		r.statistics.PktBelated++
		r.statistics.ByteBelated += pktLen

		// Note: libsrt does NOT count this as PktDrop - packet is just ignored
		return
	}

	if pkt.Header().PacketSequenceNumber.Lt(r.lastACKSequenceNumber) {
		// Already acknowledged, ignoring
		// Note: libsrt does NOT count this as PktDrop - packet is just ignored
		return
	}

	if pkt.Header().PacketSequenceNumber.Equals(r.maxSeenSequenceNumber.Inc()) {
		// In order, the packet we expected
		r.maxSeenSequenceNumber = pkt.Header().PacketSequenceNumber
	} else if pkt.Header().PacketSequenceNumber.Lte(r.maxSeenSequenceNumber) {
		// Out of order, is it a missing piece? put it in the correct position
		for e := r.packetList.Front(); e != nil; e = e.Next() {
			p := e.Value.(packet.Packet)

			if p.Header().PacketSequenceNumber == pkt.Header().PacketSequenceNumber {
				// Already received (has been sent more than once), ignoring
				// Note: libsrt does NOT count duplicates as PktDrop
				break
			} else if p.Header().PacketSequenceNumber.Gt(pkt.Header().PacketSequenceNumber) {
				// Late arrival, this fills a gap
				if dropped {
					r.markDropped(pkt)
				} else {
					r.statistics.PktBuf++
					r.statistics.PktUnique++
					r.statistics.PktLate++

					r.statistics.ByteBuf += pktLen
					r.statistics.ByteUnique += pktLen
					r.statistics.ByteLate += pktLen
				}

				r.packetList.InsertBefore(pkt, e)

				break
			}
		}

		return
	} else {
		// Too far ahead, there are some missing sequence numbers.
		// The distance is one more than the actual missing packets.
		missing := uint64(pkt.Header().PacketSequenceNumber.Distance(r.maxSeenSequenceNumber)) - 1

		// As libsrt, count the missing packets as loss right away, regardless whether they
		// are lost or only reordered.
		r.statistics.PktLoss += missing
		r.statistics.ByteLoss += missing * uint64(r.avgPayloadSize)

		// Only send an immediate NAK if more packets are missing than the reorder tolerance
		// allows. Otherwise periodicNAK will report them if they are still missing.
		if missing > uint64(r.reorderTolerance) {
			r.sendNAK([]circular.Number{
				r.maxSeenSequenceNumber.Inc(),
				pkt.Header().PacketSequenceNumber.Dec(),
			})
		}

		r.maxSeenSequenceNumber = pkt.Header().PacketSequenceNumber
	}

	if dropped {
		r.markDropped(pkt)
	} else {
		r.statistics.PktBuf++
		r.statistics.PktUnique++

		r.statistics.ByteBuf += pktLen
		r.statistics.ByteUnique += pktLen
	}

	r.packetList.PushBack(pkt)
}

// markDropped marks a packet in the queue as dropped. As libsrt, it is counted as dropped right away.
// The caller must hold the lock.
func (r *receiver) markDropped(pkt packet.Packet) {
	r.dropped[pkt.Header().PacketSequenceNumber.Val()] = struct{}{}

	r.statistics.PktDrop++
	r.statistics.ByteDrop += pkt.Len()
}

func (r *receiver) periodicACK(now uint64) (ok bool, sequenceNumber circular.Number, lite bool) {
	r.lock.Lock()
	defer r.lock.Unlock()

	// 4.8.1. Packet Acknowledgement (ACKs, ACKACKs)
	if now-r.lastPeriodicACK < r.periodicACKInterval {
		if r.nPackets >= 64 {
			lite = true // Send light ACK
		} else {
			return
		}
	}

	ackSequenceNumber := r.lastACKSequenceNumber

	// Find the sequence number up until we have all in a row.
	// Where the first gap is (or at the end of the list) is where we can ACK to.
	// TLPKTDROP: If there are gaps and the next packet's delivery time has passed,
	// virtually drop the missing packets and continue.

	for e := r.packetList.Front(); e != nil; e = e.Next() {
		p := e.Value.(packet.Packet)

		// Skip packets that we already ACK'd.
		if p.Header().PacketSequenceNumber.Lte(ackSequenceNumber) {
			continue
		}

		// Check if the packet is the next in the row.
		if p.Header().PacketSequenceNumber.Equals(ackSequenceNumber.Inc()) {
			ackSequenceNumber = p.Header().PacketSequenceNumber
			continue
		}

		// There's a gap. Check if the packet's delivery time has passed (TLPKTDROP).
		// If so, skip the gap and continue ACK'ing.
		// The skipped packets are counted as dropped in Tick, when the delivery moves past them.
		if p.Header().PktTsbpdTime <= now {
			// Skip the gap and continue with this packet
			ackSequenceNumber = p.Header().PacketSequenceNumber
			continue
		}

		// Delivery time hasn't passed, stop here
		break
	}

	ok = true
	sequenceNumber = ackSequenceNumber.Inc()

	// Keep track of the last ACK's sequence number. With this we can faster ignore
	// packets that come in late that have a lower sequence number.
	r.lastACKSequenceNumber = ackSequenceNumber

	r.lastPeriodicACK = now
	r.nPackets = 0

	// Calculate MsBuf based on the entire packet list (first to last),
	// similar to libsrt's getRcvDataSize/getTimespan_ms.
	// This includes gaps/missing packets in the timespan.
	if r.packetList.Len() > 0 {
		first := r.packetList.Front().Value.(packet.Packet)
		last := r.packetList.Back().Value.(packet.Packet)
		r.statistics.MsBuf = (last.Header().PktTsbpdTime - first.Header().PktTsbpdTime) / 1_000
	} else {
		r.statistics.MsBuf = 0
	}

	return
}

func (r *receiver) periodicNAK(now uint64) []circular.Number {
	r.lock.RLock()
	defer r.lock.RUnlock()

	if now-r.lastPeriodicNAK < r.periodicNAKInterval {
		return nil
	}

	list := []circular.Number{}

	// Send a periodic NAK

	ackSequenceNumber := r.lastACKSequenceNumber

	// Send a NAK for all gaps.
	// Not all gaps might get announced because the size of the NAK packet is limited.
	for e := r.packetList.Front(); e != nil; e = e.Next() {
		p := e.Value.(packet.Packet)

		// Skip packets that we already ACK'd.
		if p.Header().PacketSequenceNumber.Lte(ackSequenceNumber) {
			continue
		}

		// If this packet is not in sequence, we stop here and report that gap.
		if !p.Header().PacketSequenceNumber.Equals(ackSequenceNumber.Inc()) {
			nackSequenceNumber := ackSequenceNumber.Inc()

			list = append(list, nackSequenceNumber)
			list = append(list, p.Header().PacketSequenceNumber.Dec())
		}

		ackSequenceNumber = p.Header().PacketSequenceNumber
	}

	r.lastPeriodicNAK = now

	return list
}

func (r *receiver) Tick(now uint64) {
	if ok, sequenceNumber, lite := r.periodicACK(now); ok {
		r.sendACK(sequenceNumber, lite)
	}

	if list := r.periodicNAK(now); len(list) != 0 {
		r.sendNAK(list)
	}

	// Deliver packets whose PktTsbpdTime is ripe.
	r.lock.Lock()
	removeList := make([]*list.Element, 0, r.packetList.Len())
	expectedSeq := r.lastDeliveredSequenceNumber.Inc()

	for e := r.packetList.Front(); e != nil; e = e.Next() {
		p := e.Value.(packet.Packet)

		// Check if this packet is ready for delivery (TSBPD)
		if p.Header().PktTsbpdTime > now {
			// Not ready yet, stop processing
			break
		}

		// Packet is ready for delivery. Check if there's a gap before it.
		if !p.Header().PacketSequenceNumber.Equals(expectedSeq) {
			// There's a gap. TLPKTDROP: skip missing packets and continue delivery.
			// The skipped packets will not be delivered anymore. As libsrt, count them as dropped.
			skipped := uint64(p.Header().PacketSequenceNumber.Distance(expectedSeq))
			r.statistics.PktLost += skipped
			r.statistics.PktDrop += skipped
			r.statistics.ByteDrop += skipped * uint64(r.avgPayloadSize)

			// Update sequence numbers to skip the gap.
			r.lastDeliveredSequenceNumber = p.Header().PacketSequenceNumber.Dec()
			if r.lastACKSequenceNumber.Lt(p.Header().PacketSequenceNumber.Dec()) {
				r.lastACKSequenceNumber = p.Header().PacketSequenceNumber.Dec()
			}
		}

		r.lastDeliveredSequenceNumber = p.Header().PacketSequenceNumber
		removeList = append(removeList, e)

		if _, ok := r.dropped[p.Header().PacketSequenceNumber.Val()]; ok {
			// A dropped packet only held its place in the sequence, it is not delivered
			delete(r.dropped, p.Header().PacketSequenceNumber.Val())
			p.Decommission()
		} else {
			// Deliver the packet
			r.statistics.PktBuf--
			r.statistics.ByteBuf -= p.Len()

			r.deliver(p)
		}

		// Update expected sequence for next iteration
		expectedSeq = p.Header().PacketSequenceNumber.Inc()
	}

	for _, e := range removeList {
		r.packetList.Remove(e)
	}
	r.lock.Unlock()

	r.lock.Lock()
	tdiff := now - r.rate.last // microseconds

	if tdiff > r.rate.period {
		r.rate.packetsPerSecond = float64(r.rate.packets) / (float64(tdiff) / 1000 / 1000)
		r.rate.bytesPerSecond = float64(r.rate.bytes) / (float64(tdiff) / 1000 / 1000)
		if r.rate.bytes != 0 {
			r.rate.pktLossRate = float64(r.rate.bytesRetrans) / float64(r.rate.bytes) * 100
		} else {
			r.rate.bytes = 0
		}

		r.rate.packets = 0
		r.rate.bytes = 0
		r.rate.bytesRetrans = 0

		r.rate.last = now
	}
	r.lock.Unlock()
}

func (r *receiver) SetNAKInterval(nakInterval uint64) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.periodicNAKInterval = nakInterval
}

func (r *receiver) String(t uint64) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("maxSeen=%d lastACK=%d lastDelivered=%d\n", r.maxSeenSequenceNumber.Val(), r.lastACKSequenceNumber.Val(), r.lastDeliveredSequenceNumber.Val()))

	r.lock.RLock()
	for e := r.packetList.Front(); e != nil; e = e.Next() {
		p := e.Value.(packet.Packet)

		b.WriteString(fmt.Sprintf("   %d @ %d (in %d)\n", p.Header().PacketSequenceNumber.Val(), p.Header().PktTsbpdTime, int64(p.Header().PktTsbpdTime)-int64(t)))
	}
	r.lock.RUnlock()

	return b.String()
}
