package chshare

import (
	"context"
	"fmt"
	"io"
	"net"

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
	clientChannel := ctx.Value(p.remote.Remote())
	if clientChannel != nil {
		go p.listenX(ctx, clientChannel.(chan io.ReadWriteCloser))
		return nil
	}
	l, err := net.Listen("tcp4", p.remote.LocalHost+":"+p.remote.LocalPort)
	if err != nil {
		return fmt.Errorf("%s: %s", p.Logger.Prefix(), err)
	}
	go p.listen(ctx, l)
	return nil
}

func (p *TCPProxy) listenX(ctx context.Context, clientChannel chan io.ReadWriteCloser) {
	for {
		select {
		case clientStream := <-clientChannel:
			go p.accept(clientStream)
		case <-ctx.Done():
			close(clientChannel)
			p.Infof("Finished")
			return
		}
	}
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
