package srt

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDefaultConfig(t *testing.T) {
	config := DefaultConfig()
	err := config.Validate()

	if err != nil {
		require.NoError(t, err, "Failed to verify the default configuration: %s", err)
	}
}

func TestMarshalUnmarshal(t *testing.T) {
	wantConfig := Config{
		Congestion:            "xxx",
		ConnectionTimeout:     42 * time.Second,
		DriftTracer:           false,
		EnforcedEncryption:    false,
		FC:                    42,
		GroupConnect:          true,
		GroupStabilityTimeout: 42 * time.Second,
		InputBW:               42,
		IPTOS:                 42,
		IPTTL:                 42,
		IPv6Only:              42,
		KMPreAnnounce:         42,
		KMRefreshRate:         42,
		Latency:               42 * time.Second,
		LossMaxTTL:            42,
		MaxBW:                 42,
		MessageAPI:            true,
		MinInputBW:            42,
		MSS:                   42,
		NAKReport:             false,
		OverheadBW:            42,
		PacketFilter:          "FEC",
		Passphrase:            "foobar",
		PayloadSize:           42,
		PBKeylen:              42,
		PeerIdleTimeout:       42 * time.Second,
		PeerLatency:           42 * time.Second,
		ReceiverBufferSize:    42,
		ReceiverLatency:       42 * time.Second,
		SendBufferSize:        42,
		SendDropDelay:         42 * time.Second,
		StreamId:              "foobaz",
		TooLatePacketDrop:     false,
		TransmissionType:      "yyy",
		TSBPDMode:             false,
		Logger:                nil,
	}

	url := wantConfig.MarshalURL("localhost:6000")

	config := Config{}
	config.UnmarshalURL(url)

	require.Equal(t, wantConfig, config)
}

func TestQueueSizeConfig(t *testing.T) {
	// Test default queue sizes
	config := DefaultConfig()
	require.Equal(t, 1024, config.ReadQueueSize)
	require.Equal(t, 1024, config.NetworkQueueSize)
	require.Equal(t, 1024, config.WriteQueueSize)
	require.Equal(t, false, config.BlockOnFullQueue)

	// Test custom queue sizes via URL
	url := "srt://localhost:6000?readqueuesize=8192&networkqueuesize=2048&writequeuesize=4096&blockonfullqueue=true"
	config = Config{}
	_, err := config.UnmarshalURL(url)
	require.NoError(t, err)
	
	require.Equal(t, 8192, config.ReadQueueSize)
	require.Equal(t, 2048, config.NetworkQueueSize)
	require.Equal(t, 4096, config.WriteQueueSize)
	require.Equal(t, true, config.BlockOnFullQueue)

	// Test marshaling back to URL
	marshaledURL := config.MarshalURL("localhost:6000")
	config2 := Config{}
	_, err = config2.UnmarshalURL(marshaledURL)
	require.NoError(t, err)
	
	require.Equal(t, config.ReadQueueSize, config2.ReadQueueSize)
	require.Equal(t, config.NetworkQueueSize, config2.NetworkQueueSize)
	require.Equal(t, config.WriteQueueSize, config2.WriteQueueSize)
	require.Equal(t, config.BlockOnFullQueue, config2.BlockOnFullQueue)
}

func TestQueueSizeConfigVariants(t *testing.T) {
	// Test with boolean variants
	tests := []struct {
		url      string
		expected bool
	}{
		{"srt://localhost:6000?blockonfullqueue=yes", true},
		{"srt://localhost:6000?blockonfullqueue=on", true},
		{"srt://localhost:6000?blockonfullqueue=1", true},
		{"srt://localhost:6000?blockonfullqueue=no", false},
		{"srt://localhost:6000?blockonfullqueue=off", false},
		{"srt://localhost:6000?blockonfullqueue=0", false},
	}

	for _, tt := range tests {
		config := Config{}
		_, err := config.UnmarshalURL(tt.url)
		require.NoError(t, err)
		require.Equal(t, tt.expected, config.BlockOnFullQueue, "URL: %s", tt.url)
	}
}

