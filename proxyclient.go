package frontstep

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// From quic-go/internal/protocol/protocol.go:
// > MaxPacketBufferSize maximum packet size of any QUIC packet, based on
// > ethernet's max size, minus the IP and UDP headers. IPv6 has a 40 byte header,
// > UDP adds an additional 8 bytes.  This is a total overhead of 48 bytes.
// > Ethernet's max packet size is 1500 bytes,  1500 - 48 = 1452.
// Context: QUIC shouldn't allow IP packet fragmentation, so it has to fit into one Ethernet frame.
// Hence we can work backwards from the frame size.
// Also, if we set DF (don't fragment) for the network layer and pass it a packet
// larger than the MTU, we should expect it to be discarded.
const ReadBufSize = 1452

const Timeout = 10 * time.Millisecond

func DialAddr(dstAddr, proxyAddr string, shouldProxy func([]byte) bool) (*ProxyClient, error) {
	// We replace net.ListenUDP with our own type
	// Set up UDP address to return from Read* calls
	udpAddr, err := net.ResolveUDPAddr("udp", dstAddr)
	if err != nil {
		return nil, err
	}
	log.Printf("PROXYCLIENT:DialAddr: dialing %s", dstAddr)
	udpConn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		return nil, err
	}

	pc := ProxyClient{
		UDPConn:     udpConn,
		shouldProxy: shouldProxy,
		dstAddr:     dstAddr,
		DstUDPAddr:  udpAddr,
		proxyAddr:   proxyAddr,
		readLocal:   make(chan MsgUDP),
		readProxy:   make(chan MsgUDP),
		writeProxy:  make(chan []byte),
	}

	return &pc, nil
}

// TODO domain fronting
type ProxyClient struct {
	*net.UDPConn
	shouldProxy func([]byte) bool
	dstAddr     string
	DstUDPAddr  *net.UDPAddr
	proxyAddr   string
	// Local reads
	readLocal chan MsgUDP
	// Read/Write proxy
	readProxy  chan MsgUDP
	writeProxy chan []byte
}

// Response from ReadMsgUDP
// n int, oobn int, flags int, addr *net.UDPAddr, err error
type MsgUDP struct {
	bs  []byte
	err error
}

func (pc *ProxyClient) Run(ctx context.Context, done chan bool) {
	// We want to cancel if proxy sends opclose
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	log.Println("PROXYCLIENT: Running")

	//TODO handle ws vs wss depending on host?
	query := url.Values{"address": []string{pc.dstAddr}}.Encode()
	proxyWSAddr := fmt.Sprintf("ws://%s/?%s", pc.proxyAddr, query)

	// returns (net.Conn, *bufio.Reader, Handshake, error)
	// Can ignore Reader, but may want to do pbufio.PutReader(buf) to recover memory
	wsConn, _, _, err := ws.Dial(ctx, proxyWSAddr)
	if err != nil {
		log.Fatalf("PROXYCLIENT:WS:DIAL: %s", err)
	}
	defer wsConn.Close()
	log.Printf("PROXYCLIENT:WS: dialed %s", proxyWSAddr)

	// Handle messages received locally
	// This is for the specific case in which we would merge incoming data streams
	// from both a WebSocket proxy (for QUIC long header packets)
	// and a local UDP connection (for QUIC short header packets)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer cancel()
		defer wg.Done()
		// Both sides of the channel should have the same buffer capacity
		b := make([]byte, ReadBufSize)
		oob := make([]byte, 0)
		for {
			select {
			case <-ctx.Done():
				log.Printf("PROXYCLIENT:UDP:READ: Context done. Closing.")
				return
			default:
			}

			pc.UDPConn.SetReadDeadline(time.Now().Add(Timeout))
			n, _, _, _, err := pc.UDPConn.ReadMsgUDP(b, oob)
			e, ok := err.(net.Error)
			switch {
			case ok && e.Timeout(): // Don't log this; it's here in part to check for cancellation
				continue
			case err == io.EOF:
				log.Println("PROXYCLIENT:UDP:READ:EOF")
				return
			case err != nil:
				log.Printf("PROXYCLIENT:UDP:READ:ERROR: %s", err)
				continue
			}

			pc.readLocal <- MsgUDP{b[:n], err}
		}
	}()

	// Handle messages from proxy server
	wg.Add(1)
	go func() {
		defer cancel()
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				log.Printf("PROXYCLIENT:WS:READ: Context done. Closing.")
				return
			default:
			}

			//TODO this may interfere with our writes
			// by updating the writer with control frames from reads
			wsConn.SetReadDeadline(time.Now().Add(Timeout))
			data, op, err := wsutil.ReadData(wsConn, ws.StateClientSide)
			e, ok := err.(net.Error)
			switch {
			case ok && e.Timeout():
				continue
			case err == io.EOF:
				log.Println("PROXYCLIENT:WS:READ: Received EOF")
				pc.readProxy <- MsgUDP{nil, err}
				return
			case err != nil:
				log.Printf("PROXYCLIENT:WS:READ:ERROR: %s", err)
				pc.readProxy <- MsgUDP{nil, err}
				continue
			case op == ws.OpClose:
				log.Println("PROXYCLIENT:WS:GOT: Got OpClose from proxy. Closing.")
				return
			case op != ws.OpBinary:
				log.Printf("PROXYCLIENT:WS:GOT:ERROR: Expected OpBinary, got %#v with data: %s", op, string(data))
				continue
			default:
				pc.readProxy <- MsgUDP{data, nil}
			}
		}
	}()

	// Handle websocket writes to proxy
	wg.Add(1)
	go func() {
		defer cancel()
		defer wg.Done()
		log.Println("PROXYCLIENT: Handling writes")
		for {
			var data []byte
			// Maybe instead use a Closed channel, then goto DIAL if we need to reconnect?
			select {
			case <-ctx.Done():
				log.Printf("PROXYCLIENT:WS:WRITE: Context done. Closing.")
				return
			case data = <-pc.writeProxy:
			}
			// Get outbound packet
			log.Println("PROXYCLIENT:WS:WRITE: writing")
			wsConn.SetWriteDeadline(time.Now().Add(Timeout))
			err = wsutil.WriteClientBinary(wsConn, data)
			e, ok := err.(net.Error)
			switch {
			case ok && e.Timeout():
				continue
			case err != nil:
				log.Printf("PROXYCLIENT:WS:WRITE:ERROR: %s", err)
			}
		}
	}()

	// Allow writes to finish before allowing conn to close
	wg.Wait()
	log.Println("PROXYCLIENT: Closing UDPConn")
	err = pc.UDPConn.Close()
	if err != nil {
		log.Printf("PROXYCLIENT:ERROR: couldn't close UDPConn: %s", err)
	}
	done <- true
}

// TODO There's some kind of issue with passing down to pc.UDPConn.WriteTo(...).
// I think it's meant for non-connected use.
// But for some reason, it's hanging on a connected socket in a way that WriteMsgUDP doesn't.
func (pc *ProxyClient) WriteTo(bs []byte, addr net.Addr) (int, error) {
	if pc.shouldProxy(bs) {
		log.Print("PROXYCLIENT:UDP:WriteTo:PROXY: Packet:\n\t" + strings.ReplaceAll(hex.Dump(bs), "\n", "\n\t"))
		pc.writeProxy <- bs
		return len(bs), nil
	} else {
		log.Print("PROXYCLIENT:UDP:WriteTo:LOCAL: Packet:\n\t" + strings.ReplaceAll(hex.Dump(bs), "\n", "\n\t"))
		return pc.UDPConn.WriteTo(bs, addr)
	}
}

// Note that addr should almost always be nil, since we're always using a "connected" UDP socket.
func (pc *ProxyClient) WriteMsgUDP(bs, oob []byte, addr *net.UDPAddr) (int, int, error) {
	if pc.shouldProxy(bs) {
		log.Print("PROXYCLIENT:UDP:WriteMsgUDP:PROXY: Packet:\n\t" + strings.ReplaceAll(hex.Dump(bs), "\n", "\n\t"))
		pc.writeProxy <- bs
		//TODO should we lie about sending oob bytes?
		return len(bs), len(oob), nil
	} else {
		log.Print("PROXYCLIENT:UDP:WriteMsgUDP:LOCAL: Packet:\n\t" + strings.ReplaceAll(hex.Dump(bs), "\n", "\n\t"))
		return pc.UDPConn.WriteMsgUDP(bs, oob, addr)
	}
}

func (pc *ProxyClient) ReadFrom(bs []byte) (int, net.Addr, error) {
	select {
	case msgUDP := <-pc.readProxy:
		n := copy(bs, msgUDP.bs)
		log.Printf("PROXYCLIENT:UDP:ReadFrom:PROXY: %s", bs[:n])
		// We can always return the original address:
		// since we're emulating a "connected" UDP socket,
		// the kernel should only be returning packets from our destination.
		return n, pc.DstUDPAddr, msgUDP.err
	case msgUDP := <-pc.readLocal:
		n := copy(bs, msgUDP.bs)
		log.Printf("PROXYCLIENT:UDP:ReadFrom:LOCAL: %s", bs[:n])
		return n, pc.DstUDPAddr, msgUDP.err
	}
}

// Returns (n, oobn, flags int, addr *net.UDPAddr, err error)
// but since there's no underlying raw socket, we just return 0 for oobn and flags.
func (pc *ProxyClient) ReadMsgUDP(bs, oob []byte) (int, int, int, *net.UDPAddr, error) {
	select {
	case msgUDP := <-pc.readProxy:
		n := copy(bs, msgUDP.bs)
		log.Printf("PROXYCLIENT:UDP:ReadMsgUDP:PROXY: %s", bs[:n])
		return n, 0, 0, pc.DstUDPAddr, msgUDP.err
	case msgUDP := <-pc.readLocal:
		n := copy(bs, msgUDP.bs)
		log.Printf("PROXYCLIENT:UDP:ReadMsgUDP:LOCAL: %s", bs[:n])
		return n, 0, 0, pc.DstUDPAddr, msgUDP.err
	}
}
