package autonat

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"time"

	host "github.com/libp2p/go-libp2p-host"
	inet "github.com/libp2p/go-libp2p-net"
	peer "github.com/libp2p/go-libp2p-peer"
	ma "github.com/multiformats/go-multiaddr"
)

// NATStatus is the state of NAT as detected by the ambient service.
type NATStatus int

const (
	// NAT status is unknown; this means that the ambient service has not been
	// able to decide the presence of NAT in the most recent attempt to test
	// dial through known autonat peers.  initial state.
	NATStatusUnknown NATStatus = iota
	// NAT status is publicly dialable
	NATStatusPublic
	// NAT status is private network
	NATStatusPrivate
)

var (
	AutoNATBootDelay       = 15 * time.Second
	AutoNATRetryInterval   = 60 * time.Second
	AutoNATRefreshInterval = 15 * time.Minute
	AutoNATRequestTimeout  = 60 * time.Second
)

// AutoNAT is the interface for ambient NAT autodiscovery
type AutoNAT interface {
	// Status returns the current NAT status
	Status() NATStatus
	// PublicAddr returns the public dial address when NAT status is public and an
	// error otherwise
	PublicAddr() (ma.Multiaddr, error)
}

// AmbientAutoNAT is the implementation of ambient NAT autodiscovery
type AmbientAutoNAT struct {
	ctx  context.Context
	host host.Host

	mx         sync.Mutex
	peers      map[peer.ID]struct{}
	status     NATStatus
	addr       ma.Multiaddr
	confidence int
}

// NewAutoNAT creates a new ambient NAT autodiscovery instance attached to a host
func NewAutoNAT(ctx context.Context, h host.Host) AutoNAT {
	as := &AmbientAutoNAT{
		ctx:    ctx,
		host:   h,
		peers:  make(map[peer.ID]struct{}),
		status: NATStatusUnknown,
	}

	h.Network().Notify(as)
	go as.background()

	return as
}

func (as *AmbientAutoNAT) Status() NATStatus {
	return as.status
}

func (as *AmbientAutoNAT) PublicAddr() (ma.Multiaddr, error) {
	as.mx.Lock()
	defer as.mx.Unlock()

	if as.status != NATStatusPublic {
		return nil, errors.New("NAT Status is not public")
	}

	return as.addr, nil
}

func (as *AmbientAutoNAT) background() {
	// wait a bit for the node to come online and establish some connections
	// before starting autodetection
	select {
	case <-time.After(AutoNATBootDelay):
	case <-as.ctx.Done():
		return
	}

	for {
		as.autodetect()

		delay := AutoNATRefreshInterval
		if as.status == NATStatusUnknown {
			delay = AutoNATRetryInterval
		}

		select {
		case <-time.After(delay):
		case <-as.ctx.Done():
			return
		}
	}
}

func (as *AmbientAutoNAT) autodetect() {
	peers := as.getPeers()

	if len(peers) == 0 {
		log.Debugf("skipping NAT auto detection; no autonat peers")
		return
	}

	cli := NewAutoNATClient(as.host)
	failures := 0

	for _, p := range peers {
		ctx, cancel := context.WithTimeout(as.ctx, AutoNATRequestTimeout)
		a, err := cli.DialBack(ctx, p)
		cancel()

		switch {
		case err == nil:
			log.Debugf("NAT status is public; address through %s: %s", p.Pretty(), a.String())
			as.mx.Lock()
			as.addr = a
			as.status = NATStatusPublic
			as.confidence = 0
			as.mx.Unlock()
			return

		case IsDialError(err):
			log.Debugf("dial error through %s: %s", p.Pretty(), err.Error())
			failures++
			if failures >= 3 || as.confidence >= 3 { // 3 times is enemy action
				log.Debugf("NAT status is private")
				as.mx.Lock()
				as.status = NATStatusPrivate
				as.confidence = 3
				as.mx.Unlock()
				return
			}

		default:
			log.Debugf("Error dialing through %s: %s", p.Pretty(), err.Error())
		}
	}

	as.mx.Lock()
	if failures > 0 {
		as.status = NATStatusPrivate
		as.confidence++
		log.Debugf("NAT status is private")
	} else {
		as.status = NATStatusUnknown
		as.confidence = 0
		log.Debugf("NAT status is unknown")
	}
	as.mx.Unlock()
}

func (as *AmbientAutoNAT) getPeers() []peer.ID {
	as.mx.Lock()
	defer as.mx.Unlock()

	if len(as.peers) == 0 {
		return nil
	}

	peers := make([]peer.ID, 0, len(as.peers))
	for p := range as.peers {
		if as.host.Network().Connectedness(p) == inet.Connected {
			peers = append(peers, p)
		}
	}

	if len(peers) == 0 {
		// we don't have any open connections, try any autonat peer that we know about
		for p := range as.peers {
			peers = append(peers, p)
		}
	}

	shufflePeers(peers)

	return peers
}

func shufflePeers(peers []peer.ID) {
	for i := range peers {
		j := rand.Intn(i + 1)
		peers[i], peers[j] = peers[j], peers[i]
	}
}
