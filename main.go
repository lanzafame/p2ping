package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	mrand "math/rand"
	"os"
	"sync"
	"time"

	ds "github.com/ipfs/go-datastore"
	dsync "github.com/ipfs/go-datastore/sync"
	logging "github.com/ipfs/go-log/v2"
	path "github.com/ipfs/go-path"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/peerstore"
	"github.com/libp2p/go-libp2p-core/routing"
	"github.com/libp2p/go-libp2p/p2p/protocol/ping"

	dht "github.com/libp2p/go-libp2p-kad-dht"
	rhost "github.com/libp2p/go-libp2p/p2p/host/routed"
	ma "github.com/multiformats/go-multiaddr"

	cli "github.com/urfave/cli/v2"
)

var log =logging .Logger("pinger")

func main() {
	app := &cli.App{
		Name: "p2ping",
		Usage: "[flags]",
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name: "l",
				Value: 5342,
				Usage: "wait for incoming connections",
				Required: true,
			},
			&cli.StringFlag{
				Name: "d",
				Value: "",
				Usage: "target peer to dial",
			},
			&cli.Int64Flag{
				Name: "seed",
				Value: 0,
				Usage: "set random seed for id generation",
			},
		},
		Action: runCmd,
	}

	err := app.Run(os.Args)
	if err != nil {
		log.Fatal(err)
	}
}

func runCmd(c *cli.Context) error {
	ctx := context.Background()

	logging .SetAllLoggers(logging .LevelInfo) // Change to INFO for extra info

	// Parse options from the command line
	listenF := c.Int("l")
	target := c.String("d")
	seed := c.Int64("seed")

	var bootstrapPeers []peer.AddrInfo
	for _, addr := range dht.DefaultBootstrapPeers {
		pi, _ := peer.AddrInfoFromP2pAddr(addr)
		bootstrapPeers = append(bootstrapPeers, *pi)
	}
	ha, idht, err := makeRoutedHost(listenF, seed, bootstrapPeers)
	if err != nil {
		return err
	}

	peerid, err := peer.Decode(target)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	ctx, events := routing.RegisterForQueryEvents(ctx)

	//dht := ha.DHT.WAN
	//if !ha.DHT.WANActive() {
	//	dht = ha.DHT.LAN
	//}

	errCh := make(chan error, 1)
	go func() {
		defer close(errCh)
		defer cancel()
		closestPeers, err := idht.GetClosestPeers(ctx, string(peerid))
		if closestPeers != nil {
			for _, p := range closestPeers {
				routing.PublishQueryEvent(ctx, &routing.QueryEvent{
					ID:   p,
					Type: routing.FinalPeer,
				})
			}
		}

		if err != nil {
			errCh <- err
			return
		}
	}()

	pfm := pfuncMap{
		routing.FinalPeer: func(obj *routing.QueryEvent, out io.Writer, verbose bool) error {
			fmt.Fprintf(out, "%s\n", obj.ID)
			return nil
		},
	}

	for e := range events {
		printEvent(e, os.Stdout, true, pfm)
	}

	// need a new ctx so that ping actually runs.
	ctx = context.Background()

	res := ping.Ping(ctx, ha, peerid)
	select {
	case out := <-res:
		log.Infow("ping", "result", out)
	}

	return err
}

// makeRoutedHost creates a LibP2P host with a random peer ID listening on the
// given multiaddress. It will bootstrap using the provided PeerInfo.
func makeRoutedHost(listenPort int, randseed int64, bootstrapPeers []peer.AddrInfo) (host.Host, *dht.IpfsDHT, error) {
	// If the seed is zero, use real cryptographic randomness. Otherwise, use a
	// deterministic randomness source to make generated keys stay the same
	// across multiple runs
	var r io.Reader
	if randseed == 0 {
		r = rand.Reader
	} else {
		r = mrand.New(mrand.NewSource(randseed))
	}

	// Generate a key pair for this host. We will use it at least
	// to obtain a valid host ID.
	priv, _, err := crypto.GenerateKeyPairWithReader(crypto.RSA, 2048, r)
	if err != nil {
		return nil, nil, err
	}

	opts := []libp2p.Option{
		libp2p.ListenAddrStrings(fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", listenPort)),
		libp2p.Identity(priv),
		libp2p.DefaultTransports,
		libp2p.DefaultMuxers,
		libp2p.DefaultSecurity,
		libp2p.NATPortMap(),
	}

	ctx := context.Background()

	basicHost, err := libp2p.New(ctx, opts...)
	if err != nil {
		return nil, nil, err
	}

	// Construct a datastore (needed by the DHT). This is just a simple, in-memory thread-safe datastore.
	dstore := dsync.MutexWrap(ds.NewMapDatastore())

	// Make the DHT
	idht := dht.NewDHT(ctx, basicHost, dstore)

	// Make the routed host
	routedHost := rhost.Wrap(basicHost, idht)

	// connect to the chosen ipfs nodes
	err = bootstrapConnect(ctx, routedHost, bootstrapPeers)
	if err != nil {
		return nil, nil, err
	}

	// Bootstrap the host
	err = idht.Bootstrap(ctx)
	if err != nil {
		return nil, nil, err
	}

	// Build host multiaddress
	hostAddr, _ := ma.NewMultiaddr(fmt.Sprintf("/p2p/%s", routedHost.ID().Pretty()))

	// Now we can build a full multiaddress to reach this host
	// by encapsulating both addresses:
	// addr := routedHost.Addrs()[0]
	addrs := routedHost.Addrs()
	for _, addr := range addrs {
		log.Infow("ping host addr", "addr", addr.Encapsulate(hostAddr))
	}

	return routedHost, idht, nil
}

// Borrowed from ipfs code to parse the results of the command `ipfs id`
type IdOutput struct {
	ID              string
	PublicKey       string
	Addresses       []string
	AgentVersion    string
	ProtocolVersion string
}

// This code is borrowed from the go-ipfs bootstrap process
func bootstrapConnect(ctx context.Context, ph host.Host, peers []peer.AddrInfo) error {
	if len(peers) < 1 {
		return errors.New("not enough bootstrap peers")
	}

	errs := make(chan error, len(peers))
	var wg sync.WaitGroup
	for _, p := range peers {

		// performed asynchronously because when performed synchronously, if
		// one `Connect` call hangs, subsequent calls are more likely to
		// fail/abort due to an expiring context.
		// Also, performed asynchronously for dial speed.

		wg.Add(1)
		go func(p peer.AddrInfo) {
			defer wg.Done()
			defer log.Infow("bootstrapDial", "self", ph.ID(), "peer", p.ID)
			log.Infow("bootstrapping", "self", ph.ID(), "peer", p.ID)

			ph.Peerstore().AddAddrs(p.ID, p.Addrs, peerstore.PermanentAddrTTL)
			if err := ph.Connect(ctx, p); err != nil {
				log.Errorw("failed to bootstrap with peer", "peer", p.ID, "error", err)
				errs <- err
				return
			}
			log.Infow("bootstrapped with peer", "peer", p.ID)
		}(p)
	}
	wg.Wait()

	// our failure condition is when no connection attempt succeeded.
	// So drain the errs channel, counting the results.
	close(errs)
	count := 0
	var err error
	for err = range errs {
		if err != nil {
			count++
		}
	}
	if count == len(peers) {
		return fmt.Errorf("failed to bootstrap. %s", err)
	}
	return nil
}

type printFunc func(obj *routing.QueryEvent, out io.Writer, verbose bool) error
type pfuncMap map[routing.QueryEventType]printFunc

func printEvent(obj *routing.QueryEvent, out io.Writer, verbose bool, override pfuncMap) error {
	if verbose {
		fmt.Fprintf(out, "%s: ", time.Now().Format("15:04:05.000"))
	}

	if override != nil {
		if pf, ok := override[obj.Type]; ok {
			return pf(obj, out, verbose)
		}
	}

	switch obj.Type {
	case routing.SendingQuery:
		if verbose {
			fmt.Fprintf(out, "* querying %s\n", obj.ID)
		}
	case routing.Value:
		if verbose {
			fmt.Fprintf(out, "got value: '%s'\n", obj.Extra)
		} else {
			fmt.Fprint(out, obj.Extra)
		}
	case routing.PeerResponse:
		if verbose {
			fmt.Fprintf(out, "* %s says use ", obj.ID)
			for _, p := range obj.Responses {
				fmt.Fprintf(out, "%s ", p.ID)
			}
			fmt.Fprintln(out)
		}
	case routing.QueryError:
		if verbose {
			fmt.Fprintf(out, "error: %s\n", obj.Extra)
		}
	case routing.DialingPeer:
		if verbose {
			fmt.Fprintf(out, "dialing peer: %s\n", obj.ID)
		}
	case routing.AddingPeer:
		if verbose {
			fmt.Fprintf(out, "adding peer to query: %s\n", obj.ID)
		}
	case routing.FinalPeer:
	default:
		if verbose {
			fmt.Fprintf(out, "unrecognized event type: %d\n", obj.Type)
		}
	}
	return nil
}

func escapeDhtKey(s string) (string, error) {
	parts := path.SplitList(s)
	if len(parts) != 3 ||
		parts[0] != "" ||
		!(parts[1] == "ipns" || parts[1] == "pk") {
		return "", errors.New("invalid key")
	}

	k, err := peer.Decode(parts[2])
	if err != nil {
		return "", err
	}
	return path.Join(append(parts[:2], string(k))), nil
}
