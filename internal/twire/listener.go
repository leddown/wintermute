package twire

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"
)

const (
	// connDeadline bounds how long a single canary connection is serviced.
	connDeadline = 5 * time.Second
	// maxPreviewBytes caps how much of the client's first send we read and
	// store as a preview.
	maxPreviewBytes = 256
	// minAcceptBackoff and maxAcceptBackoff bound the retry pause after a
	// transient Accept failure — long enough that a persistent condition
	// doesn't spin the CPU or the log, short enough that recovery is quick.
	minAcceptBackoff = 5 * time.Millisecond
	maxAcceptBackoff = time.Second
)

// startListenerLocked opens the TCP listener for one canary and starts its
// accept loop. A bind failure (port in use, permission denied) is recorded
// in bindErrs and logged, not returned — the canary simply shows as
// not-listening with an error. The caller must hold s.mu.
func (s *Service) startListenerLocked(key string) {
	profile, ok := s.byKey[key]
	if !ok {
		// Fall back to the operator-defined canaries (caller holds s.mu).
		if profile, ok = s.customByKey[key]; !ok {
			return
		}
	}
	if _, running := s.listeners[key]; running {
		return
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", profile.Port))
	if err != nil {
		s.bindErrs[key] = err.Error()
		s.log.Warn("twire: canary could not bind",
			"canary", key, "port", profile.Port, "error", err)
		return
	}
	delete(s.bindErrs, key)
	s.listeners[key] = ln
	s.log.Info("twire: canary listening",
		"canary", key, "service", profile.Name, "port", profile.Port)
	go s.acceptLoop(profile, ln)
}

// acceptLoop services connections until the listener is closed.
//
// Only a closed listener ends the loop. Any other Accept error is treated as
// transient and retried after a growing pause, mirroring what net/http's own
// Serve does. Returning on every error instead would mean a momentary
// condition — descriptor exhaustion is the usual one — permanently stops
// this canary listening, while Status keeps reporting it as up because its
// entry is still in s.listeners. A canary that has silently stopped watching
// is worse than one that is plainly down.
func (s *Service) acceptLoop(profile ServiceProfile, ln net.Listener) {
	var backoff time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return // Disable or shutdown closed it
			}
			if backoff == 0 {
				backoff = minAcceptBackoff
			} else {
				backoff *= 2
			}
			if backoff > maxAcceptBackoff {
				backoff = maxAcceptBackoff
			}
			s.log.Warn("twire: canary accept error, retrying",
				"canary", profile.Key, "retry_in", backoff, "error", err)
			time.Sleep(backoff)
			continue
		}
		backoff = 0
		go s.handleConn(profile, conn)
	}
}

// handleConn greets the client with the profile's banner (if any), reads a
// small preview of whatever it sends, records the hit, then closes. It
// never reads more than maxPreviewBytes and is bounded by connDeadline, so
// a slow or chatty client can't tie up resources.
func (s *Service) handleConn(profile ServiceProfile, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(connDeadline))

	remoteIP, remotePort := splitHostPort(conn.RemoteAddr())

	if profile.Banner != "" {
		_, _ = conn.Write([]byte(profile.Banner))
	}

	buf := make([]byte, maxPreviewBytes)
	n, _ := conn.Read(buf)
	s.recordHit(profile, remoteIP, remotePort, sanitizePreview(buf[:n]))
}

// splitHostPort breaks a net.Addr into an IP string and numeric port,
// tolerating malformed values.
func splitHostPort(addr net.Addr) (string, int) {
	host, portStr, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String(), 0
	}
	port, _ := strconv.Atoi(portStr)
	return host, port
}

// sanitizePreview renders the client's first bytes as a safe, bounded
// printable-ASCII string (non-printable bytes become '.'), so it is safe to
// store and later display.
func sanitizePreview(b []byte) string {
	out := make([]rune, 0, len(b))
	for _, c := range b {
		if c >= 0x20 && c < 0x7f {
			out = append(out, rune(c))
		} else {
			out = append(out, '.')
		}
	}
	return string(out)
}
