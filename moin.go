package moin

import (
	"context"
	"errors"
	"fmt"
	"github.com/libp2p/go-libp2p-core/network"
	"io"
	"sync/atomic"
	"time"

	bsmsg "github.com/ipfs/go-bitswap/message"
	bsmnet "github.com/ipfs/go-bitswap/network"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p-core/connmgr"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/protocol"
	"github.com/libp2p/go-libp2p/p2p/protocol/ping"
	"github.com/libp2p/go-msgio"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/multiformats/go-multistream"
)

var sendMessageTimeout = time.Minute * 10


var (
	ProtocolMoin protocol.ID = "/ipfs/moin/1.0.0"
)
// NewMoinHost returns a BitSwapNetwork supported by underlying IPFS host.
func NewMoinHost(host host.Host, listener Listener) bsmnet.BitSwapNetwork {

	bitswapNetwork := impl{
		host:    host,
		listener: listener,
		protocolMoin:           ProtocolMoin,
		supportedProtocols: []protocol.ID{ ProtocolMoin },
	}

	return &bitswapNetwork
}


// impl transforms the ipfs network interface, which sends and receives
// NetMessage objects, into the bitswap network interface.
type impl struct {
	// NOTE: Stats must be at the top of the heap allocation to ensure 64bit
	// alignment.
	stats bsmnet.Stats
    listener Listener
	host          host.Host
	connectEvtMgr *connectEventManager

	protocolMoin           protocol.ID

	supportedProtocols []protocol.ID

	// inbound messages from the network are forwarded to the receiver
	receiver bsmnet.Receiver
}

type streamMessageSender struct {
	listener Listener
	to        peer.ID
	stream    network.Stream
	connected bool
	bsnet     *impl
	opts      *bsmnet.MessageSenderOpts
}

// Open a stream to the remote peer
func (s *streamMessageSender) Connect(ctx context.Context) (network.Stream, error) {
	if s.connected {
		return s.stream, nil
	}

	tctx, cancel := context.WithTimeout(ctx, s.opts.SendTimeout)
	defer cancel()

	if err := s.bsnet.ConnectTo(tctx, s.to); err != nil {
		return nil, err
	}

	stream, err := s.bsnet.newStreamToPeer(tctx, s.to)
	if err != nil {
		return nil, err
	}

	s.stream = stream
	s.connected = true
	return s.stream, nil
}

// Reset the stream
func (s *streamMessageSender) Reset() error {
	if s.stream != nil {
		err := s.stream.Reset()
		s.connected = false
		return err
	}
	return nil
}

// Close the stream
func (s *streamMessageSender) Close() error {
	return s.stream.Close();
}

// Indicates whether the peer supports HAVE / DONT_HAVE messages
func (s *streamMessageSender) SupportsHave() bool {
	return s.bsnet.SupportsHave(s.stream.Protocol())
}

// Send a message to the peer, attempting multiple times
func (s *streamMessageSender) SendMsg(ctx context.Context, msg bsmsg.BitSwapMessage) error {
	return s.multiAttempt(ctx, func() error {
		return s.send(ctx, msg)
	})
}

// Perform a function with multiple attempts, and a timeout
func (s *streamMessageSender) multiAttempt(ctx context.Context, fn func() error) error {
	// Try to call the function repeatedly
	var err error
	for i := 0; i < s.opts.MaxRetries; i++ {
		if err = fn(); err == nil {
			// Attempt was successful
			return nil
		}

		// Attempt failed


		// If the sender has been closed or the context cancelled, just bail out
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Protocol is not supported, so no need to try multiple times
		if errors.Is(err, multistream.ErrNotSupported) {
			s.bsnet.connectEvtMgr.MarkUnresponsive(s.to)
			return err
		}

		// Failed to send so reset stream and try again
		_ = s.Reset()

		// Failed too many times so mark the peer as unresponsive and return an error
		if i == s.opts.MaxRetries-1 {
			s.bsnet.connectEvtMgr.MarkUnresponsive(s.to)
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.opts.SendErrorBackoff):
			// wait a short time in case disconnect notifications are still propagating
			s.listener.Error(fmt.Sprintf("send message to %s failed but context was not Done: %s", s.to, err))
		}
	}
	return err
}

// Send a message to the peer
func (s *streamMessageSender) send(ctx context.Context, msg bsmsg.BitSwapMessage) error {
	start := time.Now()
	stream, err := s.Connect(ctx)
	if err != nil {
		s.listener.Error(fmt.Sprintf("failed to open stream to %s: %s", s.to, err));
		return err
	}

	// The send timeout includes the time required to connect
	// (although usually we will already have connected - we only need to
	// connect after a failed attempt to send)
	timeout := s.opts.SendTimeout - time.Since(start)
	if err = s.bsnet.msgToStream(ctx, stream, msg, timeout); err != nil {
		s.listener.Error(fmt.Sprintf("failed to send message to %s: %s", s.to, err))
		return err
	}

	return nil
}

func (bsnet *impl) Self() peer.ID {
	return bsnet.host.ID()
}

func (bsnet *impl) Ping(ctx context.Context, p peer.ID) ping.Result {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	res := <-ping.Ping(ctx, bsnet.host, p)
	return res
}

func (bsnet *impl) Latency(p peer.ID) time.Duration {
	return bsnet.host.Peerstore().LatencyEWMA(p)
}

// Indicates whether the given protocol supports HAVE / DONT_HAVE messages
func (bsnet *impl) SupportsHave(proto protocol.ID) bool {
	return true
}

func (bsnet *impl) msgToStream(ctx context.Context, s network.Stream, msg bsmsg.BitSwapMessage, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}

	if err := s.SetWriteDeadline(deadline); err != nil {
		bsnet.listener.Error(fmt.Sprintf("error setting deadline: %s", err))
	}

	// Older Bitswap versions use a slightly different wire format so we need
	// to convert the message to the appropriate format depending on the remote
	// peer's Bitswap version.
	switch s.Protocol() {
	case bsnet.protocolMoin:
		if err := msg.ToNetV1(s); err != nil {
			bsnet.listener.Error(fmt.Sprintf("error: %s", err))
			return err
		}
	default:
		return fmt.Errorf("unrecognized protocol on remote: %s", s.Protocol())
	}

	if err := s.SetWriteDeadline(time.Time{}); err != nil {
		bsnet.listener.Error(fmt.Sprintf("error resetting deadline: %s", err))
	}
	return nil
}


func (bsnet *impl) NewMessageSender(ctx context.Context, p peer.ID, opts *bsmnet.MessageSenderOpts) (bsmnet.MessageSender, error) {

	if !bsnet.listener.AllowConnect(p.String()) {
		return nil, UnknownUserError("unknown user")
	}

	opts = setDefaultOpts(opts)

	sender := &streamMessageSender{
		listener: bsnet.listener,
		to:    p,
		bsnet: bsnet,
		opts:  opts,
	}

	err := sender.multiAttempt(ctx, func() error {
		_, err := sender.Connect(ctx)
		return err
	})

	if err != nil {
		return nil, err
	}

	return sender, nil
}

func setDefaultOpts(opts *bsmnet.MessageSenderOpts) *bsmnet.MessageSenderOpts {
	copy := *opts
	if opts.MaxRetries == 0 {
		copy.MaxRetries = 3
	}
	if opts.SendTimeout == 0 {
		copy.SendTimeout = sendMessageTimeout
	}
	if opts.SendErrorBackoff == 0 {
		copy.SendErrorBackoff = 100 * time.Millisecond
	}
	return &copy
}

func (bsnet *impl) SendMessage(
	ctx context.Context,
	p peer.ID,
	outgoing bsmsg.BitSwapMessage) error {

	if !bsnet.listener.AllowConnect(p.String()) {
		return UnknownUserError("unknown user")
	}

	s, err := bsnet.newStreamToPeer(ctx, p)
	if err != nil {
		return err
	}

	if err = bsnet.msgToStream(ctx, s, outgoing, sendMessageTimeout); err != nil {
		_ = s.Reset()
		return err
	}

	return s.Close()
}

func (bsnet *impl) newStreamToPeer(ctx context.Context, p peer.ID) (network.Stream, error) {
	return bsnet.host.NewStream(ctx, p, bsnet.supportedProtocols...)
}

func (bsnet *impl) SetDelegate(r bsmnet.Receiver) {
	bsnet.receiver = r
	bsnet.connectEvtMgr = newConnectEventManager(r)
	for _, proto := range bsnet.supportedProtocols {
		bsnet.host.SetStreamHandler(proto, bsnet.handleNewStream)
	}
	bsnet.host.Network().Notify((*netNotifiee)(bsnet))
	// TODO: StopNotify.

}

type UnknownUserError string

func (err UnknownUserError) Error() string {
	return string(err)
}

func (bsnet *impl) ConnectTo(ctx context.Context, p peer.ID) error {
	if bsnet.listener.AllowConnect(p.String()) {
		return bsnet.host.Connect(ctx, peer.AddrInfo{ID: p})
	}
	return UnknownUserError("unknown user")
}

func (bsnet *impl) DisconnectFrom(ctx context.Context, p peer.ID) error {
	panic("Not implemented: DisconnectFrom() is only used by tests")
}

// FindProvidersAsync returns a channel of providers for the given key.
func (bsnet *impl) FindProvidersAsync(ctx context.Context, k cid.Cid, max int) <-chan peer.ID {

	return nil
}

// Provide provides the key to the network
func (bsnet *impl) Provide(ctx context.Context, k cid.Cid) error {
	return nil
}

// handleNewStream receives a new stream from the network.
func (bsnet *impl) handleNewStream(s network.Stream) {
	defer s.Close()

	if bsnet.receiver == nil {
		_ = s.Reset()
		return
	}

	reader := msgio.NewVarintReaderSize(s, network.MessageSizeMax)
	for {
		received, err := bsmsg.FromMsgReader(reader)
		if err != nil {
			if err != io.EOF {
				_ = s.Reset()
				bsnet.receiver.ReceiveError(err)
				_ = fmt.Sprintf("bitswap net handleNewStream from %s error: %s", s.Conn().RemotePeer(), err)
			}
			return
		}

		p := s.Conn().RemotePeer()
		if bsnet.listener.AllowConnect(p.String()) {
			ctx := context.Background()
			bsnet.connectEvtMgr.OnMessage(s.Conn().RemotePeer())
			bsnet.receiver.ReceiveMessage(ctx, p, received)
			atomic.AddUint64(&bsnet.stats.MessagesRecvd, 1)
		}
	}
}

func (bsnet *impl) ConnectionManager() connmgr.ConnManager {
	return bsnet.host.ConnManager()
}

func (bsnet *impl) Stats() bsmnet.Stats {
	return bsmnet.Stats {
		MessagesRecvd: atomic.LoadUint64(&bsnet.stats.MessagesRecvd),
		MessagesSent:  atomic.LoadUint64(&bsnet.stats.MessagesSent),
	}
}

type netNotifiee impl

func (nn *netNotifiee) impl() *impl {
	return (*impl)(nn)
}

func (nn *netNotifiee) Connected(n network.Network, v network.Conn) {
	nn.impl().connectEvtMgr.Connected(v.RemotePeer())
}
func (nn *netNotifiee) Disconnected(n network.Network, v network.Conn) {
	nn.impl().connectEvtMgr.Disconnected(v.RemotePeer())
}
func (nn *netNotifiee) OpenedStream(n network.Network, s network.Stream) {}
func (nn *netNotifiee) ClosedStream(n network.Network, v network.Stream) {}
func (nn *netNotifiee) Listen(n network.Network, a ma.Multiaddr)         {}
func (nn *netNotifiee) ListenClose(n network.Network, a ma.Multiaddr)    {}
