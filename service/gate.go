package service

import (
	"bufio"
	"cham/cham"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"sync/atomic"
)

const (
	GATE_OPEN uint8 = iota
)

var (
	bufioReaderPool sync.Pool
	bufioWriterPool sync.Pool
	GATES           map[cham.Address]*Gate
)

type ClientMsg struct {
	session uint32
	data    []byte
}

type Conf struct {
	address   string //127.0.0.1:8000
	maxclient uint32 // 0 -> no limit
}

type Gate struct {
	rwmutex   *sync.RWMutex
	source    cham.Address
	clinetnum uint32
	maxclient uint32
	quit      chan struct{}
	sessions  map[uint32]*Session
}

type Session struct {
	sessionid uint32
	conn      net.Conn
	brw       *bufio.ReadWriter
}

func (s *Session) Close() {
	putBufioReader(s.brw.Reader)
	putBufioWriter(s.brw.Writer)
	s.conn.Close()
}

func (s *Session) Write(data []byte) {
	s.brw.Write(data)
	s.brw.Flush()
}

func NewConf(address string, maxclient uint32) *Conf {
	return &Conf{address, maxclient}
}

func NewClientMsg(session uint32, data []byte) *ClientMsg {
	return &ClientMsg{session, data}
}

func newSession(sessionid uint32, conn net.Conn) *Session {
	br := newBufioReader(conn)
	bw := newBufioWriter(conn)
	return &Session{sessionid, conn, bufio.NewReadWriter(br, bw)}
}

func (s *Session) ReadFull(buf []byte) error {
	if _, err := io.ReadFull(s.brw, buf); err != nil {
		if e, ok := err.(net.Error); ok && !e.Temporary() {
			return e
		}
	}
	return nil
}

func newGate(source cham.Address) *Gate {
	gate := new(Gate)
	gate.rwmutex = new(sync.RWMutex)
	gate.source = source
	gate.clinetnum = 0
	gate.quit = make(chan struct{})
	gate.sessions = make(map[uint32]*Session)
	return gate
}

func newBufioReader(r io.Reader) *bufio.Reader {
	if v := bufioReaderPool.Get(); v != nil {
		br := v.(*bufio.Reader)
		br.Reset(r)
		return br
	}
	return bufio.NewReader(r)
}

func putBufioReader(r *bufio.Reader) {
	r.Reset(nil)
	bufioReaderPool.Put(r)
}

func newBufioWriter(w io.Writer) *bufio.Writer {
	if v := bufioWriterPool.Get(); v != nil {
		bw := v.(*bufio.Writer)
		bw.Reset(w)
		return bw
	}
	return bufio.NewWriter(w)
}

func putBufioWriter(w *bufio.Writer) {
	w.Reset(nil)
	bufioWriterPool.Put(w)
}

//gate listen
func (g *Gate) open(conf *Conf) bool {
	listen, err := net.Listen("tcp", conf.address)
	if err != nil {
		panic("gate open error:" + err.Error())
	}
	g.maxclient = conf.maxclient
	go g.start(listen)

	return true
}

func (g *Gate) close() {
	close(g.quit)
}

func (g *Gate) start(listen net.Listener) {
	defer listen.Close()
	var sessionId uint32 = 0
	for {
		select {
		case <-g.quit:
			return
		default:
			conn, err := listen.Accept()
			if err != nil {
				continue
			}
			if g.maxclient != 0 && g.clinetnum >= g.maxclient {
				conn.Close()
			}
			g.clinetnum++
			sid := atomic.AddUint32(&sessionId, 1)
			session := newSession(sid, conn)
			g.rwmutex.Lock()
			g.sessions[sid] = session
			g.rwmutex.Unlock()
			go g.serve(session)
		}
	}
}

// bigendian 2byte length+data
func (g *Gate) serve(session *Session) {
	head := make([]byte, 2)
	dest := g.source.GetService()
	for {
		if err := session.ReadFull(head); err != nil {
			g.closeSession(session)
			return
		}

		length := binary.BigEndian.Uint16(head)
		data := make([]byte, length, length)

		if err := session.ReadFull(data); err != nil {
			g.closeSession(session)
			return
		}
		msg := cham.NewMsg(0, 0, cham.PTYPE_CLIENT, NewClientMsg(session.sessionid, data))
		dest.Push(msg)
	}
}

func (g *Gate) closeSession(s *Session) {
	g.rwmutex.Lock()
	delete(g.sessions, s.sessionid)
	g.rwmutex.Unlock()
	s.Close()
}

func (g *Gate) kick(sessionid uint32) {
	var session *Session
	var ok bool
	g.rwmutex.Lock()
	if session, ok = g.sessions[sessionid]; ok {
		delete(g.sessions, sessionid)
	}
	g.rwmutex.Unlock()
	if ok {
		session.Close()
	}
}

func (g *Gate) Write(msg *ClientMsg) {
	g.rwmutex.RLock()
	session, ok := g.sessions[msg.session]
	g.rwmutex.RUnlock()
	if ok {
		session.Write(msg.data)
	}
}

func GateResponseDispatch(service *cham.Service, session int32, source cham.Address, ptype uint8, args ...interface{}) []interface{} {
	msg := args[0].(*ClientMsg)
	gate := GATES[source]
	gate.Write(msg)
	return cham.NORET
}

func GateDispatch(service *cham.Service, session int32, source cham.Address, ptype uint8, args ...interface{}) []interface{} {
	gate, ok := GATES[source]
	if !ok {
		service.RegisterProtocol(cham.PTYPE_RESPONSE, GateResponseDispatch)
		gate = newGate(source)
		GATES[source] = gate
	}

	return cham.NORET
}

//may multi gate
func init() {
	GATES = make(map[cham.Address]*Gate, 1)
}
