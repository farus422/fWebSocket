package fws

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	fcb "github.com/farus422/fCallstack"
	flog "github.com/farus422/fLogSystem"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

type FOnAccept = func(IWSConnection) IPeerCallback

var ERR_SERVER_SHUTDOWN = errors.New("Server shutdown")

type SWSPort struct {
	listener      net.Listener
	mutex         sync.Mutex
	ctx           context.Context
	cancel        context.CancelFunc
	serverWG      *sync.WaitGroup
	peerWG        sync.WaitGroup
	wsConnections map[uint64]IWSConnection
	publisher     *flog.SPublisher
	connCount     uint64
	keepalive     time.Duration
	timeout       int64
}

var NilPayload []byte = make([]byte, 1)

func (port *SWSPort) ListenAndServe(portNo int, fOnAccept FOnAccept) error {
	port.serverWG.Add(1)
	if port.listener != nil {
		port.Unlisten()
	}
	ln, listen_err := net.Listen("tcp", fmt.Sprintf(":%d", portNo))
	if listen_err != nil {
		errMsg := fmt.Sprintf("Failed to listen to port %d. err=%v", portNo, listen_err)
		if port.publisher != nil {
			port.publisher.Publish(flog.Error(errMsg))
		}
		port.serverWG.Done()
		return errors.New(errMsg)
	}
	port.listener = ln

	peerWG := &port.peerWG
	go func() {
		defer func() {
			if port.listener != nil {
				port.listener = nil
				ln.Close()
			}
			port.serverWG.Done()
		}()

		u := ws.Upgrader{
			// OnHost: func(host []byte) error {
			// 	fmt.Printf("host: %s\n", host)
			// 	return nil
			// },
			// OnHeader: func(key, value []byte) (err error) {
			// 	fmt.Printf("header: %q=%q\n", key, value)
			// 	return nil
			// },
		}
		for {
			conn, err := ln.Accept()
			if err != nil {
				// handle error
				// fmt.Printf("<< Accept fail >> %v\n", err)
				// select {
				// case <-ctx.Done():
				// 	peerWG.Wait()
				// }
				return
			} else {
				peerWG.Add(1)
				go port.runAsWebSocket(conn, u, fOnAccept, peerWG)
			}
		}
	}()

	go func() {
		ticker := time.NewTicker(time.Second)
		var currTime time.Time
		var currMilli int64
		var wsConn *sWSConnection
		var wsConns []*sWSConnection
		var err error
		for {
			select {
			case _, ok := <-ticker.C: // 定時檢查
				if !ok {
					return
				}
				// GetConns()
				port.mutex.Lock()
				i := 0
				wsConns = make([]*sWSConnection, len(port.wsConnections))
				conns := port.wsConnections
				for _, conn := range conns {
					wsConns[i] = conn.(*sWSConnection)
					i++
				}
				port.mutex.Unlock()

				currTime = time.Now()
				for _, wsConn = range wsConns {
					err = nil
					wsConn.mutex.Lock()
					if wsConn.connState == Connected {
						if wsConn.timeoutCheck > 0 {
							currMilli = currTime.UnixMilli()
							if currMilli > wsConn.timeoutCheck && currMilli-wsConn.timeoutCheck >= port.timeout {
								err = errors.New("WebSocket: client timeout!!")
								if port.publisher != nil {
									port.publisher.Publish(flog.Error("WebSocket: client timeout!!"))
								}
							}
						} else {
							if currTime.After(wsConn.lastRecvTime) && currTime.Sub(wsConn.lastRecvTime) >= port.keepalive {
								wsConn.timeoutCheck = wsConn.lastRecvTime.UnixMilli()
								// 發送Ping
								frame := ws.NewPongFrame(NilPayload)
								if err = ws.WriteHeader(wsConn, frame.Header); err != nil {
									if port.publisher != nil {
										port.publisher.Publish(flog.Error("ws.WriteHeader() err : %v", err))
									}
								}
							}
						}
					}
					wsConn.mutex.Unlock()
					if err != nil {
						wsConn.Close(err, nil)
					}
				}
			}
		}
	}()
	return nil
}

func (port *SWSPort) Unlisten() {
	if port.listener != nil {
		ln := port.listener
		port.listener = nil
		ln.Close()
	}
}

func (port *SWSPort) GetPublisher() *flog.SPublisher {
	return port.publisher
}

type FWSSCANCALLBACK = func(IWSConnection) bool

// keepalive: 單位(秒)
// timeout:   單位(秒)
func (port *SWSPort) SetTimeout(keepalive, timeout int64) {
	port.keepalive = time.Second * time.Duration(keepalive)
	port.timeout = timeout * 1000 // 秒 -> ms
}

func (port *SWSPort) GetConns() []IWSConnection {
	i := 0
	port.mutex.Lock()
	wsConns := make([]IWSConnection, len(port.wsConnections))
	for _, conn := range port.wsConnections {
		wsConns[i] = conn
		i++
	}
	port.mutex.Unlock()
	return wsConns
}

func (port *SWSPort) GetPeersAndLock() map[uint64]IWSConnection {
	port.mutex.Lock()
	return port.wsConnections
}

func (port *SWSPort) GetPeer(peerID uint64) IWSConnection {
	port.mutex.Lock()
	p := port.wsConnections[peerID]
	port.mutex.Unlock()
	return p
}

func (port *SWSPort) Lock() {
	port.mutex.Lock()
}

func (port *SWSPort) Unlock() {
	port.mutex.Unlock()
}

func (port *SWSPort) CloseAllPeer() {
	for _, wsConn := range port.wsConnections {
		wsConn.Close(ERR_SERVER_SHUTDOWN, nil)
	}
}

func (port *SWSPort) WaitForAllPeerClosed() {
	port.peerWG.Wait()
}

func (port *SWSPort) Shutdown() {
	port.Unlisten()
	port.CloseAllPeer()
	port.WaitForAllPeerClosed()
}

const FUNCNAME_RUN_AS_WE = ".(*SWSPort).runAsWebSocket"

func (port *SWSPort) runAsWebSocket(conn net.Conn, u ws.Upgrader, fOnAccept FOnAccept, peerwg *sync.WaitGroup) {
	var tWSConn *sWSConnection = nil
	defer func() {
		if err := recover(); err != nil {
			// csp := fcb.SCallstack{}
			// csp.GetCallstackWithPanic(0, "")
			// fmt.Println("\nCallstacks(Panic):")
			// csp.Print()

			// log := flog.NewLog(flog.LOGLEVELError, "\nCallstacks of log:").AddCallstack(0, FUNCNAME_RUN_AS_WE)
			// fmt.Println(log.Message())
			// callers := log.Callstack()
			// if callers != nil {
			// 	for _, caller := range callers {
			// 		fmt.Printf("%s:%d %s()\n", caller.File, caller.Line, caller.Function)
			// 	}
			// }

			// if port.publisher != nil {
			// 	port.publisher.Publish(flog.NewLog(flog.LOGLEVELError, "程式發生panic: %v", err).AddPanicCallstack(0, FUNCNAME_RUN_AS_WE).AddItem("名稱", "家家").AddItem("綽號", "戶戶"))
			// }

			if port.publisher != nil {
				// log := flog.NewLog(flog.LOGLEVELError, "").AddPanicCallstack(0, FUNCNAME_RUN_AS_WE)
				log := flog.Panic(flog.LOGLEVELError, FUNCNAME_RUN_AS_WE, "")
				port.publisher.Publish(log.SetCaption("%s() 發生panic, %v", log.GetFunctionName(), err))
			}
			if tWSConn != nil {
				tWSConn.Close(errors.New(fmt.Sprintf("Find panic. err=%v", err)), nil)
			}

			return
		}
	}()

	_, err := u.Upgrade(conn)
	if err != nil {
		// handle error
		if port.publisher != nil {
			port.publisher.Publish(flog.Debug("Websocket upgrade failed. err=%v", err))
		}
		conn.Close()
		peerwg.Done()
	} else {
		defer conn.Close()

		var (
			state  = ws.StateServerSide
			reader = wsutil.NewReader(conn, state)
			// writer = wsutil.NewWriter(conn, state, ws.OpText)	// 已經用不著了
		)
		tPeerID := port.connCount
		port.connCount++
		tWSConn = &sWSConnection{connection: conn, peerWG: peerwg, state: ws.StateServerSide, peerID: tPeerID, portObj: port, Reader: reader, sender: NewSender(conn), connState: Connected, lastRecvTime: time.Now()}
		//ifEvenCallback := ifPort.OnAccept(&tPeer)
		ifEvenCallback := fOnAccept(tWSConn)
		if ifEvenCallback == nil {
			peerwg.Done()
			return
		}
		if ifEvenCallback != nil {
			tWSConn.eventCallback = ifEvenCallback
			port.Lock()
			port.wsConnections[tPeerID] = tWSConn
			port.Unlock()
		}
		// defer func() {
		// 	if err = recover(); err != nil {
		// 		fmt.Printf("find panic, error: %v\n", err)
		// 		tWSConn.Close(err, nil)
		// 		return
		// 	}
		// }()

		for tWSConn.connState < Closing {
			header, err := reader.NextFrame()
			if err != nil {
				// handle error
				if err == io.ErrUnexpectedEOF {
					if port.publisher != nil {
						port.publisher.Publish(flog.Error("NextFrame() ErrUnexpectedEOF: %v", err))
					}
					tWSConn.Close(err, nil)
					return
				}
				// if port.publisher != nil { // 當伺服器做shutdown時會發生，屬正常
				// 	port.publisher.Publish(flog.NewLog(flog.LOGLEVELError, "NextFrame() error: %v", err))
				// }
				tWSConn.Close(err, nil)
				return
			}
			if header.Fin { // 不是 Fin 的要組合起來，記得加上,close frame也要處理，也要補上發送close frame
				tWSConn.timeoutCheck = 0
				tWSConn.lastRecvTime = time.Now()
				// 資料收完整才處理
				switch header.OpCode {
				case ws.OpBinary:
					//fmt.Printf("User%d Binary data, Length = %d\n", myCount, header.Length)
					// Reset writer to write frame with right operation code.
					//writer.Reset(conn, state, header.OpCode) // 已經用不著了
					msg := make([]byte, header.Length+1)
					n, err := reader.Read(msg)
					if (err != nil) && (err != io.EOF) {
						switch err {
						case io.ErrUnexpectedEOF:
							if port.publisher != nil {
								port.publisher.Publish(flog.Error("reader.Read(msg) err : io.ErrUnexpectedEOF"))
							}
							tWSConn.Close(err, nil)
							return
						// case io.EOF:
						// 	fmt.Printf("User%d Read EOF\n", myCount)
						// 	msg[n] = 0
						default:
							if port.publisher != nil {
								port.publisher.Publish(flog.Error("reader.Read(msg) err : %v", err))
							}
							tWSConn.Close(err, nil)
							return
						}
					}
					// if header.Length > 0 {
					if ifEvenCallback != nil && tWSConn.connState < Closing {
						ifEvenCallback.OnRecv(tWSConn, msg[:n], header.Length, false)
					}
					// }
				case ws.OpText:
					//fmt.Printf("User%d Text data, Length = %d\n", myCount, header.Length)
					// Reset writer to write frame with right operation code.
					//writer.Reset(conn, state, header.OpCode) // 已經用不著了
					msg := make([]byte, header.Length+1)
					n, err := reader.Read(msg)
					if (err != nil) && (err != io.EOF) {
						switch err {
						case io.ErrUnexpectedEOF:
							if port.publisher != nil {
								port.publisher.Publish(flog.Error("reader.Read(msg) err : io.ErrUnexpectedEOF"))
							}
							tWSConn.Close(err, nil)
							return
						// case io.EOF:
						// 	fmt.Printf("User%d Read EOF\n", myCount)
						default:
							if port.publisher != nil {
								port.publisher.Publish(flog.Error("reader.Read(msg) err : %v", err))
							}
							tWSConn.Close(err, nil)
							return
						}
					}
					if ifEvenCallback != nil && tWSConn.connState < Closing {
						ifEvenCallback.OnRecv(tWSConn, msg[:n], header.Length, true)
					}
				case ws.OpClose: // 收到遠端ws結束訊號
					tWSConn.Close(nil, nil)
					return
				case ws.OpPong:
					// Reset writer to write frame with right operation code.
					//writer.Reset(conn, state, header.OpCode) // 已經用不著了

					// if ifEvenCallback != nil {
					// ifEvenCallback.OnPong(tWSConn)
					// }
				case ws.OpPing:
					// Reset writer to write frame with right operation code.
					//writer.Reset(conn, state, header.OpCode) // 已經用不著了

					// if ifEvenCallback == nil {
					if tWSConn.connState < Closing {
						frame := ws.NewPongFrame(NilPayload)
						if err := ws.WriteHeader(conn, frame.Header); err != nil {
							if port.publisher != nil {
								port.publisher.Publish(flog.Error("ws.WriteHeader() err : %v", err))
							}
							tWSConn.Close(err, nil)
							return
						}
					}
					// } else {
					// 	ifEvenCallback.OnPing(tWSConn)
					// }
				}
			}
		}
	}
}

func NewPort(ctx context.Context, wg *sync.WaitGroup, publisher *flog.SPublisher) *SWSPort {
	p := SWSPort{serverWG: wg, wsConnections: make(map[uint64]IWSConnection), publisher: publisher, keepalive: 30, timeout: 60}
	p.ctx, p.cancel = context.WithCancel(ctx)
	return &p
}

func init() {
	fcb.AddDefaultHiddenCaller(FUNCNAME_RUN_AS_WE)
}
