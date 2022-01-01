package fws

import (
	"net"
	"sync"

	flog "github.com/farus422/fLogSystem"
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

type IWSConnection interface {
	GetPeerID() uint64
	GetPortObj() *SWSPort
	GetInterface() interface{}
	Send(p []byte)
	SendText(s string)
	Close(errFromSys error, errFromProc error)
}

type IPeerCallback interface {
	GetInterface() interface{}
	OnRecv(wsConn IWSConnection, data []byte, dataSize int64, isText bool) // 這邊仍是處於併發狀態，請注意同步問題
	OnClosed(wsConn IWSConnection, errFromSys error, errFromProc error)
	// OnPing(wsConn IWSConnection)
	// OnPong(wsConn IWSConnection)
}

type sWSConnection struct {
	connection    net.Conn
	peerWG        *sync.WaitGroup
	state         ws.State
	peerID        uint64
	portObj       *SWSPort
	Reader        *wsutil.Reader
	sender        *SSender
	connState     ConnState
	eventCallback IPeerCallback
}

func (wsConn *sWSConnection) GetPeerID() uint64 {
	return wsConn.peerID
}
func (wsConn *sWSConnection) GetPortObj() *SWSPort {
	return wsConn.portObj
}
func (wsConn *sWSConnection) GetInterface() interface{} {
	return wsConn.eventCallback.GetInterface()
}
func (wsConn *sWSConnection) Send(p []byte) {
	if wsConn.connState != Connected {
		return
	}
	wsConn.sender.Send(p, ws.OpBinary)
}
func (wsConn *sWSConnection) SendText(s string) {
	if wsConn.connState != Connected {
		return
	}
	wsConn.sender.Send([]byte(s), ws.OpText)
}
func (wsConn *sWSConnection) Close(errFromSys error, errFromProc error) {
	if wsConn == nil {
		return
	}
	var pWG *sync.WaitGroup = nil
	defer func() {
		if err := recover(); err != nil {
			port := wsConn.GetPortObj()
			if (port != nil) && (port.publisher != nil) {
				log := flog.NewLog(flog.LOGLEVELError, "").AddPanicCallstack(0, ".(*sWSConnection).Close")
				port.publisher.Publish(log.SetCaption("%s() 發生panic, %v", log.GetFunctionName(), err))
			}
		}
		if pWG != nil {
			pWG.Done()
		}
	}()

	if (wsConn.connState != Closed) && (wsConn.connState != Closing) {
		pWG = wsConn.peerWG
		wsConn.connState = Closed
		wsConn.connection.Close()
		wsConn.portObj.Lock()
		delete(wsConn.portObj.wsConnections, wsConn.peerID)
		wsConn.portObj.Unlock()
		if wsConn.eventCallback != nil {
			wsConn.eventCallback.OnClosed(wsConn, errFromSys, errFromProc)
		}
	}
}
