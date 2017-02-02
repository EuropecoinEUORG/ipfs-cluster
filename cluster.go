package ipfscluster

import (
	"context"
	"fmt"
	"sync"
	"time"

	rpc "github.com/hsanjuan/go-libp2p-gorpc"
	cid "github.com/ipfs/go-cid"
	host "github.com/libp2p/go-libp2p-host"
	peer "github.com/libp2p/go-libp2p-peer"
	peerstore "github.com/libp2p/go-libp2p-peerstore"
	swarm "github.com/libp2p/go-libp2p-swarm"
	basichost "github.com/libp2p/go-libp2p/p2p/host/basic"
	ma "github.com/multiformats/go-multiaddr"
)

// Cluster is the main IPFS cluster component. It provides
// the go-API for it and orchestrates the components that make up the system.
type Cluster struct {
	ctx context.Context

	id          peer.ID
	config      *Config
	host        host.Host
	rpcServer   *rpc.Server
	rpcClient   *rpc.Client
	peerManager *peerManager

	consensus *Consensus
	api       API
	ipfs      IPFSConnector
	state     State
	tracker   PinTracker

	shutdownLock sync.Mutex
	shutdown     bool
	shutdownCh   chan struct{}
	doneCh       chan struct{}
	readyCh      chan struct{}
	wg           sync.WaitGroup

	paMux sync.Mutex
}

// NewCluster builds a new IPFS Cluster peer. It initializes a LibP2P host,
// creates and RPC Server and client and sets up all components.
//
// The new cluster peer may still be performing initialization tasks when
// this call returns (consensus may still be bootstrapping). Use Cluster.Ready()
// if you need to wait until the peer is fully up.
func NewCluster(cfg *Config, api API, ipfs IPFSConnector, state State, tracker PinTracker) (*Cluster, error) {
	ctx := context.Background()
	host, err := makeHost(ctx, cfg)
	if err != nil {
		return nil, err
	}

	logger.Infof("IPFS Cluster v%s listening on:", Version)
	for _, addr := range host.Addrs() {
		logger.Infof("        %s/ipfs/%s", addr, host.ID().Pretty())
	}

	c := &Cluster{
		ctx:        ctx,
		id:         host.ID(),
		config:     cfg,
		host:       host,
		api:        api,
		ipfs:       ipfs,
		state:      state,
		tracker:    tracker,
		shutdownCh: make(chan struct{}, 1),
		doneCh:     make(chan struct{}, 1),
		readyCh:    make(chan struct{}, 1),
	}

	c.setupPeerManager()
	err = c.setupRPC()
	if err != nil {
		c.Shutdown()
		return nil, err
	}

	err = c.setupConsensus()
	if err != nil {
		c.Shutdown()
		return nil, err
	}
	c.setupRPCClients()
	c.run()
	return c, nil
}

func (c *Cluster) setupPeerManager() {
	pm := newPeerManager(c)
	c.peerManager = pm
	if len(c.config.ClusterPeers) > 0 {
		c.peerManager.addFromMultiaddrs(c.config.ClusterPeers)
	} else {
		c.peerManager.addFromMultiaddrs(c.config.Bootstrap)
	}

}

func (c *Cluster) setupRPC() error {
	rpcServer := rpc.NewServer(c.host, RPCProtocol)
	err := rpcServer.RegisterName("Cluster", &RPCAPI{cluster: c})
	if err != nil {
		return err
	}
	c.rpcServer = rpcServer
	rpcClient := rpc.NewClientWithServer(c.host, RPCProtocol, rpcServer)
	c.rpcClient = rpcClient
	return nil
}

func (c *Cluster) setupConsensus() error {
	var startPeers []peer.ID
	if len(c.config.ClusterPeers) > 0 {
		startPeers = peersFromMultiaddrs(c.config.ClusterPeers)
	} else {
		startPeers = peersFromMultiaddrs(c.config.Bootstrap)
	}

	consensus, err := NewConsensus(
		append(startPeers, c.host.ID()),
		c.host,
		c.config.ConsensusDataFolder,
		c.state)
	if err != nil {
		logger.Errorf("error creating consensus: %s", err)
		return err
	}
	c.consensus = consensus
	return nil
}

func (c *Cluster) setupRPCClients() {
	c.tracker.SetClient(c.rpcClient)
	c.ipfs.SetClient(c.rpcClient)
	c.api.SetClient(c.rpcClient)
	c.consensus.SetClient(c.rpcClient)
}

func (c *Cluster) stateSyncWatcher() {
	stateSyncTicker := time.NewTicker(
		time.Duration(c.config.StateSyncSeconds) * time.Second)
	for {
		select {
		case <-stateSyncTicker.C:
			c.StateSync()
		case <-c.ctx.Done():
			stateSyncTicker.Stop()
			return
		}
	}
}

// run provides a cancellable context and launches some goroutines
// before signaling readyCh
func (c *Cluster) run() {
	c.wg.Add(1)
	// cancellable context
	go func() {
		defer c.wg.Done()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c.ctx = ctx
		go c.stateSyncWatcher()
		go c.bootstrapAndReady()
		<-c.shutdownCh
	}()
}

func (c *Cluster) bootstrapAndReady() {
	ok := c.bootstrap()
	if !ok {
		logger.Error("Bootstrap unsuccessful")
		c.Shutdown()
		return
	}

	// We bootstrapped first because with dirty state consensus
	// may have a peerset and not find a leader so we cannot wait
	// for it.
	timer := time.NewTimer(30 * time.Second)
	select {
	case <-timer.C:
		logger.Error("consensus start timed out")
		c.Shutdown()
		return
	case <-c.consensus.Ready():
	case <-c.ctx.Done():
		return
	}

	// Cluster is ready.
	c.readyCh <- struct{}{}
	logger.Info("IPFS Cluster is ready")
	logger.Info("Cluster Peers (not including ourselves):")
	peers := c.peerManager.peersAddrs()
	if len(peers) == 0 {
		logger.Info("    - No other peers")
	}
	for _, a := range c.peerManager.peersAddrs() {
		logger.Infof("    - %s", a)
	}
}

func (c *Cluster) bootstrap() bool {
	// Cases in which we do not bootstrap
	if len(c.config.Bootstrap) == 0 || len(c.config.ClusterPeers) > 0 {
		return true
	}

	for _, b := range c.config.Bootstrap {
		logger.Infof("Bootstrapping to %s", b)
		err := c.Join(b)
		if err == nil {
			return true
		}
		logger.Error(err)
	}
	return false
}

// Ready returns a channel which signals when this peer is
// fully initialized (including consensus).
func (c *Cluster) Ready() <-chan struct{} {
	return c.readyCh
}

// Shutdown stops the IPFS cluster components
func (c *Cluster) Shutdown() error {
	c.shutdownLock.Lock()
	defer c.shutdownLock.Unlock()

	if c.shutdown {
		logger.Warning("Cluster is already shutdown")
		return nil
	}

	logger.Info("shutting down IPFS Cluster")

	if c.config.LeaveOnShutdown {
		// best effort
		logger.Warning("Attempting to leave Cluster. This may take some seconds")
		err := c.consensus.LogRmPeer(c.host.ID())
		if err != nil {
			logger.Error("leaving cluster: " + err.Error())
		} else {
			time.Sleep(2 * time.Second)
		}
		c.peerManager.resetPeers()
	}

	if con := c.consensus; con != nil {
		if err := con.Shutdown(); err != nil {
			logger.Errorf("error stopping consensus: %s", err)
			return err
		}
	}

	c.peerManager.savePeers()

	if err := c.api.Shutdown(); err != nil {
		logger.Errorf("error stopping API: %s", err)
		return err
	}
	if err := c.ipfs.Shutdown(); err != nil {
		logger.Errorf("error stopping IPFS Connector: %s", err)
		return err
	}

	if err := c.tracker.Shutdown(); err != nil {
		logger.Errorf("error stopping PinTracker: %s", err)
		return err
	}
	c.shutdownCh <- struct{}{}
	c.wg.Wait()
	c.host.Close() // Shutdown all network services
	c.shutdown = true
	close(c.doneCh)
	return nil
}

// Done provides a way to learn if the Peer has been shutdown
// (for example, because it has been removed from the Cluster)
func (c *Cluster) Done() <-chan struct{} {
	return c.doneCh
}

// ID returns information about the Cluster peer
func (c *Cluster) ID() ID {
	// ignore error since it is included in response object
	ipfsID, _ := c.ipfs.ID()
	var addrs []ma.Multiaddr
	for _, addr := range c.host.Addrs() {
		addrs = append(addrs, multiaddrJoin(addr, c.host.ID()))
	}

	return ID{
		ID:                 c.host.ID(),
		PublicKey:          c.host.Peerstore().PubKey(c.host.ID()),
		Addresses:          addrs,
		ClusterPeers:       c.peerManager.peersAddrs(),
		Version:            Version,
		Commit:             Commit,
		RPCProtocolVersion: RPCProtocol,
		IPFS:               ipfsID,
	}
}

// PeerAdd adds a new peer to this Cluster.
//
// The new peer must be reachable. It will be added to the
// consensus and will receive the shared state (including the
// list of peers). The new peer should be a single-peer cluster,
// preferable without any relevant state.
func (c *Cluster) PeerAdd(addr ma.Multiaddr) (ID, error) {
	// starting 10 nodes on the same box for testing
	// causes deadlock and a global lock here
	// seems to help.
	c.paMux.Lock()
	defer c.paMux.Unlock()
	logger.Debugf("peerAdd called with %s", addr)
	pid, decapAddr, err := multiaddrSplit(addr)
	if err != nil {
		id := ID{
			Error: err.Error(),
		}
		return id, err
	}

	// Figure out its real address if we have one
	remoteAddr := getRemoteMultiaddr(c.host, pid, decapAddr)

	err = c.peerManager.addPeer(remoteAddr)
	if err != nil {
		logger.Error(err)
		id := ID{ID: pid, Error: err.Error()}
		return id, err
	}

	// Figure out our address to that peer. This also
	// ensures that it is reachable
	var addrSerial MultiaddrSerial
	err = c.rpcClient.Call(pid, "Cluster",
		"RemoteMultiaddrForPeer", c.host.ID(), &addrSerial)
	if err != nil {
		logger.Error(err)
		id := ID{ID: pid, Error: err.Error()}
		c.peerManager.rmPeer(pid, false)
		return id, err
	}

	// Log the new peer in the log so everyone gets it.
	err = c.consensus.LogAddPeer(remoteAddr)
	if err != nil {
		logger.Error(err)
		id := ID{ID: pid, Error: err.Error()}
		c.peerManager.rmPeer(pid, false)
		return id, err
	}

	// Send cluster peers to the new peer.
	clusterPeers := append(c.peerManager.peersAddrs(),
		addrSerial.ToMultiaddr())
	err = c.rpcClient.Call(pid,
		"Cluster",
		"PeerManagerAddFromMultiaddrs",
		MultiaddrsToSerial(clusterPeers),
		&struct{}{})
	if err != nil {
		logger.Error(err)
	}

	id, err := c.getIDForPeer(pid)
	return id, nil
}

// PeerRemove removes a peer from this Cluster.
//
// The peer will be removed from the consensus peer set,
// it will be shut down after this happens.
func (c *Cluster) PeerRemove(pid peer.ID) error {
	if !c.peerManager.isPeer(pid) {
		return fmt.Errorf("%s is not a peer", pid.Pretty())
	}

	err := c.consensus.LogRmPeer(pid)
	if err != nil {
		logger.Error(err)
		return err
	}

	// This is a best effort. It may fail
	// if that peer is down
	err = c.rpcClient.Call(pid,
		"Cluster",
		"PeerManagerRmPeerShutdown",
		pid,
		&struct{}{})
	if err != nil {
		logger.Error(err)
	}

	return nil
}

// Join adds this peer to an existing cluster. The calling peer should
// be a single-peer cluster node. This is almost equivalent to calling
// PeerAdd on the destination cluster.
func (c *Cluster) Join(addr ma.Multiaddr) error {
	logger.Debugf("Join(%s)", addr)

	//if len(c.peerManager.peers()) > 1 {
	//	logger.Error(c.peerManager.peers())
	//	return errors.New("only single-node clusters can be joined")
	//}

	pid, _, err := multiaddrSplit(addr)
	if err != nil {
		logger.Error(err)
		return err
	}

	// Bootstrap to myself
	if pid == c.host.ID() {
		return nil
	}

	// Add peer to peerstore so we can talk to it
	c.peerManager.addPeer(addr)

	// Note that PeerAdd() on the remote peer will
	// figure out what our real address is (obviously not
	// ClusterAddr).
	var myID IDSerial
	err = c.rpcClient.Call(pid,
		"Cluster",
		"PeerAdd",
		MultiaddrToSerial(multiaddrJoin(c.config.ClusterAddr, c.host.ID())),
		&myID)
	if err != nil {
		logger.Error(err)
		return err
	}

	// wait for leader and for state to catch up
	// then sync
	err = c.consensus.WaitForSync()
	if err != nil {
		logger.Error(err)
		return err
	}
	c.StateSync()

	logger.Infof("joined %s's cluster", addr)
	return nil
}

// StateSync syncs the consensus state to the Pin Tracker, ensuring
// that every Cid that should be tracked is tracked. It returns
// PinInfo for Cids which were added or deleted.
func (c *Cluster) StateSync() ([]PinInfo, error) {
	cState, err := c.consensus.State()
	if err != nil {
		return nil, err
	}

	logger.Debug("syncing state to tracker")
	clusterPins := cState.ListPins()
	var changed []*cid.Cid

	// For the moment we run everything in parallel.
	// The PinTracker should probably decide if it can
	// pin in parallel or queues everything and does it
	// one by one

	// Track items which are not tracked
	for _, h := range clusterPins {
		if c.tracker.Status(h).Status == TrackerStatusUnpinned {
			changed = append(changed, h)
			go c.tracker.Track(h)
		}
	}

	// Untrack items which should not be tracked
	for _, p := range c.tracker.StatusAll() {
		h, _ := cid.Decode(p.CidStr)
		if !cState.HasPin(h) {
			changed = append(changed, h)
			go c.tracker.Untrack(h)
		}
	}

	var infos []PinInfo
	for _, h := range changed {
		infos = append(infos, c.tracker.Status(h))
	}
	return infos, nil
}

// StatusAll returns the GlobalPinInfo for all tracked Cids. If an error
// happens, the slice will contain as much information as could be fetched.
func (c *Cluster) StatusAll() ([]GlobalPinInfo, error) {
	return c.globalPinInfoSlice("TrackerStatusAll")
}

// Status returns the GlobalPinInfo for a given Cid. If an error happens,
// the GlobalPinInfo should contain as much information as could be fetched.
func (c *Cluster) Status(h *cid.Cid) (GlobalPinInfo, error) {
	return c.globalPinInfoCid("TrackerStatus", h)
}

// SyncAllLocal makes sure that the current state for all tracked items
// matches the state reported by the IPFS daemon.
//
// SyncAllLocal returns the list of PinInfo that where updated because of
// the operation, along with those in error states.
func (c *Cluster) SyncAllLocal() ([]PinInfo, error) {
	syncedItems, err := c.tracker.SyncAll()
	// Despite errors, tracker provides synced items that we can provide.
	// They encapsulate the error.
	if err != nil {
		logger.Error("tracker.Sync() returned with error: ", err)
		logger.Error("Is the ipfs daemon running?")
		logger.Error("LocalSync returning without attempting recovers")
	}
	return syncedItems, err
}

// SyncLocal performs a local sync operation for the given Cid. This will
// tell the tracker to verify the status of the Cid against the IPFS daemon.
// It returns the updated PinInfo for the Cid.
func (c *Cluster) SyncLocal(h *cid.Cid) (PinInfo, error) {
	var err error
	pInfo, err := c.tracker.Sync(h)
	// Despite errors, trackers provides an updated PinInfo so
	// we just log it.
	if err != nil {
		logger.Error("tracker.SyncCid() returned with error: ", err)
		logger.Error("Is the ipfs daemon running?")
	}
	return pInfo, err
}

// SyncAll triggers LocalSync() operations in all cluster peers.
func (c *Cluster) SyncAll() ([]GlobalPinInfo, error) {
	return c.globalPinInfoSlice("SyncAllLocal")
}

// Sync triggers a LocalSyncCid() operation for a given Cid
// in all cluster peers.
func (c *Cluster) Sync(h *cid.Cid) (GlobalPinInfo, error) {
	return c.globalPinInfoCid("SyncLocal", h)
}

// RecoverLocal triggers a recover operation for a given Cid
func (c *Cluster) RecoverLocal(h *cid.Cid) (PinInfo, error) {
	return c.tracker.Recover(h)
}

// Recover triggers a recover operation for a given Cid in all
// cluster peers.
func (c *Cluster) Recover(h *cid.Cid) (GlobalPinInfo, error) {
	return c.globalPinInfoCid("TrackerRecover", h)
}

// Pins returns the list of Cids managed by Cluster and which are part
// of the current global state. This is the source of truth as to which
// pins are managed, but does not indicate if the item is successfully pinned.
func (c *Cluster) Pins() []*cid.Cid {
	cState, err := c.consensus.State()
	if err != nil {
		logger.Error(err)
		return []*cid.Cid{}
	}
	return cState.ListPins()
}

// Pin makes the cluster Pin a Cid. This implies adding the Cid
// to the IPFS Cluster peers shared-state. Depending on the cluster
// pinning strategy, the PinTracker may then request the IPFS daemon
// to pin the Cid.
//
// Pin returns an error if the operation could not be persisted
// to the global state. Pin does not reflect the success or failure
// of underlying IPFS daemon pinning operations.
func (c *Cluster) Pin(h *cid.Cid) error {
	logger.Info("pinning:", h)
	err := c.consensus.LogPin(h)
	if err != nil {
		return err
	}
	return nil
}

// Unpin makes the cluster Unpin a Cid. This implies adding the Cid
// to the IPFS Cluster peers shared-state.
//
// Unpin returns an error if the operation could not be persisted
// to the global state. Unpin does not reflect the success or failure
// of underlying IPFS daemon unpinning operations.
func (c *Cluster) Unpin(h *cid.Cid) error {
	logger.Info("unpinning:", h)
	err := c.consensus.LogUnpin(h)
	if err != nil {
		return err
	}
	return nil
}

// Version returns the current IPFS Cluster version
func (c *Cluster) Version() string {
	return Version
}

// Peers returns the IDs of the members of this Cluster
func (c *Cluster) Peers() []ID {
	members := c.peerManager.peers()
	peersSerial := make([]IDSerial, len(members), len(members))
	peers := make([]ID, len(members), len(members))

	errs := c.multiRPC(members, "Cluster", "ID", struct{}{},
		copyIDSerialsToIfaces(peersSerial))

	for i, err := range errs {
		if err != nil {
			peersSerial[i].ID = peer.IDB58Encode(members[i])
			peersSerial[i].Error = err.Error()
		}
	}

	for i, ps := range peersSerial {
		peers[i] = ps.ToID()
	}
	return peers
}

// makeHost makes a libp2p-host
func makeHost(ctx context.Context, cfg *Config) (host.Host, error) {
	ps := peerstore.NewPeerstore()
	privateKey := cfg.PrivateKey
	publicKey := privateKey.GetPublic()

	if err := ps.AddPubKey(cfg.ID, publicKey); err != nil {
		return nil, err
	}

	if err := ps.AddPrivKey(cfg.ID, privateKey); err != nil {
		return nil, err
	}

	network, err := swarm.NewNetwork(
		ctx,
		[]ma.Multiaddr{cfg.ClusterAddr},
		cfg.ID,
		ps,
		nil,
	)

	if err != nil {
		return nil, err
	}

	bhost := basichost.New(network)
	return bhost, nil
}

// Perform an RPC request to multiple destinations
func (c *Cluster) multiRPC(dests []peer.ID, svcName, svcMethod string, args interface{}, reply []interface{}) []error {
	if len(dests) != len(reply) {
		panic("must have matching dests and replies")
	}
	var wg sync.WaitGroup
	errs := make([]error, len(dests), len(dests))

	for i := range dests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := c.rpcClient.Call(
				dests[i],
				svcName,
				svcMethod,
				args,
				reply[i])
			errs[i] = err
		}(i)
	}
	wg.Wait()
	return errs

}

func (c *Cluster) globalPinInfoCid(method string, h *cid.Cid) (GlobalPinInfo, error) {
	pin := GlobalPinInfo{
		Cid:     h,
		PeerMap: make(map[peer.ID]PinInfo),
	}

	members := c.peerManager.peers()
	replies := make([]PinInfo, len(members), len(members))
	args := NewCidArg(h)
	errs := c.multiRPC(members, "Cluster", method, args, copyPinInfoToIfaces(replies))

	for i, r := range replies {
		if e := errs[i]; e != nil { // This error must come from not being able to contact that cluster member
			logger.Errorf("%s: error in broadcast response from %s: %s ", c.host.ID(), members[i], e)
			if r.Status == TrackerStatusBug {
				r = PinInfo{
					CidStr: h.String(),
					Peer:   members[i],
					Status: TrackerStatusClusterError,
					TS:     time.Now(),
					Error:  e.Error(),
				}
			} else {
				r.Error = e.Error()
			}
		}
		pin.PeerMap[members[i]] = r
	}

	return pin, nil
}

func (c *Cluster) globalPinInfoSlice(method string) ([]GlobalPinInfo, error) {
	var infos []GlobalPinInfo
	fullMap := make(map[string]GlobalPinInfo)

	members := c.peerManager.peers()
	replies := make([][]PinInfo, len(members), len(members))
	errs := c.multiRPC(members, "Cluster", method, struct{}{}, copyPinInfoSliceToIfaces(replies))

	mergePins := func(pins []PinInfo) {
		for _, p := range pins {
			item, ok := fullMap[p.CidStr]
			c, _ := cid.Decode(p.CidStr)
			if !ok {
				fullMap[p.CidStr] = GlobalPinInfo{
					Cid: c,
					PeerMap: map[peer.ID]PinInfo{
						p.Peer: p,
					},
				}
			} else {
				item.PeerMap[p.Peer] = p
			}
		}
	}

	erroredPeers := make(map[peer.ID]string)
	for i, r := range replies {
		if e := errs[i]; e != nil { // This error must come from not being able to contact that cluster member
			logger.Errorf("%s: error in broadcast response from %s: %s ", c.host.ID(), members[i], e)
			erroredPeers[members[i]] = e.Error()
		} else {
			mergePins(r)
		}
	}

	// Merge any errors
	for p, msg := range erroredPeers {
		for c := range fullMap {
			fullMap[c].PeerMap[p] = PinInfo{
				CidStr: c,
				Peer:   p,
				Status: TrackerStatusClusterError,
				TS:     time.Now(),
				Error:  msg,
			}
		}
	}

	for _, v := range fullMap {
		infos = append(infos, v)
	}

	return infos, nil
}

func (c *Cluster) getIDForPeer(pid peer.ID) (ID, error) {
	idSerial := ID{ID: pid}.ToSerial()
	err := c.rpcClient.Call(
		pid, "Cluster", "ID", struct{}{}, &idSerial)
	id := idSerial.ToID()
	if err != nil {
		logger.Error(err)
		id.Error = err.Error()
	}
	return id, err
}
