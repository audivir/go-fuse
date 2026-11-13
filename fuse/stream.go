// Copyright 2026 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fuse

import (
	"fmt"
	"sync"
	"syscall"
	"unsafe"
)

// fuseTStream is set when the FUSE fd is a FUSE-T SOCK_STREAM socket
// rather than a message-framed device. Requests must then be read as
// length-prefixed frames, and replies must not interleave.
var fuseTStream bool

// streamReadMu and streamWriteMu serialize I/O on a FUSE-T stream
// socket, where concurrent readers or writers could split or
// interleave frames.
var streamReadMu, streamWriteMu sync.Mutex

// readFull reads exactly len(dest) bytes from fd.
func readFull(fd int, dest []byte) error {
	for off := 0; off < len(dest); {
		n, err := syscall.Read(fd, dest[off:])
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			// Peer closed the socket.
			return syscall.ENODEV
		}
		off += n
	}
	return nil
}

// readFrame reads one request from fd. On a message-framed device it
// is a single read(2); on a FUSE-T stream it reads the uint32 length
// that starts every InHeader, then the rest of the request.
func readFrame(fd int, dest []byte) (int, error) {
	if !fuseTStream {
		return syscall.Read(fd, dest)
	}
	streamReadMu.Lock()
	defer streamReadMu.Unlock()

	if err := readFull(fd, dest[:4]); err != nil {
		return 0, err
	}
	l := int(*(*uint32)(unsafe.Pointer(&dest[0])))
	if l < int(unsafe.Sizeof(InHeader{})) || l > len(dest) {
		return 0, fmt.Errorf("FUSE-T: bad request length %d (buffer %d)", l, len(dest))
	}
	if err := readFull(fd, dest[4:l]); err != nil {
		return 0, err
	}
	return l, nil
}

// writevFull writes all of iov to a stream socket. A blocking writev
// on a stream socket may return a short count when a signal (e.g. the
// Go runtime's SIGURG preemption) arrives mid-transfer; unlike a FUSE
// device, the remainder must still be sent or the frame is truncated
// and the peer stalls waiting for it.
func writevFull(fd int, iov [][]byte) (int, error) {
	total := 0
	for _, b := range iov {
		total += len(b)
	}
	// Work on a copy so the caller's slice headers stay intact.
	rest := make([][]byte, 0, len(iov))
	for _, b := range iov {
		if len(b) > 0 {
			rest = append(rest, b)
		}
	}
	written := 0
	for len(rest) > 0 {
		n, err := writev(fd, rest)
		if err == syscall.EINTR || err == syscall.EAGAIN {
			continue
		}
		if err != nil {
			return written, err
		}
		written += n
		for n > 0 && len(rest) > 0 {
			if n >= len(rest[0]) {
				n -= len(rest[0])
				rest = rest[1:]
			} else {
				rest[0] = rest[0][n:]
				n = 0
			}
		}
	}
	if written != total {
		return written, fmt.Errorf("FUSE-T: short write %d/%d", written, total)
	}
	return written, nil
}
