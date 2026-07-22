package cli

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"sync"

	"github.com/cklxx/tune/internal/config"
	"github.com/cklxx/tune/internal/sshx"
)

// The hold wire protocol: length-prefixed frames over a Unix socket.
//
//	frame = type (1 byte) | payload length (4 bytes, big endian) | payload
//
// On accept the daemon immediately sends a HELLO frame (JSON). The client
// replies with one REQ frame (JSON) describing the operation, optionally
// followed by STDIN chunks (terminated by STDIN_EOF) and SIGNAL frames.
// The daemon streams STDOUT/STDERR chunks and finishes with one EXIT frame.
const (
	frameReq      byte = 1  // client → daemon: JSON holdRequest
	frameStdin    byte = 2  // client → daemon: stdin chunk
	frameStdinEOF byte = 3  // client → daemon: no more stdin
	frameSignal   byte = 4  // client → daemon: signal name ("INT", "TERM")
	frameHello    byte = 16 // daemon → client: JSON holdHello
	frameStdout   byte = 17 // daemon → client: stdout chunk
	frameStderr   byte = 18 // daemon → client: stderr chunk
	frameExit     byte = 19 // daemon → client: JSON holdExit, ends the op
)

const (
	holdProtoVersion = 1
	maxFramePayload  = 8 << 20
	holdChunkSize    = 32 << 10
)

// holdHello is sent by the daemon on every accepted connection, before the
// client commits to anything. Config identifies all connection-affecting host
// settings so a changed user, jump, auth source, or host-key policy is not
// silently served by an old held connection.
type holdHello struct {
	Version int    `json:"version"`
	Host    string `json:"host"`
	Target  string `json:"target"`
	Config  uint64 `json:"config"`
	Pid     int    `json:"pid"`
	DialMs  int64  `json:"dialMs"` // dial cost paid once, when the hold started
}

func holdConfigID(host *config.Host, policy sshx.HostKeyPolicy) uint64 {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%d\x00%s\x00%s\x00%s\x00%s\x00%s\x00", policy,
		host.Target.Addr, host.Target.User, host.Target.IdentityFile, host.Target.PasswordCmd, host.KnownHosts)
	if host.Jump != nil {
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s", host.Jump.Addr, host.Jump.User, host.Jump.IdentityFile, host.Jump.PasswordCmd)
	}
	return h.Sum64()
}

// holdRequest describes one operation for the daemon to run on the held
// connection. Exactly one Op per socket connection.
type holdRequest struct {
	Op     string   `json:"op"` // exec|read|write|ls|push|pull|info|stop
	Args   []string `json:"args,omitempty"`
	Env    []string `json:"env,omitempty"`
	Cwd    string   `json:"cwd,omitempty"`
	Proxy  bool     `json:"proxy,omitempty"`
	Path   string   `json:"path,omitempty"`   // read/write/ls remote path
	Local  string   `json:"local,omitempty"`  // push/pull local path (absolute)
	Remote string   `json:"remote,omitempty"` // push/pull remote path
	JSON   bool     `json:"json,omitempty"`
	Long   bool     `json:"long,omitempty"`
}

// holdExit terminates an operation: the remote exit code plus any transport
// or SFTP error, stringified.
type holdExit struct {
	Code int    `json:"code"`
	Err  string `json:"err,omitempty"`
}

// holdInfo is the payload of the "info" op (one JSON STDOUT frame).
type holdInfo struct {
	Host        string `json:"host"`
	Target      string `json:"target"`
	HasJump     bool   `json:"hasJump"`
	Pid         int    `json:"pid"`
	DialMs      int64  `json:"dialMs"`
	HeldSince   string `json:"heldSince"`
	OpsServed   uint64 `json:"opsServed"`
	IdleTimeout string `json:"idleTimeout"`
	PingMs      int64  `json:"pingMs"`
	Remote      string `json:"remote,omitempty"`
}

// frameConn wraps a socket with atomic frame writes (stdout/stderr/signal
// writers race from different goroutines) and sequential frame reads.
type frameConn struct {
	mu   sync.Mutex
	conn net.Conn
}

func newFrameConn(c net.Conn) *frameConn { return &frameConn{conn: c} }

func (f *frameConn) writeFrame(t byte, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var hdr [5]byte
	hdr[0] = t
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := f.conn.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := f.conn.Write(payload)
	return err
}

func (f *frameConn) writeJSON(t byte, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return f.writeFrame(t, data)
}

// readFrame reads the next frame. Only one goroutine may read at a time.
func (f *frameConn) readFrame() (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(f.conn, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFramePayload {
		return 0, nil, fmt.Errorf("hold protocol: frame too large (%d bytes)", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(f.conn, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

// stream returns an io.Writer that chunks writes into frames of type t.
func (f *frameConn) stream(t byte) io.Writer { return &frameStream{fc: f, t: t} }

type frameStream struct {
	fc *frameConn
	t  byte
}

func (s *frameStream) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := min(len(p), holdChunkSize)
		if err := s.fc.writeFrame(s.t, p[:n]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}
