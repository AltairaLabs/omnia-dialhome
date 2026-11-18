// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package dialhome

import (
	"io"
	"net"
	"sync"
)

// LocalForward is a loopback listener whose connections ride the channel as
// streams of one kind. It lets a runtime point an unmodified HTTP client — a
// session-api or memory-api client — at the channel: set its base URL to
// URL() and every request it makes is carried to the relay, which terminates
// it in-cluster.
type LocalForward struct {
	lis  net.Listener
	c    *Client
	kind StreamKind
	wg   sync.WaitGroup
}

// ForwardLocal starts a LocalForward for kind on 127.0.0.1 (an ephemeral
// port). Loopback only: the forward is this process's door to the channel,
// not a port anything else on the host may use.
func (c *Client) ForwardLocal(kind StreamKind) (*LocalForward, error) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f := &LocalForward{lis: lis, c: c, kind: kind}
	f.wg.Add(1)
	go f.serve()
	return f, nil
}

// URL is the base URL to give an HTTP client.
func (f *LocalForward) URL() string { return "http://" + f.lis.Addr().String() }

// Close stops accepting. Connections already carried finish on their own.
func (f *LocalForward) Close() error {
	err := f.lis.Close()
	f.wg.Wait()
	return err
}

func (f *LocalForward) serve() {
	defer f.wg.Done()
	for {
		conn, err := f.lis.Accept()
		if err != nil {
			return
		}
		go f.carry(conn)
	}
}

func (f *LocalForward) carry(conn net.Conn) {
	st, err := f.c.OpenStream(f.kind)
	if err != nil {
		f.c.log.V(1).Info("dial-home forward dropped", "kind", f.kind.String(), "err", err.Error())
		_ = conn.Close()
		return
	}
	pipe(conn, st)
}

// pipe copies both ways between a and b until either side ends, then closes
// both.
func pipe(a, b net.Conn) {
	var once sync.Once
	closeBoth := func() {
		_ = a.Close()
		_ = b.Close()
	}
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		once.Do(closeBoth)
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}
