package main

import (
	"fmt"
	"log"
	"net"
	"time"

	"deepseekaiworker/internal/config"
	"deepseekaiworker/internal/mux"
)

// handleConnection establishes a mux, reads the client's REGISTER, and directs
// it to entry or NAT handling.
func handleConnection(conn net.Conn, rt *runtime) {
	client := conn.RemoteAddr().String()
	defer func() {
		conn.Close()
		log.Printf("client disconnected: %s", client)
	}()

	m := mux.Dial(conn)
	regBytes, err := m.Register()
	if err != nil {
		log.Printf("client %s: no register frame: %v", client, err)
		return
	}
	reg, err := mux.DecodeRegister(regBytes)
	if err != nil {
		log.Printf("client %s: bad register: %v", client, err)
		return
	}
	switch reg.Role {
	case mux.RoleEntry:
		handleEntry(m, conn, rt)
	case mux.RoleNat:
		handleNAT(m, conn, rt, reg)
	default:
		log.Printf("client %s: unknown role %q", client, reg.Role)
	}
}

func handleEntry(m *mux.Mux, conn net.Conn, rt *runtime) {
	client := conn.RemoteAddr().String()
	log.Printf("entry client connected: %s", client)

	m.SetOnOpen(func(s *mux.Stream, listenPort uint16) {
		rt.mu.RLock()
		route, ok := rt.cfg[int(listenPort)]
		dialTimeout := rt.dialTimeout
		rt.mu.RUnlock()
		if !ok {
			log.Printf("entry %s: unknown port %d", client, listenPort)
			s.Close()
			s.Release()
			return
		}
		switch {
		case route.Target != "":
			log.Printf("entry %s: port %d -> target %s", client, listenPort, route.Target)
			tc, err := net.DialTimeout("tcp", route.Target, dialTimeout)
			if err != nil {
				log.Printf("dial target %s: %v", route.Target, err)
				s.Close()
				s.Release()
				return
			}
			go s.Relay(tc)
		default:
			handleNATRoute(s, client, route, rt)
		}
	})

	rt.mu.RLock()
	entryCfg := rt.entryConfig
	rt.mu.RUnlock()
	if err := m.SendConfig(entryCfg); err != nil {
		log.Printf("send config to %s: %v", client, err)
		return
	}
	<-m.DeadChan()
}

// handleNATRoute bridges an entry stream to a NAT client's registered service.
func handleNATRoute(s *mux.Stream, client string, route config.Route, rt *runtime) {
	sess := rt.reg.get(route.NatClient)
	if sess == nil {
		log.Printf("entry %s: nat client %q not connected", client, route.NatClient)
		s.Close()
		s.Release()
		return
	}
	svc, ok := sess.byName[route.Service]
	if !ok {
		log.Printf("entry %s: nat client %q has no service %q", client, route.NatClient, route.Service)
		s.Close()
		s.Release()
		return
	}
	log.Printf("entry %s: port %d -> nat %s/%s (local port %d)", client, route.ListenPort, route.NatClient, route.Service, svc.Port)
	natStream, err := sess.m.Open(uint16(svc.Port))
	if err != nil {
		log.Printf("open stream to nat %s: %v", route.NatClient, err)
		s.Close()
		s.Release()
		return
	}
	// Bridge in a goroutine: OnOpen runs inside the mux readLoop, so it must
	// not block waiting for the bridge to finish (BridgeStreams waits for both
	// directions to close).
	go mux.BridgeStreams(s, natStream)
}

func handleNAT(m *mux.Mux, conn net.Conn, rt *runtime, reg mux.Register) {
	client := conn.RemoteAddr().String()
	if reg.ClientID == "" {
		log.Printf("NAT client %s: missing client_id", client)
		return
	}
	// read secret under lock (may be reloaded)
	var secret string
	var timeout time.Duration
	rt.mu.RLock()
	secret = rt.regSecrets[reg.ClientID]
	timeout = rt.dialTimeout
	rt.mu.RUnlock()
	if secret == "" {
		log.Printf("NAT client %s: unknown id %q", client, reg.ClientID)
		return
	}
	if secret != reg.Secret {
		log.Printf("NAT client %s: bad secret for %q", client, reg.ClientID)
		return
	}

	byName := map[string]mux.ServiceInfo{}
	byPort := map[int]string{}
	for _, svc := range reg.Services {
		byName[svc.Name] = svc
		addr := svc.Addr
		if addr == "" {
			addr = net.JoinHostPort("127.0.0.1", itoa(svc.Port))
		}
		byPort[svc.Port] = addr
	}

	sess := &natSession{addr: client, m: m, byName: byName, byPort: byPort}
	rt.reg.set(reg.ClientID, sess)
	defer rt.reg.remove(reg.ClientID, sess)

	m.SetOnOpen(func(s *mux.Stream, port uint16) {
		addr, ok := sess.byPort[int(port)]
		if !ok {
			log.Printf("NAT %s: unsolicited/open unknown service port %d", reg.ClientID, port)
			s.Close()
			return
		}
		log.Printf("NAT %s: serve local %s", reg.ClientID, addr)
		tc, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			log.Printf("NAT %s: dial local %s: %v", reg.ClientID, addr, err)
			s.Close()
			return
		}
		go s.Relay(tc)
	})

	<-m.DeadChan()
	log.Printf("NAT client %s connection closed", reg.ClientID)
}

func itoa(n int) string { return fmt.Sprint(n) }
