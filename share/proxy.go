package chshare

import (
	"context"
	"fmt"
	"io"
	"net"
	"runtime"
	"syscall"

	"github.com/jpillora/sizestr"
	"golang.org/x/crypto/ssh"
)

type GetSSHConn func() ssh.Conn

type TCPProxy struct {
	*Logger
	ssh    GetSSHConn
	id     int
	count  int
	remote *Remote
}

func NewTCPProxy(logger *Logger, ssh GetSSHConn, index int, remote *Remote) *TCPProxy {
	id := index + 1
	return &TCPProxy{
		Logger: logger.Fork("proxy#%d:%s", id, remote),
		ssh:    ssh,
		id:     id,
		remote: remote,
	}
}

func (p *TCPProxy) Start(ctx context.Context) error {
	protocol := "tcp4"
	remote := p.remote.Remote()
	if p.remote.LocalHost == "unix" {
		protocol = "unix"
		remote = p.remote.LocalPort
	}
	netConfig := &net.ListenConfig{Control: p.reusePort}
	l, err := netConfig.Listen(ctx, protocol, remote)
	if err != nil {
		return fmt.Errorf("%s: %s", p.Logger.Prefix(), err)
	}
	go p.listen(ctx, l)
	return nil
}

func (p *TCPProxy) reusePort(network, address string, conn syscall.RawConn) error {
	return conn.Control(func(descriptor uintptr) {
		if !p.remote.Reverse {
			return
		}
		switch runtime.GOOS {
		case "darwin":
			syscall.SetsockoptInt(int(descriptor), syscall.SOL_SOCKET, 0x200 /* syscall.SO_REUSEPORT */, 1)
		case "linux":
			syscall.SetsockoptInt(int(descriptor), syscall.SOL_SOCKET, 0x0F, 1)
		}
	})
}

func (p *TCPProxy) listen(ctx context.Context, l net.Listener) {
	p.Infof("Listening")
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			l.Close()
			p.Infof("Closed")
		case <-done:
		}
	}()
	for {
		src, err := l.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				//listener closed
			default:
				p.Infof("Accept error: %s", err)
			}
			close(done)
			return
		}
		go p.accept(src)
	}
}

func (p *TCPProxy) accept(src io.ReadWriteCloser) {
	defer src.Close()
	p.count++
	cid := p.count
	l := p.Fork("conn#%d", cid)
	l.Debugf("Open")
	sshConn := p.ssh()
	if sshConn == nil {
		l.Debugf("No remote connection")
		return
	}
	//ssh request for tcp connection for this proxy's remote
	dst, reqs, err := sshConn.OpenChannel("chisel", []byte(p.remote.Remote()))
	if err != nil {
		l.Infof("Stream error: %s", err)
		return
	}
	go ssh.DiscardRequests(reqs)
	//then pipe
	s, r := Pipe(src, dst)
	l.Debugf("Close (sent %s received %s)", sizestr.ToString(s), sizestr.ToString(r))
}
