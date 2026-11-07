package srt

import (
	"testing"
	"time"

	"github.com/datarhei/gosrt/circular"
	"github.com/datarhei/gosrt/congestion/live"
	"github.com/datarhei/gosrt/crypto"
	"github.com/datarhei/gosrt/packet"
	"github.com/stretchr/testify/require"
)

func TestUndecryptedPacketIsDropped(t *testing.T) {
	var ack uint32
	var naks []uint32
	var delivered [][]byte
	recv := live.NewReceiver(live.ReceiveConfig{
		InitialSequenceNumber: circular.New(100, packet.MAX_SEQUENCENUMBER),
		PeriodicACKInterval:   10,
		PeriodicNAKInterval:   20,
		OnSendACK:             func(seq circular.Number, _ bool) { ack = seq.Val() },
		OnSendNAK: func(seqs []circular.Number) {
			for _, seq := range seqs {
				naks = append(naks, seq.Val())
			}
		},
		OnDeliver: func(p packet.Packet) {
			delivered = append(delivered, p.Data())
		},
	})

	cr, err := crypto.New(16)
	require.NoError(t, err)
	require.NoError(t, cr.GenerateSEK(packet.EvenKeyEncrypted))

	c := &srtConn{
		recv:            recv,
		snd:             live.NewSender(live.SendConfig{InitialSequenceNumber: circular.New(0, packet.MAX_SEQUENCENUMBER)}),
		crypto:          cr,
		config:          DefaultConfig(),
		start:           time.Now(),
		peerIdleTimeout: time.NewTimer(time.Hour),
		logger:          NewLogger(nil),
	}
	defer c.peerIdleTimeout.Stop()

	push := func(seq uint32, key packet.PacketEncryption, data string) {
		p := packet.NewPacket(nil)
		p.Header().PacketSequenceNumber = circular.New(seq, packet.MAX_SEQUENCENUMBER)
		p.Header().MessageNumber = seq
		p.Header().KeyBaseEncryptionFlag = key

		payload := []byte(data)
		if key == packet.EvenKeyEncrypted {
			require.NoError(t, cr.EncryptOrDecryptPayload(payload, key, seq))
		}
		p.SetData(payload)

		c.handlePacket(p)
	}

	push(100, packet.EvenKeyEncrypted, "a")
	push(101, packet.UnencryptedPacket, "injected")  // unencrypted on an encrypted connection
	push(102, packet.PacketEncryption(3), "garbage") // can't be decrypted, the key is invalid
	push(103, packet.EvenKeyEncrypted, "b")

	// The dropped packets are neither reported as missing nor delivered, and they don't hold the ACK
	recv.Tick(1_000_000_000_000)
	require.Equal(t, uint32(104), ack)
	require.Empty(t, naks)
	require.Equal(t, [][]byte{[]byte("a"), []byte("b")}, delivered)

	// As libsrt, they are counted as received, dropped and undecrypted
	s := &Statistics{}
	c.Stats(s)
	require.Equal(t, uint64(4), s.Accumulated.PktRecv)
	require.Equal(t, uint64(2), s.Accumulated.PktRecvUnique)
	require.Equal(t, uint64(2), s.Accumulated.PktRecvDrop)
	require.Equal(t, uint64(2), s.Accumulated.PktRecvUndecrypt)
	require.Equal(t, uint64(2), s.Accumulated.PktRecvLost)
	require.Equal(t, uint64(0), s.Accumulated.PktRecvLoss)
}
