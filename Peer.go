package fws

import (
	"net"
	flog "server/fLogSystem"
	"sync"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

type ConnState uint8

const (
	NoConnected ConnState = iota
	Connected
	Closing
	Closed
)

type ICorePeer interface {
	GetPeerID() uint64
	GetPortObj() *SPort
	GetInterface() interface{}
	Send(p []byte)
	SendText(s string)
	Close(errFromSys error, errFromProc error)
}

type IPeerCallback interface {
	GetInterface() interface{}
	OnRecv(corePeer ICorePeer, data []byte, dataSize int64, isText bool) // 這邊仍是處於併發狀態，請注意同步問題
	OnClosed(corePeer ICorePeer, errFromSys error, errFromProc error)
	// OnPing(corePeer ICorePeer)
	// OnPong(corePeer ICorePeer)
}

type sCorePeer struct {
	connection    net.Conn
	peerWG        *sync.WaitGroup
	state         ws.State
	peerID        uint64
	portObj       *SPort
	Reader        *wsutil.Reader
	sender        *SSender
	connState     ConnState
	eventCallback IPeerCallback
}

func (speer *sCorePeer) GetPeerID() uint64 {
	return speer.peerID
}
func (speer *sCorePeer) GetPortObj() *SPort {
	return speer.portObj
}
func (speer *sCorePeer) GetInterface() interface{} {
	return speer.eventCallback.GetInterface()
}
func (speer *sCorePeer) Send(p []byte) {
	if speer.connState != Connected {
		return
	}
	speer.sender.Send(p, ws.OpBinary)
}
func (speer *sCorePeer) SendText(s string) {
	if speer.connState != Connected {
		return
	}
	speer.sender.Send([]byte(s), ws.OpText)
}
func (speer *sCorePeer) Close(errFromSys error, errFromProc error) {
	if speer == nil {
		return
	}
	defer func() {
		if err := recover(); err != nil {
			port := speer.GetPortObj()
			if (port != nil) && (port.publisher != nil) {
				port.publisher.Publish(flog.NewLog(flog.LOGLEVELError, "程式在(*sCorePeer).Close()發生panic: %v", err).AddPanicCallstack(0, FUNCNAME_RUN_AS_WE))
			}
			return
		}
	}()
	if (speer.connState != Closed) && (speer.connState != Closing) {
		speer.connState = Closed
		speer.connection.Close()
		speer.portObj.Lock()
		delete(speer.portObj.peers, speer.peerID)
		speer.portObj.Unlock()
		if speer.eventCallback != nil {
			speer.eventCallback.OnClosed(speer, errFromSys, errFromProc)
		}
		speer.peerWG.Done()
	}
}
