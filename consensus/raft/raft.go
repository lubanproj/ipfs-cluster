package raft

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/lubanproj/ipfs-cluster/state"

	host "github.com/libp2p/go-libp2p-core/host"
	peer "github.com/libp2p/go-libp2p-core/peer"
	p2praft "github.com/libp2p/go-libp2p-raft"

	hraft "github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb"
	"go.opencensus.io/trace"
)

// ErrWaitingForSelf is returned when we are waiting for ourselves to depart
// the peer set, which won't happen
var errWaitingForSelf = errors.New("waiting for ourselves to depart")

// RaftMaxSnapshots indicates how many snapshots to keep in the consensus data
// folder.
// TODO: Maybe include this in Config. Not sure how useful it is to touch
// this anyways.
var RaftMaxSnapshots = 5

// RaftLogCacheSize is the maximum number of logs to cache in-memory.
// This is used to reduce disk I/O for the recently committed entries.
var RaftLogCacheSize = 512

// How long we wait for updates during shutdown before snapshotting
var waitForUpdatesShutdownTimeout = 5 * time.Second
var waitForUpdatesInterval = 400 * time.Millisecond

// How many times to retry snapshotting when shutting down
var maxShutdownSnapshotRetries = 5

// raftWrapper wraps the hraft.Raft object and related things like the
// different stores used or the hraft.Configuration.
// Its methods provide functionality for working with Raft.
type raftWrapper struct {
	ctx           context.Context
	cancel        context.CancelFunc
	raft          *hraft.Raft
	config        *Config
	host          host.Host
	serverConfig  hraft.Configuration
	transport     *hraft.NetworkTransport
	snapshotStore hraft.SnapshotStore
	logStore      hraft.LogStore
	stableStore   hraft.StableStore
	boltdb        *raftboltdb.BoltStore
	staging       bool
}

// newRaftWrapper creates a Raft instance and initializes
// everything leaving it ready to use. Note, that Bootstrap() should be called
// to make sure the raft instance is usable.
func newRaftWrapper(
	host host.Host,
	cfg *Config,
	fsm hraft.FSM,
	staging bool,
) (*raftWrapper, error) {

	raftW := &raftWrapper{}
	raftW.config = cfg
	raftW.host = host
	raftW.staging = staging
	// Set correct LocalID
	cfg.RaftConfig.LocalID = hraft.ServerID(peer.Encode(host.ID()))

	df := cfg.GetDataFolder()
	err := makeDataFolder(df)
	if err != nil {
		return nil, err
	}

	raftW.makeServerConfig()

	err = raftW.makeTransport()
	if err != nil {
		return nil, err
	}

	err = raftW.makeStores()
	if err != nil {
		return nil, err
	}

	logger.Debug("creating Raft")
	raftW.raft, err = hraft.NewRaft(
		cfg.RaftConfig,
		fsm,
		raftW.logStore,
		raftW.stableStore,
		raftW.snapshotStore,
		raftW.transport,
	)
	if err != nil {
		logger.Error("initializing raft: ", err)
		return nil, err
	}

	raftW.ctx, raftW.cancel = context.WithCancel(context.Background())
	go raftW.observePeers()

	return raftW, nil
}

// makeDataFolder creates the folder that is meant to store Raft data. Ensures
// we always set 0700 mode.
func makeDataFolder(folder string) error {
	return os.MkdirAll(folder, 0700)
}

func (rw *raftWrapper) makeTransport() (err error) {
	logger.Debug("creating libp2p Raft transport")
	rw.transport, err = p2praft.NewLibp2pTransport(
		rw.host,
		rw.config.NetworkTimeout,
	)
	return err
}

func (rw *raftWrapper) makeStores() error {
	logger.Debug("creating BoltDB store")
	df := rw.config.GetDataFolder()
	store, err := raftboltdb.NewBoltStore(filepath.Join(df, "raft.db"))
	if err != nil {
		return err
	}

	// wraps the store in a LogCache to improve performance.
	// See consul/agent/consul/server.go
	cacheStore, err := hraft.NewLogCache(RaftLogCacheSize, store)
	if err != nil {
		return err
	}

	logger.Debug("creating raft snapshot store")
	snapstore, err := hraft.NewFileSnapshotStoreWithLogger(
		df,
		RaftMaxSnapshots,
		raftStdLogger,
	)
	if err != nil {
		return err
	}

	rw.logStore = cacheStore
	rw.stableStore = store
	rw.snapshotStore = snapstore
	rw.boltdb = store
	return nil
}

// Bootstrap calls BootstrapCluster on the Raft instance with a valid
// Configuration (generated from InitPeerset) when Raft has no state
// and we are not setting up a staging peer. It returns if Raft
// was boostrapped (true) and an error.
func (rw *raftWrapper) Bootstrap() (bool, error) {
	logger.Debug("checking for existing raft states")
	hasState, err := hraft.HasExistingState(
		rw.logStore,
		rw.stableStore,
		rw.snapshotStore,
	)
	if err != nil {
		return false, err
	}

	if hasState {
		logger.Debug("raft cluster is already initialized")

		// Inform the user that we are working with a pre-existing peerset
		logger.Info("existing Raft state found! raft.InitPeerset will be ignored")
		cf := rw.raft.GetConfiguration()
		if err := cf.Error(); err != nil {
			logger.Debug(err)
			return false, err
		}
		currentCfg := cf.Configuration()
		srvs := ""
		for _, s := range currentCfg.Servers {
			srvs += fmt.Sprintf("        %s\n", s.ID)
		}

		logger.Debugf("Current Raft Peerset:\n%s\n", srvs)
		return false, nil
	}

	if rw.staging {
		logger.Debug("staging servers do not need initialization")
		logger.Info("peer is ready to join a cluster")
		return false, nil
	}

	voters := ""
	for _, s := range rw.serverConfig.Servers {
		voters += fmt.Sprintf("        %s\n", s.ID)
	}

	logger.Infof("initializing raft cluster with the following voters:\n%s\n", voters)

	future := rw.raft.BootstrapCluster(rw.serverConfig)
	if err := future.Error(); err != nil {
		logger.Error("bootstrapping cluster: ", err)
		return true, err
	}
	return true, nil
}

// create Raft servers configuration. The result is used
// by Bootstrap() when it proceeds to Bootstrap.
func (rw *raftWrapper) makeServerConfig() {
	rw.serverConfig = makeServerConf(append(rw.config.InitPeerset, rw.host.ID()))
}

// creates a server configuration with all peers as Voters.
func makeServerConf(peers []peer.ID) hraft.Configuration {
	sm := make(map[string]struct{})

	servers := make([]hraft.Server, 0)

	// Servers are peers + self. We avoid duplicate entries below
	for _, pid := range peers {
		p := peer.Encode(pid)
		_, ok := sm[p]
		if !ok { // avoid dups
			sm[p] = struct{}{}
			servers = append(servers, hraft.Server{
				Suffrage: hraft.Voter,
				ID:       hraft.ServerID(p),
				Address:  hraft.ServerAddress(p),
			})
		}
	}
	return hraft.Configuration{Servers: servers}
}

// WaitForLeader holds until Raft says we have a leader.
// Returns if ctx is canceled.
func (rw *raftWrapper) WaitForLeader(ctx context.Context) (string, error) {
	ctx, span := trace.StartSpan(ctx, "consensus/raft/WaitForLeader")
	defer span.End()

	ticker := time.NewTicker(time.Second / 2)
	for {
		select {
		case <-ticker.C:
			if l := rw.raft.Leader(); l != "" {
				logger.Debug("waitForleaderTimer")
				logger.Infof("Current Raft Leader: %s", l)
				ticker.Stop()
				return string(l), nil
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func (rw *raftWrapper) WaitForVoter(ctx context.Context) error {
	ctx, span := trace.StartSpan(ctx, "consensus/raft/WaitForVoter")
	defer span.End()

	logger.Debug("waiting until we are promoted to a voter")

	pid := hraft.ServerID(peer.Encode(rw.host.ID()))
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			logger.Debugf("%s: get configuration", pid)
			configFuture := rw.raft.GetConfiguration()
			if err := configFuture.Error(); err != nil {
				return err
			}

			if isVoter(pid, configFuture.Configuration()) {
				return nil
			}
			logger.Debugf("%s: not voter yet", pid)

			time.Sleep(waitForUpdatesInterval)
		}
	}
}

func isVoter(srvID hraft.ServerID, cfg hraft.Configuration) bool {
	for _, server := range cfg.Servers {
		if server.ID == srvID && server.Suffrage == hraft.Voter {
			return true
		}
	}
	return false
}

// WaitForUpdates holds until Raft has synced to the last index in the log
func (rw *raftWrapper) WaitForUpdates(ctx context.Context) error {
	ctx, span := trace.StartSpan(ctx, "consensus/raft/WaitForUpdates")
	defer span.End()

	logger.Debug("Raft state is catching up to the latest known version. Please wait...")
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			lai := rw.raft.AppliedIndex()
			li := rw.raft.LastIndex()
			logger.Debugf("current Raft index: %d/%d",
				lai, li)
			if lai == li {
				return nil
			}
			time.Sleep(waitForUpdatesInterval)
		}
	}
}

func (rw *raftWrapper) WaitForPeer(ctx context.Context, pid string, depart bool) error {
	ctx, span := trace.StartSpan(ctx, "consensus/raft/WaitForPeer")
	defer span.End()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			peers, err := rw.Peers(ctx)
			if err != nil {
				return err
			}

			if len(peers) == 1 && pid == peers[0] && depart {
				return errWaitingForSelf
			}

			found := find(peers, pid)

			// departing
			if depart && !found {
				return nil
			}

			// joining
			if !depart && found {
				return nil
			}

			time.Sleep(50 * time.Millisecond)
		}
	}
}

// Snapshot tells Raft to take a snapshot.
func (rw *raftWrapper) Snapshot() error {
	future := rw.raft.Snapshot()
	err := future.Error()
	if err != nil && err.Error() != hraft.ErrNothingNewToSnapshot.Error() {
		return err
	}
	return nil
}

// snapshotOnShutdown attempts to take a snapshot before a shutdown.
// Snapshotting might fail if the raft applied index is not the last index.
// This waits for the updates and tries to take a snapshot when the
// applied index is up to date.
// It will retry if the snapshot still fails, in case more updates have arrived.
// If waiting for updates times-out, it will not try anymore, since something
// is wrong. This is a best-effort solution as there is no way to tell Raft
// to stop processing entries because we want to take a snapshot before
// shutting down.
func (rw *raftWrapper) snapshotOnShutdown() error {
	var err error
	for i := 0; i < maxShutdownSnapshotRetries; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), waitForUpdatesShutdownTimeout)
		err = rw.WaitForUpdates(ctx)
		cancel()
		if err != nil {
			logger.Warn("timed out waiting for state updates before shutdown. Snapshotting may fail")
			return rw.Snapshot()
		}

		err = rw.Snapshot()
		if err == nil {
			return nil // things worked
		}

		// There was an error
		err = errors.New("could not snapshot raft: " + err.Error())
		logger.Warnf("retrying to snapshot (%d/%d)...", i+1, maxShutdownSnapshotRetries)
	}
	return err
}

// Shutdown shutdown Raft and closes the BoltDB.
func (rw *raftWrapper) Shutdown(ctx context.Context) error {
	_, span := trace.StartSpan(ctx, "consensus/raft/Shutdown")
	defer span.End()

	errMsgs := ""

	rw.cancel()

	err := rw.snapshotOnShutdown()
	if err != nil {
		errMsgs += err.Error() + ".\n"
	}

	future := rw.raft.Shutdown()
	err = future.Error()
	if err != nil {
		errMsgs += "could not shutdown raft: " + err.Error() + ".\n"
	}

	err = rw.boltdb.Close() // important!
	if err != nil {
		errMsgs += "could not close boltdb: " + err.Error()
	}

	if errMsgs != "" {
		return errors.New(errMsgs)
	}

	return nil
}

// AddPeer adds a peer to Raft
func (rw *raftWrapper) AddPeer(ctx context.Context, peer string) error {
	ctx, span := trace.StartSpan(ctx, "consensus/raft/AddPeer")
	defer span.End()

	// Check that we don't have it to not waste
	// log entries if so.
	peers, err := rw.Peers(ctx)
	if err != nil {
		return err
	}
	if find(peers, peer) {
		logger.Infof("%s is already a raft peer", peer)
		return nil
	}

	future := rw.raft.AddVoter(
		hraft.ServerID(peer),
		hraft.ServerAddress(peer),
		0,
		0,
	) // TODO: Extra cfg value?
	err = future.Error()
	if err != nil {
		logger.Error("raft cannot add peer: ", err)
	}
	return err
}

// RemovePeer removes a peer from Raft
func (rw *raftWrapper) RemovePeer(ctx context.Context, peer string) error {
	ctx, span := trace.StartSpan(ctx, "consensus/RemovePeer")
	defer span.End()

	// Check that we have it to not waste
	// log entries if we don't.
	peers, err := rw.Peers(ctx)
	if err != nil {
		return err
	}
	if !find(peers, peer) {
		logger.Infof("%s is not among raft peers", peer)
		return nil
	}

	if len(peers) == 1 && peers[0] == peer {
		return errors.New("cannot remove ourselves from a 1-peer cluster")
	}

	rmFuture := rw.raft.RemoveServer(
		hraft.ServerID(peer),
		0,
		0,
	) // TODO: Extra cfg value?
	err = rmFuture.Error()
	if err != nil {
		logger.Error("raft cannot remove peer: ", err)
		return err
	}

	return nil
}

// Leader returns Raft's leader. It may be an empty string if
// there is no leader or it is unknown.
func (rw *raftWrapper) Leader(ctx context.Context) string {
	_, span := trace.StartSpan(ctx, "consensus/raft/Leader")
	defer span.End()

	return string(rw.raft.Leader())
}

func (rw *raftWrapper) Peers(ctx context.Context) ([]string, error) {
	_, span := trace.StartSpan(ctx, "consensus/raft/Peers")
	defer span.End()

	ids := make([]string, 0)

	configFuture := rw.raft.GetConfiguration()
	if err := configFuture.Error(); err != nil {
		return nil, err
	}

	for _, server := range configFuture.Configuration().Servers {
		ids = append(ids, string(server.ID))
	}

	return ids, nil
}

// latestSnapshot looks for the most recent raft snapshot stored at the
// provided basedir.  It returns the snapshot's metadata, and a reader
// to the snapshot's bytes
func latestSnapshot(raftDataFolder string) (*hraft.SnapshotMeta, io.ReadCloser, error) {
	store, err := hraft.NewFileSnapshotStore(raftDataFolder, RaftMaxSnapshots, nil)
	if err != nil {
		return nil, nil, err
	}
	snapMetas, err := store.List()
	if err != nil {
		return nil, nil, err
	}
	if len(snapMetas) == 0 { // no error if snapshot isn't found
		return nil, nil, nil
	}
	meta, r, err := store.Open(snapMetas[0].ID)
	if err != nil {
		return nil, nil, err
	}
	return meta, r, nil
}

// LastStateRaw returns the bytes of the last snapshot stored, its metadata,
// and a flag indicating whether any snapshot was found.
func LastStateRaw(cfg *Config) (io.Reader, bool, error) {
	// Read most recent snapshot
	dataFolder := cfg.GetDataFolder()
	if _, err := os.Stat(dataFolder); os.IsNotExist(err) {
		// nothing to read
		return nil, false, nil
	}

	meta, r, err := latestSnapshot(dataFolder)
	if err != nil {
		return nil, false, err
	}
	if meta == nil { // no snapshots could be read
		return nil, false, nil
	}
	return r, true, nil
}

// SnapshotSave saves the provided state to a snapshot in the
// raft data path.  Old raft data is backed up and replaced
// by the new snapshot.  pids contains the config-specified
// peer ids to include in the snapshot metadata if no snapshot exists
// from which to copy the raft metadata
func SnapshotSave(cfg *Config, newState state.State, pids []peer.ID) error {
	dataFolder := cfg.GetDataFolder()
	err := makeDataFolder(dataFolder)
	if err != nil {
		return err
	}
	meta, _, err := latestSnapshot(dataFolder)
	if err != nil {
		return err
	}

	// make a new raft snapshot
	var raftSnapVersion hraft.SnapshotVersion = 1 // As of hraft v1.0.0 this is always 1
	configIndex := uint64(1)
	var raftIndex uint64
	var raftTerm uint64
	var srvCfg hraft.Configuration
	if meta != nil {
		raftIndex = meta.Index
		raftTerm = meta.Term
		srvCfg = meta.Configuration
		CleanupRaft(cfg)
	} else {
		// Begin the log after the index of a fresh start so that
		// the snapshot's state propagate's during bootstrap
		raftIndex = uint64(2)
		raftTerm = uint64(1)
		srvCfg = makeServerConf(pids)
	}

	snapshotStore, err := hraft.NewFileSnapshotStoreWithLogger(dataFolder, RaftMaxSnapshots, nil)
	if err != nil {
		return err
	}
	_, dummyTransport := hraft.NewInmemTransport("")

	sink, err := snapshotStore.Create(raftSnapVersion, raftIndex, raftTerm, srvCfg, configIndex, dummyTransport)
	if err != nil {
		return err
	}

	err = p2praft.EncodeSnapshot(newState, sink)
	if err != nil {
		sink.Cancel()
		return err
	}
	err = sink.Close()
	if err != nil {
		return err
	}
	return nil
}

// CleanupRaft moves the current data folder to a backup location
func CleanupRaft(cfg *Config) error {
	dataFolder := cfg.GetDataFolder()
	keep := cfg.BackupsRotate

	meta, _, err := latestSnapshot(dataFolder)
	if meta == nil && err == nil {
		// no snapshots at all. Avoid creating backups
		// from empty state folders.
		logger.Infof("cleaning empty Raft data folder (%s)", dataFolder)
		os.RemoveAll(dataFolder)
		return nil
	}

	logger.Infof("cleaning and backing up Raft data folder (%s)", dataFolder)
	dbh := newDataBackupHelper(dataFolder, keep)
	err = dbh.makeBackup()
	if err != nil {
		logger.Warn(err)
		logger.Warn("the state could not be cleaned properly")
		logger.Warn("manual intervention may be needed before starting cluster again")
	}
	return nil
}

// only call when Raft is shutdown
func (rw *raftWrapper) Clean() error {
	return CleanupRaft(rw.config)
}

func find(s []string, elem string) bool {
	for _, selem := range s {
		if selem == elem {
			return true
		}
	}
	return false
}

func (rw *raftWrapper) observePeers() {
	obsCh := make(chan hraft.Observation, 1)
	defer close(obsCh)

	observer := hraft.NewObserver(obsCh, true, func(o *hraft.Observation) bool {
		po, ok := o.Data.(hraft.PeerObservation)
		return ok && po.Removed
	})

	rw.raft.RegisterObserver(observer)
	defer rw.raft.DeregisterObserver(observer)

	for {
		select {
		case obs := <-obsCh:
			pObs := obs.Data.(hraft.PeerObservation)
			logger.Info("raft peer departed. Removing from peerstore: ", pObs.Peer.ID)
			pID, err := peer.Decode(string(pObs.Peer.ID))
			if err != nil {
				logger.Error(err)
				continue
			}
			rw.host.Peerstore().ClearAddrs(pID)
		case <-rw.ctx.Done():
			logger.Debug("stopped observing raft peers")
			return
		}
	}
}
