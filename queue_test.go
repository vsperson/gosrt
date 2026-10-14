package srt

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A connection whose application doesn't read must not stall the other connections on the same listener.
func TestBlockOnFullQueueSlowReader(t *testing.T) {
	config := DefaultConfig()
	config.BlockOnFullQueue = true
	config.ReadQueueSize = 16
	config.NetworkQueueSize = 16
	config.ReadBacklogSize = 64

	ln, err := Listen("srt", "127.0.0.1:6199", config)
	require.NoError(t, err)
	defer ln.Close()

	conns := make(chan Conn, 2)
	go func() {
		for {
			req, err := ln.Accept2()
			if err != nil {
				return
			}

			conn, err := req.Accept()
			if err != nil {
				return
			}

			conns <- conn
		}
	}()

	dial := func(streamId string) Conn {
		dconfig := DefaultConfig()
		dconfig.StreamId = streamId

		conn, err := Dial("srt", "127.0.0.1:6199", dconfig)
		require.NoError(t, err)

		return conn
	}

	slow := dial("publish:slow")
	defer slow.Close()
	slowConn := <-conns
	defer slowConn.Close()

	fast := dial("publish:fast")
	defer fast.Close()
	fastConn := <-conns
	defer fastConn.Close()

	if slowConn.StreamId() != "publish:slow" {
		slowConn, fastConn = fastConn, slowConn
	}

	// Only the fast connection is read
	var received atomic.Uint64
	go func() {
		buf := make([]byte, 2048)
		for {
			if _, err := fastConn.Read(buf); err != nil {
				return
			}
			received.Add(1)
		}
	}()

	const n = 1000

	var wg sync.WaitGroup
	for _, conn := range []Conn{slow, fast} {
		wg.Add(1)
		go func(conn Conn) {
			defer wg.Done()

			data := make([]byte, 1316)
			for range n {
				conn.Write(data)
				time.Sleep(time.Millisecond)
			}
		}(conn)
	}
	wg.Wait()

	// Wait for the latency to pass
	time.Sleep(500 * time.Millisecond)

	require.Equal(t, uint64(n), received.Load())

	stats := Statistics{}
	slowConn.Stats(&stats)

	// The slow connection kept what it could and dropped the rest
	require.Greater(t, stats.Accumulated.PktQueueDrop, uint64(0))
}
