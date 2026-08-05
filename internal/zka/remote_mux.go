package zka

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"time"

	"github.com/hashicorp/yamux"
)

// stdioConn gives yamux the net.Conn it expects without inventing another
// framing layer. SSH owns transport liveness and encryption; deadlines are
// enforced by operation contexts and process cancellation.
type stdioConn struct {
	reader io.Reader
	writer io.Writer
	closer []io.Closer
}

func newStdioConn(reader io.Reader, writer io.Writer) net.Conn {
	conn := &stdioConn{reader: reader, writer: writer}
	if closer, ok := reader.(io.Closer); ok {
		conn.closer = append(conn.closer, closer)
	}
	if closer, ok := writer.(io.Closer); ok {
		conn.closer = append(conn.closer, closer)
	}
	return conn
}

func (c *stdioConn) Read(p []byte) (int, error)  { return c.reader.Read(p) }
func (c *stdioConn) Write(p []byte) (int, error) { return c.writer.Write(p) }
func (c *stdioConn) Close() error {
	var result error
	for _, closer := range c.closer {
		result = errors.Join(result, closer.Close())
	}
	return result
}
func (c *stdioConn) LocalAddr() net.Addr              { return remotePipeAddr("local") }
func (c *stdioConn) RemoteAddr() net.Addr             { return remotePipeAddr("remote") }
func (c *stdioConn) SetDeadline(time.Time) error      { return nil }
func (c *stdioConn) SetReadDeadline(time.Time) error  { return nil }
func (c *stdioConn) SetWriteDeadline(time.Time) error { return nil }

type remotePipeAddr string

func (a remotePipeAddr) Network() string { return "ssh-stdio" }
func (a remotePipeAddr) String() string  { return string(a) }

// chunkedWriter prevents a large workspace snapshot from monopolising the
// outer SSH channel in one application write. yamux still provides the stream
// framing and flow control.
type chunkedWriter struct {
	w io.Writer
}

func (w chunkedWriter) Write(p []byte) (int, error) {
	const maximumWrite = 32 << 10
	written := 0
	for len(p) != 0 {
		chunk := p
		if len(chunk) > maximumWrite {
			chunk = chunk[:maximumWrite]
		}
		n, err := w.w.Write(chunk)
		written += n
		p = p[n:]
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func remoteYamuxConfig() *yamux.Config {
	return yamux.DefaultConfig()
}

func runRemoteControlMux(ctx context.Context, paths Paths, stdin io.Reader, stdout io.Writer) error {
	session, err := yamux.Server(newStdioConn(stdin, stdout), remoteYamuxConfig())
	if err != nil {
		return err
	}
	defer session.Close()
	control, err := session.Accept()
	if err != nil {
		return err
	}
	defer control.Close()
	return runRemoteControlSession(ctx, paths, control, control, session, os.Getenv("SSH_CONNECTION"))
}
