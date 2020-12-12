package moin

import (
	"context"
	_ "expvar"
	"fmt"
	"github.com/ipfs/go-bitswap"
	"github.com/ipfs/go-blockservice"
	"github.com/ipfs/go-cid"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	logging "github.com/ipfs/go-log"
	lwriter "github.com/ipfs/go-log/writer"
	"github.com/ipfs/go-merkledag"
	"github.com/libp2p/go-libp2p"
	connmgr "github.com/libp2p/go-libp2p-connmgr"
	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/event"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/peerstore"
	"github.com/libp2p/go-libp2p-core/protocol"
	"github.com/libp2p/go-libp2p-core/routing"
	gostream "github.com/libp2p/go-libp2p-gostream"
	ddht "github.com/libp2p/go-libp2p-kad-dht"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	noise "github.com/libp2p/go-libp2p-noise"
	"github.com/libp2p/go-libp2p-peerstore/pstoremem"
	libp2pquic "github.com/libp2p/go-libp2p-quic-transport"
	tls "github.com/libp2p/go-libp2p-tls"
	"github.com/libp2p/go-tcp-transport"
	ma "github.com/multiformats/go-multiaddr"
	"io"
	_ "net/http/pprof"
	"os"
	"sort"
	"time"
)

var (
	ProtocolPush protocol.ID = "/ipfs/push/1.0.0"
)

func (n *Node) Push(pid string, msg []byte) (int, error) {
	var id peer.ID

	var Timeout = time.Second * 3
	var num int
	var err error

	id, err = peer.Decode(pid)
	if err != nil {
		return 0, err
	}
	clientConn, err := gostream.Dial(context.Background(),
		n.Host, id, ProtocolPush)

	if err != nil {
		return 0, err
	}

	defer clientConn.Close()
	err = clientConn.SetDeadline(time.Now().Add(Timeout))
	if err != nil {
		return 0, err
	}

	num, err = clientConn.Write(msg)
	if err != nil && err != io.EOF {
		return 0, err
	}

	buf := make([]byte, 2)
	cum, err := clientConn.Read(buf)

	if err != nil && err != io.EOF {
		n.Listener.Error(err.Error())
		return 0, err
	}

	if string(buf[:cum]) != "ok" {
		return 0, err
	}

	return num, nil
}

func connected(n *Node, ctx context.Context) {
	// TODO change to EvtPeerConnectednessChanged
	subCompleted, err := n.Host.EventBus().Subscribe(new(event.EvtPeerIdentificationCompleted))
	defer subCompleted.Close()
	if err != nil {
		n.Listener.Error("failed to subscribe to identify notifications")
		return
	}
	for {
		select {
		case ev, ok := <-subCompleted.Out():
			if !ok {
				return
			}

			evt, ok := ev.(event.EvtPeerIdentificationCompleted)
			if !ok {
				return
			}
			n.Listener.Connected(evt.Peer.Pretty())

		case <-ctx.Done():
			return
		}
	}
}

func receiver(n *Node) {

	listener, _ := gostream.Listen(n.Host, ProtocolPush)
	defer listener.Close()
	var Timeout = time.Second * 3

	for {
		conn, err := listener.Accept()

		if err != nil {
			n.Listener.Error(err.Error())
			return
		}

		pid := conn.RemoteAddr().String()
		if !n.Shutdown {

			if n.Listener.AllowConnect(pid) {

				buf := make([]byte, 59)
				err = conn.SetDeadline(time.Now().Add(Timeout))
				if err != nil {
					n.Listener.Error(err.Error())
				}
				num, err := conn.Read(buf)
				if err != nil && err != io.EOF {
					n.Listener.Error(err.Error())
				} else {
					if n.Pushing {
						id, err := cid.Decode(string(buf[:num]))
						if err == nil {
							n.Listener.Push(id.String(), pid)
							_, err = conn.Write([]byte("ok"))
							if err != nil {
								n.Listener.Error(err.Error())
							}
						} else {
							_, err = conn.Write([]byte("ko"))
							if err != nil {
								n.Listener.Error(err.Error())
							}
						}
					} else {
						_, err = conn.Write([]byte("ko"))
						if err != nil {
							n.Listener.Error(err.Error())
						}
					}
				}
			}
			err = conn.Close()
			if err != nil {
				n.Listener.Error(err.Error())
			}
		} else {
			return
		}
	}
}


func logs(ctx context.Context) {

	logging.SetupLogging()
	logging.SetDebugLogging()

	w := os.Stdout;

	go func() {
		defer w.Close()
		<-ctx.Done()
	}()
	lwriter.WriterGroup.AddWriter(w)

}

func (n *Node) Daemon(agent string, ) error {

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n.Listener.Info("Initializing daemon...")

	var Swarm []string

	Swarm = append(Swarm, fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", n.Port))
	Swarm = append(Swarm, fmt.Sprintf("/ip6/::/tcp/%d", n.Port))
	Swarm = append(Swarm, fmt.Sprintf("/ip4/0.0.0.0/udp/%d/quic", n.Port))
	Swarm = append(Swarm, fmt.Sprintf("/ip6/::/udp/%d/quic", n.Port))

	var err error
	mAddresses, err := listenAddresses(Swarm)
	if err != nil {
		return err
	}

	id, err := peer.Decode(n.PeerID)
	if err != nil {
		return fmt.Errorf("invalid peer id")
	}

	sk, err := DecodePrivateKey(n.PrivateKey)
	if err != nil {
		return err
	}

	n.PeerStore = pstoremem.NewPeerstore()

	err = pstoreAddSelfKeys(id, sk, n.PeerStore)
	if err != nil {
		return err
	}

	// hash security
	bs := blockstore.NewBlockstore(n.DataStore)

	n.BlockStore = blockstore.NewIdStore(bs)

	grace, err := time.ParseDuration(n.GracePeriod)
	if err != nil {
		return fmt.Errorf("parsing Swarm.ConnMgr.GracePeriod: %s", err)
	}
	n.ConnectionManager = connmgr.NewConnManager(n.LowWater, n.HighWater, grace)

	// HOST and Routing
	var opts []libp2p.Option
	opts = append(opts, libp2p.ListenAddrs(mAddresses...))
	opts = append(opts, libp2p.UserAgent(agent))
	opts = append(opts, libp2p.ChainOptions(libp2p.Security(tls.ID, tls.New), libp2p.Security(noise.ID, noise.New)))
	opts = append(opts, libp2p.ConnectionManager(n.ConnectionManager))
	opts = append(opts, libp2p.Transport(libp2pquic.NewTransport))
	opts = append(opts, libp2p.Transport(tcp.NewTCPTransport))
	opts = append(opts, libp2p.Ping(false))

	opts = append(opts, libp2p.ChainOptions(libp2p.EnableAutoRelay(), libp2p.DefaultStaticRelays()))

	//opts = append(opts, libp2p.EnableNATService())

	/*
		interval := time.Minute


		opts = append(opts,
			libp2p.AutoNATServiceRateLimit(30, 3, interval),
		)*/

	// Let this host use the DHT to find other hosts
	opts = append(opts, libp2p.Routing(func(host host.Host) (routing.PeerRouting, error) {

		n.Routing, err = ddht.New(
			ctx, host,
			dht.Concurrency(10),
			dht.DisableAutoRefresh(),
			dht.Mode(dht.ModeClient))

		return n.Routing, err
	}))

	n.Host, err = constructPeerHost(ctx, id, n.PeerStore, opts)

	bitSwapNetwork := NewMoinHost(n.Host, n.Listener)
	exchange := bitswap.New(ctx, bitSwapNetwork, bs,
		bitswap.ProvideEnabled(false))

	n.BlockService = blockservice.New(n.BlockStore, exchange)
	n.DagService = merkledag.NewDAGService(n.BlockService)

	if err != nil {
		return fmt.Errorf("constructPeerHost: %s", err)
	}

	n.Listener.Info("New Node...")

	printSwarmAddrs(n)

	n.Listener.Info("Daemon is ready")

	n.Running = true
	n.Shutdown = false



	go receiver(n)
	go connected(n, ctx)

	if n.Debug {
		go logs(ctx)
	}

	for {
		if n.Shutdown {
			n.Running = false
			n.Listener.Info("Daemon is shutdown")
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}

	return nil
}

func printSwarmAddrs(n *Node) {

	var lisAddrs []string
	ifaceAddrs, err := n.Host.Network().InterfaceListenAddresses()
	if err != nil {
		n.Listener.Error(fmt.Sprintf("failed to read listening addresses: %s", err))
	}
	for _, addr := range ifaceAddrs {
		lisAddrs = append(lisAddrs, addr.String())
	}
	sort.Strings(lisAddrs)
	for _, addr := range lisAddrs {
		n.Listener.Info(fmt.Sprintf("Swarm listening on %s", addr))
	}

	var addrs []string
	for _, addr := range n.Host.Addrs() {
		addrs = append(addrs, addr.String())
	}
	sort.Strings(addrs)
	for _, addr := range addrs {
		n.Listener.Info(fmt.Sprintf("Swarm announcing %s", addr))
	}

}

func listenAddresses(addresses []string) ([]ma.Multiaddr, error) {
	var listen []ma.Multiaddr
	for _, addr := range addresses {
		maddr, err := ma.NewMultiaddr(addr)
		if err != nil {
			return nil, fmt.Errorf("failure to parse config.Addresses.Swarm: %s", addresses)
		}
		listen = append(listen, maddr)
	}

	return listen, nil
}

func pstoreAddSelfKeys(id peer.ID, sk crypto.PrivKey, ps peerstore.Peerstore) error {
	if err := ps.AddPubKey(id, sk.GetPublic()); err != nil {
		return err
	}

	return ps.AddPrivKey(id, sk)
}

func constructPeerHost(ctx context.Context, id peer.ID, ps peerstore.Peerstore, options []libp2p.Option) (host.Host, error) {
	pkey := ps.PrivKey(id)
	if pkey == nil {
		return nil, fmt.Errorf("missing private key for node ID: %s", id.Pretty())
	}
	options = append([]libp2p.Option{libp2p.Identity(pkey), libp2p.Peerstore(ps)}, options...)

	return libp2p.New(ctx, options...)
}
