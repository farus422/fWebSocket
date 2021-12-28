package fws

import (
	"fmt"
	"net"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/gobwas/ws"
)

type SLinkNode struct {
	dataBuf        []byte
	previous, next *SLinkNode
}

type SSender struct {
	mutex          sync.Mutex
	firstNode      *SLinkNode
	lastNode       *SLinkNode
	connection     net.Conn
	sendProcessing bool
}

func NewSender(conn net.Conn) *SSender {
	sdr := &SSender{connection: conn}
	return sdr
}

func (sdr *SSender) processNextSend() *SLinkNode {
	sdr.mutex.Lock()
	if sdr.firstNode != nil {
		next := sdr.firstNode
		sdr.firstNode = sdr.firstNode.next
		if sdr.firstNode != nil {
			sdr.firstNode.previous = nil
		} else {
			sdr.lastNode = nil
		}
		sdr.mutex.Unlock()
		next.next = nil
		return next
	}
	sdr.sendProcessing = false
	sdr.mutex.Unlock()
	return nil
}

func (sdr *SSender) clearupAndEndProcess() {
	var previous *SLinkNode
	sdr.mutex.Lock()
	for sdr.firstNode != nil {
		previous = sdr.firstNode
		sdr.firstNode = sdr.firstNode.next
		previous.next = nil
		previous.previous = nil
		previous.dataBuf = nil
	}
	sdr.lastNode = nil
	sdr.sendProcessing = false
	sdr.mutex.Unlock()
}

var linkNodePool = sync.Pool{
	New: func() interface{} {
		return new(SLinkNode)
	},
}

func (sdr *SSender) Send(buf []byte, opCode ws.OpCode) {
	// node := &SLinkNode{dataBuf: buf}
	node := linkNodePool.Get().(*SLinkNode)
	node.dataBuf = buf

	sdr.mutex.Lock()
	if sdr.sendProcessing {
		// 已經有傳送在進行中，我們交給他就好
		if sdr.lastNode != nil {
			node.previous = sdr.lastNode
			sdr.lastNode.next = node
			sdr.lastNode = node
		} else {
			sdr.firstNode = node
			sdr.lastNode = node
		}
		sdr.mutex.Unlock()
		return
	}

	// 沒有傳送中的，啟動傳動程序
	sdr.sendProcessing = true
	sdr.mutex.Unlock()
	go func() {
		var writeCount int
		for {
			frame := ws.NewFrame(opCode, true, node.dataBuf)
			//frame := ws.NewFrame(opCode, true, NilPayload)
			if err := ws.WriteHeader(sdr.connection, frame.Header); err != nil {
				for err == syscall.EAGAIN {
					time.Sleep(50 * time.Millisecond)
					err = ws.WriteHeader(sdr.connection, frame.Header)
				}
				if err != nil {
					sdr.connection.Close()
					sdr.clearupAndEndProcess()
					callStack := debug.Stack()
					fmt.Printf("SSender.Send() error: %v\n<< call stack >>\n%s\n", err, callStack)
					node.dataBuf = nil
					linkNodePool.Put(node)
					return
				}
			}

			if n, err := sdr.connection.Write(node.dataBuf); err != nil {
				for err == syscall.EAGAIN {
					time.Sleep(100 * time.Millisecond)
					writeCount, err = sdr.connection.Write(node.dataBuf[n:])
					n += writeCount
				}
				if err != nil {
					sdr.connection.Close()
					sdr.clearupAndEndProcess()
					callStack := debug.Stack()
					fmt.Printf("SSender.Send() error: %v\n<< call stack >>\n%s\n", err, callStack)
					node.dataBuf = nil
					linkNodePool.Put(node)
					return
				}
			}
			node.dataBuf = nil
			linkNodePool.Put(node)

			// callStack := debug.Stack()
			// fmt.Printf("SSender.Send() send: %s\n<< call stack >>\n%s\n", node.dataBuf, callStack)
			// 讀取下一筆，執行發送
			node = sdr.processNextSend()
			if node == nil {
				// 如果沒有資料了，結束工作
				return
			}
		}
	}()
}
