package srt

import "fmt"

// SRTErrorType classifies the type of SRT error
type SRTErrorType int

const (
	ErrTypeUnknown SRTErrorType = iota
	ErrTypeTimeout
	ErrTypeHandshakeFailed
	ErrTypeConnectionRejected
	ErrTypeVersionMismatch
	ErrTypeCryptoError
	ErrTypeConnectionClosed
)

// SRTError provides structured error information for SRT connections
type SRTError struct {
	Type    SRTErrorType
	Message string
	Reason  string // Rejection reason from server (if applicable)
	Cause   error  // Underlying error
}

func (e *SRTError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("srt: %s: %s", e.Message, e.Reason)
	}
	return fmt.Sprintf("srt: %s", e.Message)
}

func (e *SRTError) Unwrap() error {
	return e.Cause
}

// NewTimeoutError creates a new timeout error
func NewTimeoutError(msg string) *SRTError {
	return &SRTError{Type: ErrTypeTimeout, Message: msg}
}

// NewRejectionError creates a new rejection error
func NewRejectionError(reason string) *SRTError {
	return &SRTError{Type: ErrTypeConnectionRejected, Message: "connection rejected", Reason: reason}
}

// NewHandshakeError creates a new handshake error
func NewHandshakeError(msg string, cause error) *SRTError {
	return &SRTError{Type: ErrTypeHandshakeFailed, Message: msg, Cause: cause}
}

// NewVersionMismatchError creates a new version mismatch error
func NewVersionMismatchError(msg string) *SRTError {
	return &SRTError{Type: ErrTypeVersionMismatch, Message: msg}
}

// NewCryptoError creates a new crypto error
func NewCryptoError(msg string, cause error) *SRTError {
	return &SRTError{Type: ErrTypeCryptoError, Message: msg, Cause: cause}
}
