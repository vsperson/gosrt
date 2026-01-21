package live

import (
	"container/list"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/datarhei/gosrt/circular"
	"github.com/datarhei/gosrt/congestion"
	"github.com/datarhei/gosrt/packet"
)

// probeWindowSize is the number of probe pair samples to keep for median filter.
// libsrt uses median filter to discard outliers from bandwidth estimation.
const probeWindowSize = 16

// ReceiveConfig is the configuration for the liveRecv congestion control
type ReceiveConfig struct {
	InitialSequenceNumber circular.Number
	PeriodicACKInterval   uint64 // microseconds
	PeriodicNAKInterval   uint64 // microseconds
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
	lock                        sync.RWMutex

	nPackets uint

	periodicACKInterval uint64 // config
	periodicNAKInterval uint64 // config

	lastPeriodicACK uint64
	lastPeriodicNAK uint64

	avgPayloadSize float64 // bytes

	// Probe pair bandwidth estimation (libsrt algorithm)
	probeTime     int64 // nanoseconds since epoch (from PacketHeader.ArrivalTime)
	probeNextSeq  circular.Number
	probeSamples  []float64 // circular buffer of capacity samples (pps)
	probeIndex    int       // current index in circular buffer
	probeCount    int       // number of samples collected
	linkCapacity  float64   // median-filtered link capacity (pps)

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

		periodicACKInterval: config.PeriodicACKInterval,
		periodicNAKInterval: config.PeriodicNAKInterval,

		avgPayloadSize: 1456, //  5.1.2. SRT's Default LiveCC Algorithm

		// Probe pair samples buffer for median filter (libsrt algorithm)
		probeSamples: make([]float64, probeWindowSize),

		sendACK: config.OnSendACK,
		sendNAK: config.OnSendNAK,
		deliver: config.OnDeliver,
	}

	// Initialize probe samples with 1000 microseconds (1 ms) as libsrt does.
	// This prevents unrealistic capacity estimates when first probes arrive.
	// 1000us = 1ms = 1000 pps = ~1.1 Gbps with full packets
	for i := range r.probeSamples {
		r.probeSamples[i] = 1000
	}
	r.probeCount = probeWindowSize // Buffer is pre-filled, so it's full from the start

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

	// Link capacity from probe pairs with median filter (libsrt algorithm)
	linkCapacity := r.linkCapacity * packet.MAX_PAYLOAD_SIZE * 8 / 1024 / 1024

	// Fallback: if probe pairs didn't work (losses, jitter), use receive bandwidth
	// as a lower bound estimate. The actual link capacity is at least what we're receiving.
	if linkCapacity < r.statistics.MbpsEstimatedRecvBandwidth {
		linkCapacity = r.statistics.MbpsEstimatedRecvBandwidth
	}

	r.statistics.MbpsEstimatedLinkCapacity = linkCapacity
	r.statistics.PktLossRate = r.rate.pktLossRate

	return r.statistics
}

func (r *receiver) PacketRate() (pps, bps, capacity float64) {
	r.lock.Lock()
	defer r.lock.Unlock()

	pps = r.rate.packetsPerSecond
	bps = r.rate.bytesPerSecond
	capacity = r.linkCapacity

	return
}

func (r *receiver) Flush() {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.packetList = r.packetList.Init()
}

// calculateMedianCapacity returns the bandwidth in packets-per-second.
// This is the libsrt algorithm:
// 1. Samples are stored as scaled intervals in microseconds
// 2. Find median interval
// 3. Filter: keep only intervals in range [median/8, median*8]
// 4. Calculate average of filtered intervals + median
// 5. Convert to packets-per-second: 1_000_000 / average_interval
// Must be called with lock held.
func (r *receiver) calculateMedianCapacity() float64 {
	if r.probeCount == 0 {
		return 0
	}

	// Copy samples (scaled intervals) for sorting
	intervals := make([]float64, r.probeCount)
	copy(intervals, r.probeSamples[:r.probeCount])

	// Sort to find median
	sort.Float64s(intervals)
	mid := r.probeCount / 2
	median := intervals[mid]

	// Filter range: [median/8, median*8]
	lower := median / 8.0
	upper := median * 8.0

	// Calculate average of filtered intervals (libsrt includes median)
	sum := median
	count := 1.0

	for _, interval := range intervals {
		// Keep intervals in filter range (excluding median as we already added it)
		if interval > lower && interval < upper && interval != median {
			sum += interval
			count++
		}
	}

	if count == 0 {
		return 0
	}

	// Average interval in microseconds
	avgInterval := sum / count

	// Convert to packets per second: 1_000_000 / interval_us
	return 1_000_000 / avgInterval
}

func (r *receiver) Push(pkt packet.Packet) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if pkt == nil {
		return
	}

	// Probe pair bandwidth estimation (libsrt algorithm).
	// Every 16th and 17th packet are sent back-to-back as probe pairs.
	// The receiver measures inter-arrival time, scales it by packet size, and filters outliers.
	// libsrt stores: (interval_us * MAX_PAYLOAD_SIZE) / actual_packet_size
	// This normalizes intervals to full-size packets.
	if !pkt.Header().RetransmittedPacketFlag {
		probe := pkt.Header().PacketSequenceNumber.Val() & 0xF
		if probe == 0 {
			// Capture arrival time from packet header (set at network receive time)
			r.probeTime = pkt.Header().ArrivalTime
			r.probeNextSeq = pkt.Header().PacketSequenceNumber.Inc()
		} else if probe == 1 && pkt.Header().PacketSequenceNumber.Equals(r.probeNextSeq) && r.probeTime != 0 && pkt.Len() != 0 {
			// Use arrival time from packet header (captured BEFORE lock, avoiding queue delays)
			arrivalTime := pkt.Header().ArrivalTime
			
			// DEBUG: Check if ArrivalTime is actually set
			if arrivalTime == 0 {
				println("ERROR: ArrivalTime not set on packet!")
				r.probeTime = 0
				return
			}
			
			// Measure inter-arrival time in microseconds
			intervalUs := float64(arrivalTime-r.probeTime) / 1000.0 // nanoseconds to microseconds
			
			// DEBUG: Log probe measurements
			println(fmt.Sprintf("Probe interval: %.2f us, seq=%d, pktLen=%d, capacity=%.2f Gbps", 
				intervalUs, pkt.Header().PacketSequenceNumber.Val(), pkt.Len(),
				(1_000_000/intervalUs)*float64(packet.MAX_PAYLOAD_SIZE)*8/1024/1024/1000))
			
			// With accurate arrival timestamps, all intervals should be realistic
			if intervalUs > 0 {
				// Scale interval by packet size (libsrt algorithm)
				// stored_interval = interval * MAX_PAYLOAD_SIZE / actual_packet_size
				scaledInterval := intervalUs * float64(packet.MAX_PAYLOAD_SIZE) / float64(pkt.Len())

				// Store scaled interval (NOT packets-per-second)
				r.probeSamples[r.probeIndex] = scaledInterval
				r.probeIndex = (r.probeIndex + 1) % probeWindowSize
				if r.probeCount < probeWindowSize {
					r.probeCount++
				}

				// Calculate bandwidth from filtered average of intervals
				r.linkCapacity = r.calculateMedianCapacity()
			}
			
			r.probeTime = 0
		} else {
			r.probeTime = 0
		}
	} else {
		r.probeTime = 0
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
				r.statistics.PktBuf++
				r.statistics.PktUnique++

				r.statistics.ByteBuf += pktLen
				r.statistics.ByteUnique += pktLen

				r.packetList.InsertBefore(pkt, e)

				break
			}
		}

		return
	} else {
		// Too far ahead, there are some missing sequence numbers, immediate NAK report
		// here we can prevent a possibly unnecessary NAK with SRTO_LOXXMAXTTL
		r.sendNAK([]circular.Number{
			r.maxSeenSequenceNumber.Inc(),
			pkt.Header().PacketSequenceNumber.Dec(),
		})

		len := uint64(pkt.Header().PacketSequenceNumber.Distance(r.maxSeenSequenceNumber))
		r.statistics.PktLoss += len
		r.statistics.ByteLoss += len * uint64(r.avgPayloadSize)

		r.maxSeenSequenceNumber = pkt.Header().PacketSequenceNumber
	}

	r.statistics.PktBuf++
	r.statistics.PktUnique++

	r.statistics.ByteBuf += pktLen
	r.statistics.ByteUnique += pktLen

	r.packetList.PushBack(pkt)
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

	minPktTsbpdTime, maxPktTsbpdTime := uint64(0), uint64(0)
	ackSequenceNumber := r.lastACKSequenceNumber

	e := r.packetList.Front()
	if e != nil {
		p := e.Value.(packet.Packet)

		minPktTsbpdTime = p.Header().PktTsbpdTime
		maxPktTsbpdTime = p.Header().PktTsbpdTime
	}

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
			maxPktTsbpdTime = p.Header().PktTsbpdTime
			continue
		}

		// There's a gap. Check if the packet's delivery time has passed (TLPKTDROP).
		// If so, skip the gap and continue ACK'ing.
		// Note: Don't count as PktDrop here - if packets arrive later via retransmit,
		// they will be delivered. Real drops are only when packets never arrive.
		if p.Header().PktTsbpdTime <= now {
			// Skip the gap and continue with this packet
			ackSequenceNumber = p.Header().PacketSequenceNumber
			maxPktTsbpdTime = p.Header().PktTsbpdTime
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

	r.statistics.MsBuf = (maxPktTsbpdTime - minPktTsbpdTime) / 1_000

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
	// Note: libsrt doesn't count TLPKTDROP as PktDrop in statistics - only real drops
	// (packets that never arrived) are counted.
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
			// Note: Don't count as PktDrop - missing packets may arrive later via retransmit.
			// Update sequence numbers to skip the gap.
			r.lastDeliveredSequenceNumber = p.Header().PacketSequenceNumber.Dec()
			if r.lastACKSequenceNumber.Lt(p.Header().PacketSequenceNumber.Dec()) {
				r.lastACKSequenceNumber = p.Header().PacketSequenceNumber.Dec()
			}
		}

		// Deliver the packet
		r.statistics.PktBuf--
		r.statistics.ByteBuf -= p.Len()

		r.lastDeliveredSequenceNumber = p.Header().PacketSequenceNumber

		r.deliver(p)
		removeList = append(removeList, e)

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
