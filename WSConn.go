package fws

import (
	"net"
	"sync"
	"time"

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
	mutex         sync.Mutex
	peerWG        *sync.WaitGroup
	state         ws.State
	peerID        uint64
	portObj       *SWSPort
	Reader        *wsutil.Reader
	sender        *SSender
	connState     ConnState
	eventCallback IPeerCallback
	// 最後一次收到遠端資料的時間，用來做timeout檢測
	// 1. lastRecvTime 只在接收資料時更新
	// 2. 當lastRecvTime閒置超過keepalive容許後會送出ping強制客端回應，並設定timeoutCheck
	// 3. 當timeoutCheck != 0 且timeoutCheck超過timeout容許值時認定為斷線
	lastRecvTime time.Time
	timeoutCheck int64
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
	wsConn.mutex.Lock()
	if wsConn.connState == Connected {
		wsConn.sender.Send(p, ws.OpBinary)
	}
	wsConn.mutex.Unlock()
}
func (wsConn *sWSConnection) SendText(s string) {
	wsConn.mutex.Lock()
	if wsConn.connState == Connected {
		wsConn.sender.Send([]byte(s), ws.OpText)
	}
	wsConn.mutex.Unlock()
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
				// log := flog.NewLog(flog.LOGLEVELError, "").AddPanicCallstack(0, ".(*sWSConnection).Close")
				log := flog.Panic(flog.LOGLEVELError, ".(*sWSConnection).Close", "")
				port.publisher.Publish(log.SetCaption("%s() 發生panic, %v", log.GetFunctionName(), err))
			}
		}
		if pWG != nil {
			pWG.Done()
		}
	}()

	wsConn.mutex.Lock()
	switch wsConn.connState {
	case Closed, Closing:
		wsConn.mutex.Unlock()
		return
	default:
		pWG = wsConn.peerWG
		wsConn.connState = Closed
		wsConn.connection.Close()
		wsConn.mutex.Unlock()
		wsConn.portObj.Lock()
		delete(wsConn.portObj.wsConnections, wsConn.peerID)
		wsConn.portObj.Unlock()
		if wsConn.eventCallback != nil {
			wsConn.eventCallback.OnClosed(wsConn, errFromSys, errFromProc)
		}
	}
}
