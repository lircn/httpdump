package main

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

// gopacket provide a tcp connection, however it split one tcp connection into two stream.
// So it is hard to match http request and response. we make our own connection here

const maxTCPSeq uint32 = 0xFFFFFFFF
const tcpSeqWindow = 0x0000FFFF

// TCPAssembler do tcp package assemble
type TCPAssembler struct {
	connectionDict    map[string]*TCPConnection
	lock              sync.Mutex
	connectionHandler ConnectionHandler
	filterIP          string
	filterPort        uint16
}

type TsInfo struct {
	up          bool
	reqFragment bool
	repFragment bool
	req1        time.Time
	req2        time.Time
	reqLen      int
	rep1        time.Time
	rep2        time.Time
	repLen      int
	id          string
}

var gTsInfo map[string]TsInfo = map[string]TsInfo{}

func newTCPAssembler(connectionHandler ConnectionHandler) *TCPAssembler {
	return &TCPAssembler{connectionDict: map[string]*TCPConnection{}, connectionHandler: connectionHandler}
}

func (assembler *TCPAssembler) assemble(flow gopacket.Flow, tcp *layers.TCP, timestamp time.Time) {
	src := Endpoint{ip: flow.Src().String(), port: uint16(tcp.SrcPort)}
	dst := Endpoint{ip: flow.Dst().String(), port: uint16(tcp.DstPort)}
	dropped := false
	if assembler.filterIP != "" {
		if src.ip != assembler.filterIP && dst.ip != assembler.filterIP {
			dropped = true
		}
	}
	if assembler.filterPort != 0 {
		if src.port != assembler.filterPort && dst.port != assembler.filterPort {
			dropped = true
		}
	}
	if dropped {
		return
	}

	srcString := src.String()
	dstString := dst.String()
	var key string
	if srcString < dstString {
		key = srcString + "-" + dstString
	} else {
		key = dstString + "-" + srcString
	}

	var createNewConn = tcp.SYN && !tcp.ACK || isHTTPRequestData(tcp.Payload)
	connection := assembler.retrieveConnection(src, dst, key, createNewConn)
	if connection == nil {
		return
	}

	connection.onReceive(src, dst, tcp, timestamp)

	if connection.closed() {
		printTsInfo(connection.key)
		assembler.deleteConnection(key)
		connection.finish()
	}
}

// get connection this packet belong to; create new one if is new connection
func (assembler *TCPAssembler) retrieveConnection(src, dst Endpoint, key string, init bool) *TCPConnection {
	assembler.lock.Lock()
	defer assembler.lock.Unlock()
	connection := assembler.connectionDict[key]
	if connection == nil {
		if init {
			connection = newTCPConnection(key)
			assembler.connectionDict[key] = connection
			assembler.connectionHandler.handle(src, dst, connection)
		}
	}

	return connection
}

// remove connection (when is closed or timeout)
func (assembler *TCPAssembler) deleteConnection(key string) {
	assembler.lock.Lock()
	defer assembler.lock.Unlock()
	delete(assembler.connectionDict, key)
}

// flush timeout connections
func (assembler *TCPAssembler) flushOlderThan(time time.Time) {
	var connections []*TCPConnection
	assembler.lock.Lock()
	for _, connection := range assembler.connectionDict {
		if connection.lastTimestamp.Before(time) {
			connections = append(connections, connection)
		}
	}
	for _, connection := range connections {
		delete(assembler.connectionDict, connection.key)
	}
	assembler.lock.Unlock()

	for _, connection := range connections {
		connection.flushOlderThan()
	}
}

func (assembler *TCPAssembler) finishAll() {
	assembler.lock.Lock()
	defer assembler.lock.Unlock()
	for _, connection := range assembler.connectionDict {
		connection.finish()
	}
	assembler.connectionDict = nil
	assembler.connectionHandler.finish()
}

// ConnectionHandler is interface for handle tcp connection
type ConnectionHandler interface {
	handle(src Endpoint, dst Endpoint, connection *TCPConnection)
	finish()
}

// TCPConnection hold info for one tcp connection
type TCPConnection struct {
	upStream      *NetworkStream // stream from client to server
	downStream    *NetworkStream // stream from server to client
	clientID      Endpoint       // the client key(by ip and port)
	lastTimestamp time.Time      // timestamp receive last packet
	isHTTP        bool
	key           string
}

// Endpoint is one endpoint of a tcp connection
type Endpoint struct {
	ip   string
	port uint16
}

func (p Endpoint) equals(p2 Endpoint) bool {
	return p.ip == p2.ip && p.port == p2.port
}

func (p Endpoint) String() string {
	return p.ip + ":" + strconv.Itoa(int(p.port))
}

// ConnectionID identify a tcp connection
type ConnectionID struct {
	src Endpoint
	dst Endpoint
}

// create tcp connection, by the first tcp packet. this packet should from client to server
func newTCPConnection(key string) *TCPConnection {
	connection := &TCPConnection{
		upStream:   newNetworkStream(),
		downStream: newNetworkStream(),
		key:        key,
	}
	return connection
}

// when receive tcp packet
func (connection *TCPConnection) onReceive(src, dst Endpoint, tcp *layers.TCP, timestamp time.Time) {
	connection.lastTimestamp = timestamp
	payload := tcp.Payload

	if !connection.isHTTP {
		// skip no-http data
		if !isHTTPRequestData(payload) {
			return
		}
		// receive first valid http data packet
		connection.clientID = src
		connection.isHTTP = true
	}

	var sendStream, confirmStream *NetworkStream
	var up bool
	if connection.clientID.equals(src) {
		sendStream = connection.upStream
		confirmStream = connection.downStream
		up = true
	} else {
		sendStream = connection.downStream
		confirmStream = connection.upStream
		up = false
	}

	if isHTTPRequestData(payload) {
		info := TsInfo{req1: timestamp, req2: timestamp, up: up, reqFragment: false, repFragment: false, reqLen: len(payload)}
		info.id = src.String() + "-" + dst.String()
		if info.reqLen > 1400 {
			info.reqFragment = true
		}
		gTsInfo[connection.key] = info
	}
	if len(payload) > 100 { /* not only ack */
		if info, ok := gTsInfo[connection.key]; ok {
			if info.up == up {
				info.req2 = timestamp
				info.reqLen += len(payload)
			} else {
				info.rep2 = timestamp
				info.repLen += len(payload)
			}
			gTsInfo[connection.key] = info
		}
	}
	if isHTTPReplyData(payload) {
		printTsInfo(connection.key)
		if info, ok := gTsInfo[connection.key]; ok {
			if len(payload) > 1400 {
				info.repFragment = true
			}
			info.rep1 = timestamp
			info.rep2 = timestamp
			info.repLen = len(payload)
			gTsInfo[connection.key] = info
		}
	}

	sendStream.appendPacket(tcp)

	if tcp.SYN {
		// do nothing
	}

	if tcp.ACK {
		// confirm
		confirmStream.confirmPacket(tcp.Ack)
	}

	// terminate connection
	if tcp.FIN || tcp.RST {
		sendStream.closed = true
	}
}

// just close this connection?
func (connection *TCPConnection) flushOlderThan() {
	// flush all data
	//connection.upStream.window
	//connection.downStream.window
	// remove and close connection
	connection.upStream.closed = true
	connection.downStream.closed = true
	connection.finish()

}

func (connection *TCPConnection) closed() bool {
	return connection.upStream.closed && connection.downStream.closed
}

func (connection *TCPConnection) finish() {
	connection.upStream.finish()
	connection.downStream.finish()
}

// NetworkStream tread one-direction tcp data as stream. impl reader closer
type NetworkStream struct {
	window *ReceiveWindow
	c      chan *layers.TCP
	remain []byte
	ignore bool
	closed bool
}

func newNetworkStream() *NetworkStream {
	return &NetworkStream{window: newReceiveWindow(64), c: make(chan *layers.TCP, 1024)}
}

func (stream *NetworkStream) appendPacket(tcp *layers.TCP) {
	if stream.ignore {
		return
	}
	stream.window.insert(tcp)
}

func (stream *NetworkStream) confirmPacket(ack uint32) {
	if stream.ignore {
		return
	}
	stream.window.confirm(ack, stream.c)
}

func (stream *NetworkStream) finish() {
	close(stream.c)
}

func (stream *NetworkStream) Read(p []byte) (n int, err error) {
	for len(stream.remain) == 0 {
		packet, ok := <-stream.c
		if !ok {
			err = io.EOF
			return
		}
		stream.remain = packet.Payload
	}

	if len(stream.remain) > len(p) {
		n = copy(p, stream.remain[:len(p)])
		stream.remain = stream.remain[len(p):]
	} else {
		n = copy(p, stream.remain)
		stream.remain = nil
	}
	return
}

// Close the stream
func (stream *NetworkStream) Close() error {
	stream.ignore = true
	return nil
}

// ReceiveWindow simulate tcp receivec window
type ReceiveWindow struct {
	size        int
	start       int
	buffer      []*layers.TCP
	lastAck     uint32
	expectBegin uint32
}

func newReceiveWindow(initialSize int) *ReceiveWindow {
	buffer := make([]*layers.TCP, initialSize)
	return &ReceiveWindow{buffer: buffer}
}

func (window *ReceiveWindow) destroy() {
	window.size = 0
	window.start = 0
	window.buffer = nil
}

func (window *ReceiveWindow) insert(packet *layers.TCP) {

	if window.expectBegin != 0 && compareTCPSeq(window.expectBegin, packet.Seq+uint32(len(packet.Payload))) >= 0 {
		// dropped
		return
	}

	if len(packet.Payload) == 0 {
		//ignore empty data packet
		return
	}

	idx := window.size
	for ; idx > 0; idx-- {
		index := (idx - 1 + window.start) % len(window.buffer)
		prev := window.buffer[index]
		result := compareTCPSeq(prev.Seq, packet.Seq)
		if result == 0 {
			// duplicated
			return
		}
		if result < 0 {
			// insert at index
			break
		}
	}

	if window.size == len(window.buffer) {
		window.expand()
	}

	if idx == window.size {
		// append at last
		index := (idx + window.start) % len(window.buffer)
		window.buffer[index] = packet
	} else {
		// insert at index
		for i := window.size - 1; i >= idx; i-- {
			next := (i + window.start + 1) % len(window.buffer)
			current := (i + window.start) % len(window.buffer)
			window.buffer[next] = window.buffer[current]
		}
		index := (idx + window.start) % len(window.buffer)
		window.buffer[index] = packet
	}

	window.size++
}

// send confirmed packets to reader, when receive ack
func (window *ReceiveWindow) confirm(ack uint32, c chan *layers.TCP) {
	idx := 0
	for ; idx < window.size; idx++ {
		index := (idx + window.start) % len(window.buffer)
		packet := window.buffer[index]
		result := compareTCPSeq(packet.Seq, ack)
		if result >= 0 {
			break
		}
		window.buffer[index] = nil
		newExpect := packet.Seq + uint32(len(packet.Payload))
		if window.expectBegin != 0 {
			diff := compareTCPSeq(window.expectBegin, packet.Seq)
			if diff > 0 {
				duplicatedSize := window.expectBegin - packet.Seq
				if duplicatedSize < 0 {
					duplicatedSize += maxTCPSeq
				}
				if duplicatedSize >= uint32(len(packet.Payload)) {
					continue
				}
				packet.Payload = packet.Payload[duplicatedSize:]
			} else if diff < 0 {
				//TODO: we lose packet here
			}
		}
		c <- packet
		window.expectBegin = newExpect
	}
	window.start = (window.start + idx) % len(window.buffer)
	window.size = window.size - idx
	if compareTCPSeq(window.lastAck, ack) < 0 || window.lastAck == 0 {
		window.lastAck = ack
	}
}

func (window *ReceiveWindow) expand() {
	buffer := make([]*layers.TCP, len(window.buffer)*2)
	end := window.start + window.size
	if end < len(window.buffer) {
		copy(buffer, window.buffer[window.start:window.start+window.size])
	} else {
		copy(buffer, window.buffer[window.start:])
		copy(buffer[len(window.buffer)-window.start:], window.buffer[:end-len(window.buffer)])
	}
	window.start = 0
	window.buffer = buffer
}

// compare two tcp sequences, if seq1 is earlier, return num < 0, if seq1 == seq2, return 0, else return num > 0
func compareTCPSeq(seq1, seq2 uint32) int {
	if seq1 < tcpSeqWindow && seq2 > maxTCPSeq-tcpSeqWindow {
		return int(seq1 + maxTCPSeq - seq2)
	} else if seq2 < tcpSeqWindow && seq1 > maxTCPSeq-tcpSeqWindow {
		return int(seq1 - (maxTCPSeq + seq2))
	}
	return int(int32(seq1 - seq2))
}

var httpMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "DELETE": true, "HEAD": true,
	"TRACE": true, "OPTIONS": true, "PATCH": true}

// if is first http request packet
func isHTTPRequestData(body []byte) bool {
	if len(body) < 8 {
		return false
	}
	data := body[0:8]
	idx := bytes.IndexByte(data, byte(' '))
	if idx < 0 {
		return false
	}

	method := string(data[:idx])
	return httpMethods[method]
}

func isHTTPReplyData(body []byte) bool {
	if len(body) < 12 {
		return false
	}
	if strings.EqualFold("HTTP/1.1 200", string(body[0:12])) {
		return true
	}
	return false
}

func getInverseKey(key string) string {
	s := strings.Split(key, "-")
	return s[1] + "-" + s[0]
}

const gTimeFmt = "05.000000"

func printTsInfo(key string) {
	tsInfo := gTsInfo[key]
	if tsInfo.rep1.Before(tsInfo.req2) {
		return
	}

	fmt.Printf("%s \t%s \t%s \t%s \t%s \t%s \t%s \t%d \t%d \t", tsInfo.req1.Format(gTimeFmt), tsInfo.req2.Format(gTimeFmt), tsInfo.rep1.Format(gTimeFmt), tsInfo.rep2.Format(gTimeFmt), tsInfo.req2.Sub(tsInfo.req1), tsInfo.rep1.Sub(tsInfo.req2), tsInfo.rep2.Sub(tsInfo.rep1), tsInfo.reqLen, tsInfo.repLen)
	fmt.Println(tsInfo.reqFragment, tsInfo.repFragment, tsInfo.up, tsInfo.id)
}
