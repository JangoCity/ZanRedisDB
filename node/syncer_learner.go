package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/absolute8511/ZanRedisDB/common"
	"github.com/absolute8511/ZanRedisDB/raft/raftpb"
)

var enableTest = false

func EnableForTest() {
	enableTest = true
}

const (
	logSendBufferLen = 100
)

type logSyncerSM struct {
	clusterInfo    common.IClusterInfo
	fullNS         string
	machineConfig  MachineConfig
	ID             uint64
	syncedCnt      int64
	syncedIndex    uint64
	syncedTerm     uint64
	lgSender       *RemoteLogSender
	stopping       int32
	sendCh         chan *BatchInternalRaftRequest
	sendStop       chan struct{}
	wg             sync.WaitGroup
	waitSendLogChs chan chan struct{}
}

func NewLogSyncerSM(opts *KVOptions, machineConfig MachineConfig, localID uint64, fullNS string,
	clusterInfo common.IClusterInfo) (StateMachine, error) {

	lg := &logSyncerSM{
		fullNS:         fullNS,
		machineConfig:  machineConfig,
		ID:             localID,
		clusterInfo:    clusterInfo,
		sendCh:         make(chan *BatchInternalRaftRequest, logSendBufferLen),
		sendStop:       make(chan struct{}),
		waitSendLogChs: make(chan chan struct{}, 1),
		//dataDir:       path.Join(opts.DataDir, "logsyncer"),
	}

	var localCluster string
	if clusterInfo != nil {
		localCluster = clusterInfo.GetClusterName()
	}
	lgSender, err := NewRemoteLogSender(localCluster, lg.fullNS, lg.machineConfig.RemoteSyncCluster)
	if err != nil {
		return nil, err
	}
	lg.lgSender = lgSender
	return lg, nil
}

func (sm *logSyncerSM) Debugf(f string, args ...interface{}) {
	msg := fmt.Sprintf(f, args...)
	nodeLog.DebugDepth(1, fmt.Sprintf("%v: %s", sm.fullNS, msg))
}

func (sm *logSyncerSM) Infof(f string, args ...interface{}) {
	msg := fmt.Sprintf(f, args...)
	nodeLog.InfoDepth(1, fmt.Sprintf("%v: %s", sm.fullNS, msg))
}

func (sm *logSyncerSM) Errorf(f string, args ...interface{}) {
	msg := fmt.Sprintf(f, args...)
	nodeLog.ErrorDepth(1, fmt.Sprintf("%v: %s", sm.fullNS, msg))
}

func (sm *logSyncerSM) Optimize() {
}

func (sm *logSyncerSM) GetDBInternalStats() string {
	return ""
}

func (sm *logSyncerSM) GetStats() common.NamespaceStats {
	var ns common.NamespaceStats
	stat := make(map[string]interface{})
	stat["role"] = common.LearnerRoleLogSyncer
	stat["synced"] = atomic.LoadInt64(&sm.syncedCnt)
	stat["synced_index"] = atomic.LoadUint64(&sm.syncedIndex)
	stat["synced_term"] = atomic.LoadUint64(&sm.syncedTerm)
	ns.InternalStats = stat
	return ns
}

func (sm *logSyncerSM) CleanData() error {
	return nil
}

func (sm *logSyncerSM) Destroy() {
}

func (sm *logSyncerSM) CheckExpiredData(buffer common.ExpiredDataBuffer, stop chan struct{}) error {
	return nil
}

func (sm *logSyncerSM) Start() error {
	sm.wg.Add(1)
	go func() {
		defer sm.wg.Done()
		sm.handlerRaftLogs()
	}()
	return nil
}

// the raft node will make sure the raft apply is stopped first
func (sm *logSyncerSM) Close() {
	if !atomic.CompareAndSwapInt32(&sm.stopping, 0, 1) {
		return
	}
	close(sm.sendStop)
	sm.wg.Wait()
}

func (sm *logSyncerSM) handlerRaftLogs() {
	defer func() {
		sm.lgSender.Stop()
		sm.Infof("raft log syncer send loop exit")
	}()
	raftLogs := make([]*BatchInternalRaftRequest, 0, logSendBufferLen)
	var last *BatchInternalRaftRequest
	state, err := sm.lgSender.getRemoteSyncedRaft(sm.sendStop)
	if err != nil {
		sm.Errorf("failed to get the synced state from remote: %v", err)
	}
	for {
		handled := false
		var err error
		select {
		case req := <-sm.sendCh:
			last = req
			raftLogs = append(raftLogs, req)
			if nodeLog.Level() > common.LOG_DETAIL {
				sm.Debugf("batching raft log: %v in batch: %v", req.String(), len(raftLogs))
			}
		default:
			if len(raftLogs) == 0 {
				select {
				case <-sm.sendStop:
					return
				case req := <-sm.sendCh:
					last = req
					raftLogs = append(raftLogs, req)
					if nodeLog.Level() >= common.LOG_DETAIL {
						sm.Debugf("batching raft log: %v in batch: %v", req.String(), len(raftLogs))
					}
				case waitCh := <-sm.waitSendLogChs:
					select {
					case req := <-sm.sendCh:
						last = req
						raftLogs = append(raftLogs, req)
						go func() {
							select {
							// put back to wait next again
							case sm.waitSendLogChs <- waitCh:
							case <-sm.sendStop:
							}
						}()
					default:
						sm.Infof("wake up waiting buffered send logs since no more logs")
						close(waitCh)
					}
				}
				continue
			}
			handled = true
			if state.SyncedTerm >= last.OrigTerm && state.SyncedIndex >= last.OrigIndex {
				// remote is already replayed this raft log
			} else {
				err = sm.lgSender.sendRaftLog(raftLogs, sm.sendStop)
			}
		}
		if err != nil {
			select {
			case <-sm.sendStop:
				return
			default:
				sm.Errorf("failed to send raft log to remote: %v, %v", err, raftLogs)
			}
			continue
		}
		if handled {
			atomic.AddInt64(&sm.syncedCnt, int64(len(raftLogs)))
			atomic.StoreUint64(&sm.syncedIndex, last.OrigIndex)
			atomic.StoreUint64(&sm.syncedTerm, last.OrigTerm)
			raftLogs = raftLogs[:0]
		}
	}
}

func (sm *logSyncerSM) waitBufferedLogs(timeout time.Duration) error {
	waitCh := make(chan struct{})
	sm.Infof("wait buffered send logs")
	var waitT <-chan time.Time
	if timeout > 0 {
		tm := time.NewTimer(timeout)
		defer tm.Stop()
		waitT = tm.C
	}
	select {
	case sm.waitSendLogChs <- waitCh:
	case <-waitT:
		return errors.New("wait log commit timeout")
	case <-sm.sendStop:
		return common.ErrStopped
	}
	select {
	case <-waitCh:
	case <-waitT:
		return errors.New("wait log commit timeout")
	case <-sm.sendStop:
		return common.ErrStopped
	}
	return nil
}

// snapshot should wait all buffered commit logs
func (sm *logSyncerSM) GetSnapshot(term uint64, index uint64) (*KVSnapInfo, error) {
	var si KVSnapInfo
	err := sm.waitBufferedLogs(time.Second * 10)
	return &si, err
}

func (sm *logSyncerSM) RestoreFromSnapshot(startup bool, raftSnapshot raftpb.Snapshot, stop chan struct{}) error {
	// get (term-index) from the remote cluster, if the remote cluster has
	// greater (term-index) than snapshot, we can just ignore the snapshot restore
	// since we already synced the data in snapshot.
	sm.Infof("restore snapshot : %v", raftSnapshot.Metadata.String())
	state, err := sm.lgSender.getRemoteSyncedRaft(stop)
	if err != nil {
		return err
	}
	if state.SyncedTerm > raftSnapshot.Metadata.Term && state.SyncedIndex > raftSnapshot.Metadata.Index {
		sm.Infof("ignored restore snapshot since remote has newer raft: %v than %v", state, raftSnapshot.Metadata.String())
		return nil
	}

	// TODO: should wait all batched sync raft logs success
	if sm.clusterInfo == nil {
		// in test, the cluster coordinator is not enabled, we can just ignore restore.
		sm.Infof("nil cluster info, only for test: %v", raftSnapshot.Metadata.String())
		return nil
	}

	sm.Infof("wait buffered send logs while restore from snapshot")
	err = sm.waitBufferedLogs(0)
	if err != nil {
		return err
	}

	// while startup we can use the local snapshot to restart,
	// but while running, we should install the leader's snapshot,
	// so we need remove local and sync from leader
	retry := 0
	for retry < 3 {
		forceRemote := true
		if enableTest {
			// for test we use local
			forceRemote = false
		}
		syncAddr, syncDir := GetValidBackupInfo(sm.machineConfig, sm.clusterInfo, sm.fullNS, sm.ID, stop, raftSnapshot, retry, forceRemote)
		// note the local sync path not supported, so we need try another replica if syncAddr is empty
		if syncAddr == "" && syncDir == "" {
			err = errors.New("no backup available from others")
		} else {
			err = sm.lgSender.notifyTransferSnap(raftSnapshot, syncAddr, syncDir)
			if err != nil {
				sm.Infof("notify apply snap %v,%v,%v failed: %v", raftSnapshot.Metadata, syncAddr, syncDir, err)
			} else {
				err := sm.lgSender.waitApplySnapStatus(raftSnapshot, stop)
				if err != nil {
					sm.Infof("wait apply snap %v,%v,%v failed: %v", raftSnapshot.Metadata, syncAddr, syncDir, err)
				} else {
					break
				}
			}
		}
		retry++
		select {
		case <-stop:
			return err
		case <-time.After(time.Second):
		}
	}
	if err != nil {
		return err
	}

	sm.Infof("apply snap done %v", raftSnapshot.Metadata)
	atomic.StoreUint64(&sm.syncedIndex, raftSnapshot.Metadata.Index)
	atomic.StoreUint64(&sm.syncedTerm, raftSnapshot.Metadata.Term)
	return nil
}

func (sm *logSyncerSM) ApplyRaftRequest(reqList BatchInternalRaftRequest, term uint64, index uint64, stop chan struct{}) (bool, error) {
	if nodeLog.Level() >= common.LOG_DETAIL {
		sm.Debugf("applying in log syncer: %v at (%v, %v)", reqList.String(), term, index)
	}
	forceBackup := false
	reqList.OrigTerm = term
	reqList.OrigIndex = index
	reqList.Type = FromClusterSyncer
	if sm.clusterInfo != nil {
		reqList.OrigCluster = sm.clusterInfo.GetClusterName()
	}
	if reqList.ReqId == 0 {
		for _, e := range reqList.Reqs {
			reqList.ReqId = e.Header.ID
			break
		}
	}
	if reqList.Timestamp == 0 {
		sm.Errorf("miss timestamp in raft request: %v", reqList)
	}
	for _, req := range reqList.Reqs {
		if req.Header.DataType == int32(CustomReq) {
			var p customProposeData
			err := json.Unmarshal(req.Data, &p)
			if err != nil {
				sm.Infof("failed to unmarshal http propose: %v", req.String())
			}
			if p.ProposeOp == ProposeOp_Backup {
				sm.Infof("got force backup request")
				forceBackup = true
				break
			}
		}
	}
	select {
	case sm.sendCh <- &reqList:
	case <-stop:
		return false, nil
	case <-sm.sendStop:
		return false, nil
	}

	return forceBackup, nil
}
