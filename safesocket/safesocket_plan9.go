// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build plan9

package safesocket

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/plan9"
)

// Plan 9's devsrv srv(3) is a server registry  and
// it is conventionally bound to "/srv" in the default
// namespace. It is "a one level directory for holding
// already open channels to services". Post one end of
// a pipe to "/srv/tailscale.sock" and use the other
// end for communication with a requestor.

type plan9SrvAddr string

func (sl plan9SrvAddr) Network() string {
	return "/srv"
}

func (sl plan9SrvAddr) String() string {
	return string(sl)
}

// There is no net.FileListener for Plan 9 at this time
type plan9SrvListener struct {
	name string
	file *os.File
}

func (sl *plan9SrvListener) Accept() (net.Conn, error) {
	// sl.file is the server end of the pipe that's
	// connected to /srv/tailscale.sock
	log.Printf("plan9SrvListener.Accept() on %s", sl.name)
	for {
		hello := make([]byte, len("Hello"))
		_, err := sl.file.Read(hello)
		if err != nil {
			return nil, err
		}
		if string(hello) == "Hello" {
			break
		}
		// log.Printf("plan9SrvListener.Accept() looping")
	}
	log.Printf("plan9SrvListener.Accepted")

	return plan9FileConn{name: sl.name, file: sl.file}, nil
}

func (sl *plan9SrvListener) Close() error {
	return sl.file.Close()
}

func (sl *plan9SrvListener) Addr() net.Addr {
	return plan9SrvAddr(sl.name)
}

type plan9FileConn struct {
	name string
	file *os.File
}

func (fc plan9FileConn) Read(b []byte) (n int, err error) {
	return fc.file.Read(b)
}
func (fc plan9FileConn) Write(b []byte) (n int, err error) {
	return fc.file.Write(b)
}
func (fc plan9FileConn) Close() error {
	return fc.file.Close()
}
func (fc plan9FileConn) LocalAddr() net.Addr {
	return plan9SrvAddr(fc.name)
}
func (fc plan9FileConn) RemoteAddr() net.Addr {
	return plan9SrvAddr(fc.name)
}
func (fc plan9FileConn) SetDeadline(t time.Time) error {
	return syscall.EPLAN9
}
func (fc plan9FileConn) SetReadDeadline(t time.Time) error {
	return syscall.EPLAN9
}
func (fc plan9FileConn) SetWriteDeadline(t time.Time) error {
	return syscall.EPLAN9
}

func connect(s *ConnectionStrategy) (net.Conn, error) {
	log.Printf("safesocket_plan9.connect(%s)", s.path)
	f, err := os.OpenFile(s.path, os.O_RDWR, 0666)
	if err != nil {
		return nil, err
	}
	n, err := f.Write([]byte("Hello"))
	if err != nil {
		return nil, err
	}
	if n != len("Hello") {
		return nil, errors.New("Bad handshake")
	}
	log.Printf("safesocket_plan9.connected")

	return plan9FileConn{name: s.path, file: f}, nil
}

func listen(path string) (net.Listener, error) {
	file, err := createSrv(path)
	if err != nil {
		return nil, err
	}
	return &plan9SrvListener{name: path, file: file}, nil
}

// Create an entry in /srv, open a pipe, write the
// client end to the entry and return the server
// end of the pipe to the caller. When the server
// end of the pipe is closed, /srv name associated
// with it will be removed (controlled by ORCLOSE flag)
func createSrv(path string) (*os.File, error) {
	const O_RCLOSE = 64 // remove on close; should be in plan9 package
	var pip [2]int

	err := plan9.Pipe(pip[:])
	if err != nil {
		return nil, err
	}
	defer plan9.Close(pip[1])

	srvfd, err := plan9.Create(path, plan9.O_WRONLY|plan9.O_CLOEXEC|O_RCLOSE, 0600)
	if err != nil {
		return nil, err
	}
	srv := os.NewFile(uintptr(srvfd), path)
	defer srv.Close()

	_, err = fmt.Fprintf(srv, "%d", pip[1])
	if err != nil {
		return nil, err
	}

	return os.NewFile(uintptr(pip[0]), path), nil
}
