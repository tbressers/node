/*
 * Copyright (C) 2019 The "MysteriumNetwork/node" Authors.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package traversal

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/mysteriumnetwork/node/core/port"

	log "github.com/cihub/seelog"
	"github.com/mysteriumnetwork/node/services"
	"github.com/pkg/errors"
	"golang.org/x/net/ipv4"
)

const prefix = "[NATPinger] "
const pingInterval = 200
const pingTimeout = 10000

// Pinger represents NAT pinger structure
type Pinger struct {
	pingTarget     chan *Params
	pingCancelled  chan struct{}
	natEventWaiter NatEventWaiter
	configParser   ConfigParser
	once           sync.Once
	natProxy       natProxy
	portPool       portSupplier
	consumerPort   int
}

// NatEventWaiter is responsible for waiting for nat events
type NatEventWaiter interface {
	WaitForEvent() Event
}

// ConfigParser is able to parse a config from given raw json
type ConfigParser interface {
	Parse(config json.RawMessage) (ip string, port int, serviceType services.ServiceType, err error)
}

type portSupplier interface {
	Acquire() (port.Port, error)
}

// NewPingerFactory returns Pinger instance
func NewPingerFactory(waiter NatEventWaiter, parser ConfigParser, proxy natProxy, portPool portSupplier) *Pinger {
	target := make(chan *Params)
	cancel := make(chan struct{})
	return &Pinger{
		pingTarget:     target,
		pingCancelled:  cancel,
		natEventWaiter: waiter,
		configParser:   parser,
		natProxy:       proxy,
		portPool:       portPool,
	}
}

type natProxy interface {
	handOff(serviceType services.ServiceType, conn *net.UDPConn)
	registerServicePort(serviceType services.ServiceType, port int)
	isAvailable(serviceType services.ServiceType) bool
}

// Params contains session parameters needed to NAT ping remote peer
type Params struct {
	RequestConfig json.RawMessage
	Port          int
}

// Start starts NAT pinger and waits for PingTarget to ping
func (p *Pinger) Start() {
	log.Info(prefix, "Starting a NAT pinger")

	// We dont need to run pinger if NAT port auto-configuration is successful
	if p.natEventWaiter.WaitForEvent().Type == SuccessEventType {
		return
	}

	for {
		select {
		case pingParams := <-p.pingTarget:
			log.Info(prefix, "Pinging peer with", pingParams)

			IP, port, serviceType, err := p.configParser.Parse(pingParams.RequestConfig)
			if err != nil {
				log.Warn(prefix, errors.Wrap(err, fmt.Sprintf("unable to parse ping message: %v", pingParams)))
			}

			if !p.natProxy.isAvailable(serviceType) {
				log.Warn(prefix, serviceType, " NATProxy is not available for this transport protocol")
				continue
			}

			log.Infof("%sping target received: IP: %v, port: %v", prefix, IP, port)
			if port == 0 {
				// client did not sent its port to ping to, notifying the service to start
				continue
			}
			conn, err := p.getConnection(IP, port, pingParams.Port)
			if err != nil {
				log.Error(prefix, "failed to get connection: ", err)
				continue
			}

			go func() {
				err := p.ping(conn)
				if err != nil {
					log.Warn(prefix, "Error while pinging: ", err)
				}
			}()

			err = p.pingReceiver(conn)
			if err != nil {
				log.Error(prefix, "ping receiver error: ", err)
				continue
			}

			log.Info(prefix, "ping received, waiting for a new connection")

			go p.natProxy.handOff(serviceType, conn)
		}
	}
}

// Stop noop method
func (p *Pinger) Stop() {
	// noop method - NATPinger should not stop
}

// PingProvider pings provider determined by destination provided in sessionConfig
func (p *Pinger) PingProvider(ip string, port int) error {
	log.Info(prefix, "NAT pinging to provider")

	conn, err := p.getConnection(ip, port, p.consumerPort)
	if err != nil {
		return errors.Wrap(err, "failed to get connection")
	}
	defer conn.Close()

	go func() {
		err := p.ping(conn)
		if err != nil {
			log.Warn(prefix, "Error while pinging: ", err)
		}
	}()

	time.Sleep(pingInterval * time.Millisecond)
	err = p.pingReceiver(conn)
	if err != nil {
		return err
	}

	// wait for provider to setup NAT proxy connection
	time.Sleep(200 * time.Millisecond)

	return nil
}

func (p *Pinger) ping(conn *net.UDPConn) error {
	n := 1

	for {
		select {
		case <-p.pingCancelled:
			return nil

		case <-time.After(pingInterval * time.Millisecond):
			log.Trace(prefix, "pinging.. ")
			// This is the essence of the TTL based udp punching.
			// We're slowly increasing the TTL so that the packet is held.
			// After a few attempts we're setting the value to 128 and assuming we're through.
			// We could stop sending ping to Consumer beyond 4 hops to prevent from possible Consumer's router's
			//  DOS block, but we plan, that Consumer at the same time will be Provider too in near future.
			if n > 4 {
				n = 128
			}

			err := ipv4.NewConn(conn).SetTTL(n)
			if err != nil {
				return errors.Wrap(err, "pinger setting ttl failed")
			}

			n++

			_, err = conn.Write([]byte("continuously pinging to " + conn.RemoteAddr().String()))
			if err != nil {
				return err
			}
		}
	}
}

func (p *Pinger) getConnection(ip string, port int, pingerPort int) (*net.UDPConn, error) {
	udpAddr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", ip, port))
	if err != nil {
		return nil, err
	}

	log.Info(prefix, "remote socket: ", udpAddr.String())

	conn, err := net.DialUDP("udp", &net.UDPAddr{Port: pingerPort}, udpAddr)
	if err != nil {
		return nil, err
	}

	log.Info(prefix, "local socket: ", conn.LocalAddr().String())

	return conn, nil
}

// PingTarget relays ping target address data
func (p *Pinger) PingTarget(target *Params) {
	p.pingTarget <- target
}

// BindConsumerPort binds NATPinger to source consumer port
func (p *Pinger) BindConsumerPort(port int) {
	p.consumerPort = port
}

// BindServicePort register service port to forward connection to
func (p *Pinger) BindServicePort(serviceType services.ServiceType, port int) {
	p.natProxy.registerServicePort(serviceType, port)
}

func (p *Pinger) pingReceiver(conn *net.UDPConn) error {
	timeout := time.After(pingTimeout * time.Millisecond)
	for {
		select {
		case <-timeout:
			p.pingCancelled <- struct{}{}
			return errors.New("NAT punch attempt timed out")
		default:
		}

		var buf [512]byte
		n, err := conn.Read(buf[0:])
		if err != nil {
			log.Errorf(prefix, "Failed to read remote peer: %s cause: %s", conn.RemoteAddr().String(), err)
			time.Sleep(pingInterval * time.Millisecond)
			continue
		}
		fmt.Println("remote peer data received: ", string(buf[:n]))

		// send another couple of pings to remote side, because only now we have a pinghole
		// or wait for your pings to reach other end before closing pinger conn.
		select {
		case <-time.After(2 * pingInterval * time.Millisecond):
			p.pingCancelled <- struct{}{}
			return nil
		}
	}
}
