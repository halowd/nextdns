package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"github.com/nextdns/nextdns/resolver"
)

const maxUDPSize = 512

// This is the required size of the OOB buffer to pass to ReadMsgUDP.
var udpOOBSize = func() int {
	// We can't know whether we'll get an IPv4 control message or an
	// IPv6 control message ahead of time. To get around this, we size
	// the buffer equal to the largest of the two.

	oob4 := ipv4.NewControlMessage(ipv4.FlagDst | ipv4.FlagInterface)
	oob6 := ipv6.NewControlMessage(ipv6.FlagDst | ipv6.FlagInterface)

	if len(oob4) > len(oob6) {
		return len(oob4)
	}

	return len(oob6)
}()

func (p Proxy) serveUDP(l net.PacketConn) error {
	bpool := sync.Pool{
		New: func() interface{} {
			b := make([]byte, maxUDPSize)
			return &b
		},
	}

	c, ok := l.(*net.UDPConn)
	if !ok {
		return errors.New("not a UDP socket")
	}
	if err := setUDPDstOptions(c); err != nil {
		return fmt.Errorf("setUDPDstOptions: %w", err)
	}

	for {
		buf := *bpool.Get().(*[]byte)
		qsize, lip, raddr, err := readUDP(c, buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Temporary() {
				bpool.Put(&buf)
				continue
			}
			return err
		}
		if qsize <= 14 {
			bpool.Put(&buf)
			continue
		}
		start := time.Now()
		go func() {
			var err error
			var rsize int
			var ri resolver.ResolveInfo
			q, err := resolver.NewQuery(buf[:qsize], addrIP(raddr))
			if err != nil {
				p.logErr(err)
			}
			defer func() {
				bpool.Put(&buf)
				p.logQuery(QueryInfo{
					PeerIP:            q.PeerIP,
					Protocol:          "UDP",
					Type:              q.Type,
					Name:              q.Name,
					QuerySize:         qsize,
					ResponseSize:      rsize,
					Duration:          time.Since(start),
					UpstreamTransport: ri.Transport,
					Error:             err,
				})
			}()
			ctx := context.Background()
			if p.Timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, p.Timeout)
				defer cancel()
			}
			if rsize, ri, err = p.Resolve(ctx, q, buf); err != nil {
				return
			}
			if rsize > maxUDPSize {
				return
			}
			_, _, err = c.WriteMsgUDP(buf[:rsize], oobWithSrc(lip), raddr)
		}()
	}
}

// setUDPDstOptions sets the FlagDst on c to request the destination address as
// part of the oob data.
func setUDPDstOptions(c *net.UDPConn) error {
	// Try setting the flags for both families and ignore the errors unless they
	// both error.
	err6 := ipv6.NewPacketConn(c).SetControlMessage(ipv6.FlagDst|ipv6.FlagInterface, true)
	err4 := ipv4.NewPacketConn(c).SetControlMessage(ipv4.FlagDst|ipv4.FlagInterface, true)
	if err6 != nil && err4 != nil {
		return err4
	}
	return nil
}

// readUDP reads from c to buf and returns the local and remote addresses.
func readUDP(c *net.UDPConn, buf []byte) (n int, lip net.IP, raddr *net.UDPAddr, err error) {
	var oobn int
	oob := make([]byte, udpOOBSize)
	n, oobn, _, raddr, err = c.ReadMsgUDP(buf, oob)
	if err != nil {
		return -1, nil, nil, err
	}
	lip = parseDstFromOOB(oob[:oobn])
	return n, lip, raddr, nil
}

// oobWithSrc returns oob data with the Dst set with ip.
func oobWithSrc(ip net.IP) []byte {
	// If the dst is definitely an IPv6, then use ipv6's ControlMessage to
	// respond otherwise use ipv4's because ipv6's marshal ignores ipv4
	// addresses.
	if ip.To4() == nil {
		cm := &ipv6.ControlMessage{}
		cm.Src = ip
		return cm.Marshal()
	}
	cm := &ipv4.ControlMessage{}
	cm.Src = ip
	return cm.Marshal()
}

// parseDstFromOOB takes oob data and returns the destination IP.
func parseDstFromOOB(oob []byte) net.IP {
	cm6 := &ipv6.ControlMessage{}
	if cm6.Parse(oob) == nil && cm6.Dst != nil {
		return cm6.Dst
	}
	cm4 := &ipv4.ControlMessage{}
	if cm4.Parse(oob) == nil && cm4.Dst != nil {
		return cm4.Dst
	}
	return nil
}
