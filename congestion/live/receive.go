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

	// CanDeliver is optional. If it returns false, the packets that are ready for delivery are
	// kept in the receiver until the next tick, instead of passing them to OnDeliver.
	CanDeliver func() bool

	// MaxBacklog is the max. number of packets that are kept because they are ready for delivery but
	// CanDeliver returned false. If there are more, the oldest are dropped. 0 means no limit.
	MaxBacklog uint64
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
	maxLinkCapacity float64 // Mbps, tracks maximum observed receive bandwidth

	// Link Capacity Probing
	lastProbeTime  time.Time
	lastProbeSeq   circular.Number
	probeSamples   []float64
	probeSampleIdx int

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

	sendACK    func(seq circular.Number, light bool)
	sendNAK    func(list []circular.Number)
	deliver    func(p packet.Packet)
	canDeliver func() bool
	maxBacklog uint64
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

		probeSamples: make([]float64, 16), // Store last 16 samples for median filtering

		canDeliver: config.CanDeliver,
		maxBacklog: config.MaxBacklog,
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

	if r.canDeliver == nil {
		r.canDeliver = func() bool { return true }
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
	r.updateLinkCapacity()

	r.statistics.MbpsEstimatedLinkCapacity = r.maxLinkCapacity
	r.statistics.PktLossRate = r.rate.pktLossRate

	return r.statistics
}

// updateLinkCapacity updates the estimated link capacity in Mbps. The caller must hold the lock.
func (r *receiver) updateLinkCapacity() {
	mbpsRecvBandwidth := r.rate.bytesPerSecond * 8 / 1024 / 1024

	// Link capacity from probe pairs with median filter (libsrt algorithm)
	// We use the probeSamples collected in Push() to estimate capacity.
	// If no samples (e.g. buffered socket), fallback to max observed bandwidth.

	// Calculate median of probe samples
	var medianCapacity float64
	if len(r.probeSamples) > 0 && r.probeSamples[0] > 0 {
		// Copy samples to avoid sorting the ring buffer
		sorted := make([]float64, len(r.probeSamples))
		copy(sorted, r.probeSamples)
		// Simple bubble sort for small array
		for i := 0; i < len(sorted); i++ {
			for j := i + 1; j < len(sorted); j++ {
				if sorted[i] > sorted[j] {
					sorted[i], sorted[j] = sorted[j], sorted[i]
				}
			}
		}
		// Count non-zero samples
		count := 0
		for _, v := range sorted {
			if v > 0 {
				count++
			}
		}

		if count > 0 {
			// Get median of non-zero samples
			// The zeros are at the beginning because we sorted ascending
			startIdx := len(sorted) - count
			medianCapacity = sorted[startIdx+count/2]
		}
	}

	// If probe estimation works, use it. Otherwise fallback to max bandwidth.
	if medianCapacity > mbpsRecvBandwidth {
		r.maxLinkCapacity = medianCapacity
	} else if mbpsRecvBandwidth > r.maxLinkCapacity {
		r.maxLinkCapacity = mbpsRecvBandwidth
	}
}

func (r *receiver) PacketRate() (pps, bps, capacity float64) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.updateLinkCapacity()

	pps = r.rate.packetsPerSecond
	bps = r.rate.bytesPerSecond
	// The link capacity is reported in packets per second (full sized packets)
	capacity = r.maxLinkCapacity * 1024 * 1024 / 8 / packet.MAX_PAYLOAD_SIZE

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

	// Probe Packet Logic for Link Capacity Estimation
	// Probe packets are sent every 16 packets. Pairs are (0, 1), (16, 17), etc.
	// We measure the time difference between the arrival of the pair.
	seqVal := pkt.Header().PacketSequenceNumber.Val()
	probeSeq := seqVal & 0xF

	if probeSeq == 0 {
		r.lastProbeTime = time.Now()
		r.lastProbeSeq = pkt.Header().PacketSequenceNumber
	} else if probeSeq == 1 {
		// Check if this is the pair of the last probe
		if pkt.Header().PacketSequenceNumber.Equals(r.lastProbeSeq.Inc()) {
			now := time.Now()
			delta := now.Sub(r.lastProbeTime)

			// Filter out buffered packets (delta too small)
			// 10us is arbitrary but filters out "instant" reads from buffer
			// At 10Gbps, 1500 bytes take ~1.2us. At 1Gbps ~12us.
			// If we see < 1us, it's definitely buffered.
			// Go's time resolution might be limited.
			if delta > 1*time.Microsecond {
				// Calculate Mbps
				mbps := float64(pktLen*8) / delta.Seconds() / 1024 / 1024

				// Sanity check: ignore unrealistic values (e.g. > 100Gbps)
				if mbps < 100000 {
					if len(r.probeSamples) > 0 {
						r.probeSamples[r.probeSampleIdx] = mbps
						r.probeSampleIdx = (r.probeSampleIdx + 1) % len(r.probeSamples)
					}
				}
			}
		}
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

	// release removes a ripe packet from the buffer and delivers it. If deliver is false, the packet is dropped
	// because the application didn't read in time.
	release := func(e *list.Element, deliver bool) {
		p := e.Value.(packet.Packet)

		// Check if there's a gap before the packet.
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
			r.statistics.PktBuf--
			r.statistics.ByteBuf -= p.Len()

			if deliver {
				r.deliver(p)
			} else {
				r.statistics.PktQueueDrop++
				r.statistics.ByteQueueDrop += p.Len()

				p.Decommission()
			}
		}

		// Update expected sequence for next iteration
		expectedSeq = p.Header().PacketSequenceNumber.Inc()
	}

	var backlog []*list.Element
	for e := r.packetList.Front(); e != nil; e = e.Next() {
		p := e.Value.(packet.Packet)

		// Check if this packet is ready for delivery (TSBPD)
		if p.Header().PktTsbpdTime > now {
			// Not ready yet, stop processing
			break
		}

		if backlog != nil || !r.canDeliver() {
			// The packet is ready, but it can't be delivered now. Keep it until the next tick.
			backlog = append(backlog, e)
			continue
		}

		release(e, true)
	}

	if r.maxBacklog != 0 && uint64(len(backlog)) > r.maxBacklog {
		// Too many packets are waiting for delivery, drop the oldest
		for _, e := range backlog[:uint64(len(backlog))-r.maxBacklog] {
			release(e, false)
		}
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
