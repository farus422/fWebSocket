package fws

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	flog "server/fLogSystem"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

type FOnAccept = func(ICorePeer) IPeerCallback

var ERR_SERVER_SHUTDOWN = errors.New("Server shutdown")

type SPort struct {
	listener  net.Listener
	mutex     sync.Mutex
	serverWG  *sync.WaitGroup
	peerWG    sync.WaitGroup
	peers     map[uint64]ICorePeer
	publisher *flog.SPublisher
	connCount uint64
}

var NilPayload []byte = make([]byte, 1)

func (port *SPort) ListenAndServe(addr string, fOnAccept FOnAccept) error {
	port.serverWG.Add(1)
	if port.listener != nil {
		port.Unlisten()
	}
	ln, listen_err := net.Listen("tcp", addr)
	if listen_err != nil {
		port.serverWG.Done()
		return listen_err
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
	return nil
}

func (port *SPort) Unlisten() {
	if port.listener != nil {
		ln := port.listener
		port.listener = nil
		ln.Close()
	}
}

func (port *SPort) GetPublisher() *flog.SPublisher {
	return port.publisher
}

func (port *SPort) GetPeersAndLock() map[uint64]ICorePeer {
	port.mutex.Lock()
	return port.peers
}

func (port *SPort) Lock() {
	port.mutex.Lock()
}

func (port *SPort) Unlock() {
	port.mutex.Unlock()
}

func (port *SPort) CloseAllPeer() {
	for _, corePeer := range port.peers {
		corePeer.Close(ERR_SERVER_SHUTDOWN, nil)
	}
}

func (port *SPort) WaitForAllPeerClosed() {
	port.peerWG.Wait()
}

func (port *SPort) Shutdown() {
	port.Unlisten()
	port.CloseAllPeer()
	port.WaitForAllPeerClosed()
}

const FUNCNAME_RUN_AS_WE = "fWebSocket.(*SPort).runAsWebSocket"

func (port *SPort) runAsWebSocket(conn net.Conn, u ws.Upgrader, fOnAccept FOnAccept, peerwg *sync.WaitGroup) {
	var tCorePeer *sCorePeer = nil
	defer func() {
		if err := recover(); err != nil {
			// cs := fdebug.SCallstack{}
			// cs.GetCallstack(0, "")
			// fmt.Println("Callstacks:")
			// cs.Print()

			// csp := fdebug.SCallstack{}
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
				port.publisher.Publish(flog.NewLog(flog.LOGLEVELError, "程式發生panic: %v", err).AddPanicCallstack(0, FUNCNAME_RUN_AS_WE))
			}
			if tCorePeer != nil {
				tCorePeer.Close(errors.New(fmt.Sprintf("find panic, error: %v\n", err)), nil)
			}

			return
		}
	}()
	_, err := u.Upgrade(conn)
	if err != nil {
		// handle error
		if port.publisher != nil {
			port.publisher.Publish(flog.NewLog(flog.LOGLEVELError, "Upgrade fail: %v", err))
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
		tCorePeer = &sCorePeer{connection: conn, peerWG: peerwg, state: ws.StateServerSide, peerID: tPeerID, portObj: port, Reader: reader, sender: NewSender(conn), connState: Connected}
		//ifEvenCallback := ifPort.OnAccept(&tPeer)
		ifEvenCallback := fOnAccept(tCorePeer)
		if ifEvenCallback == nil {
			conn.Close()
			peerwg.Done()
			return
		}
		if ifEvenCallback != nil {
			tCorePeer.eventCallback = ifEvenCallback
			port.Lock()
			port.peers[tPeerID] = tCorePeer
			port.Unlock()
		}
		// defer func() {
		// 	if err = recover(); err != nil {
		// 		fmt.Printf("find panic, error: %v\n", err)
		// 		tCorePeer.Close(err, nil)
		// 		return
		// 	}
		// }()

		for tCorePeer.connState < Closing {
			header, err := reader.NextFrame()
			if err != nil {
				// handle error
				if err == io.ErrUnexpectedEOF {
					if port.publisher != nil {
						port.publisher.Publish(flog.NewLog(flog.LOGLEVELError, "NextFrame() ErrUnexpectedEOF: %v", err))
					}
					tCorePeer.Close(err, nil)
					return
				}
				if port.publisher != nil {
					port.publisher.Publish(flog.NewLog(flog.LOGLEVELError, "NextFrame() error: %v", err))
				}
				tCorePeer.Close(err, nil)
				return
			}
			if header.Fin {
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
								port.publisher.Publish(flog.NewLog(flog.LOGLEVELError, "reader.Read(msg) err : io.ErrUnexpectedEOF"))
							}
							tCorePeer.Close(err, nil)
							return
						// case io.EOF:
						// 	fmt.Printf("User%d Read EOF\n", myCount)
						// 	msg[n] = 0
						default:
							if port.publisher != nil {
								port.publisher.Publish(flog.NewLog(flog.LOGLEVELError, "reader.Read(msg) err : %v", err))
							}
							tCorePeer.Close(err, nil)
							return
						}
					}
					if ifEvenCallback != nil && header.Length > 0 {
						ifEvenCallback.OnRecv(tCorePeer, msg[:n], header.Length, false)
					}
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
								port.publisher.Publish(flog.NewLog(flog.LOGLEVELError, "reader.Read(msg) err : io.ErrUnexpectedEOF"))
							}
							tCorePeer.Close(err, nil)
							return
						// case io.EOF:
						// 	fmt.Printf("User%d Read EOF\n", myCount)
						default:
							if port.publisher != nil {
								port.publisher.Publish(flog.NewLog(flog.LOGLEVELError, "reader.Read(msg) err : %v", err))
							}
							tCorePeer.Close(err, nil)
							return
						}
					}
					if ifEvenCallback != nil {
						ifEvenCallback.OnRecv(tCorePeer, msg[:n], header.Length, true)
					}
				case ws.OpClose:
					if port.publisher != nil {
						port.publisher.Publish(flog.NewLog(flog.LOGLEVELError, "WebSocket is closed"))
					}
					tCorePeer.Close(nil, nil)
					return
				case ws.OpPong:
					// Reset writer to write frame with right operation code.
					//writer.Reset(conn, state, header.OpCode) // 已經用不著了

					// if ifEvenCallback != nil {
					// ifEvenCallback.OnPong(tCorePeer)
					// }
				case ws.OpPing:
					// Reset writer to write frame with right operation code.
					//writer.Reset(conn, state, header.OpCode) // 已經用不著了

					// if ifEvenCallback == nil {
					frame := ws.NewPongFrame(NilPayload)
					if err := ws.WriteHeader(conn, frame.Header); err != nil {
						if port.publisher != nil {
							port.publisher.Publish(flog.NewLog(flog.LOGLEVELError, "ws.WriteHeader() err : %v", err))
						}
						tCorePeer.Close(err, nil)
						return
					}
					// } else {
					// 	ifEvenCallback.OnPing(tCorePeer)
					// }
				}
			}
		}
	}
}

func NewPort(wg *sync.WaitGroup, publisher *flog.SPublisher) *SPort {
	return &SPort{serverWG: wg, peers: make(map[uint64]ICorePeer), publisher: publisher}
}
