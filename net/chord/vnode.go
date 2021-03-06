package chord

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"time"

	"github.com/nknorg/nkn/util/config"
	nlog "github.com/nknorg/nkn/util/log"
)

// Converts the ID to Hex string
func (vn *Vnode) String() string {
	return fmt.Sprintf("%x", vn.Id)
}

func (vn *Vnode) ToData() *VnodeData {
	if vn == nil {
		return nil
	}
	return &VnodeData{vn.String(), vn.Host, vn.NodePort, vn.HttpWsPort}
}

func (vn *Vnode) NodeAddr() (string, error) {
	host, _, err := net.SplitHostPort(vn.Host)
	if err != nil {
		nlog.Error(err)
		return "", err
	}

	return net.JoinHostPort(host, strconv.Itoa(int(vn.NodePort))), nil
}

func (vn *Vnode) HttpWsAddr() (string, error) {
	host, _, err := net.SplitHostPort(vn.Host)
	if err != nil {
		nlog.Error(err)
		return "", err
	}

	return net.JoinHostPort(host, strconv.Itoa(int(vn.HttpWsPort))), nil
}

// Initializes a local vnode
func (vn *localVnode) init(idx int) {
	ringCfg := vn.ring.config

	// Generate an ID
	vn.genId(ringCfg.Hostname, ringCfg.JoinBlkHeight+uint32(idx))

	// Set our host
	vn.Host = ringCfg.Hostname

	// Set node ports
	vn.NodePort = config.Parameters.NodePort
	vn.HttpWsPort = config.Parameters.HttpWsPort

	// Initialize all state
	vn.successors = make([]*Vnode, ringCfg.NumSuccessors)
	vn.finger = make([]*Vnode, ringCfg.hashBits)

	// Register with the RPC mechanism
	vn.ring.transport.Register(&vn.Vnode, vn)

	if idx == 0 {
		go vn.keepalive()
	}
}

// Schedules the Vnode to do regular maintenence
func (vn *localVnode) schedule() {
	// Setup our stabilize timer
	vn.timer = time.AfterFunc(randStabilize(vn.ring.config), vn.stabilize)
}

// Generates an ID for the node
func (vn *localVnode) genId(host string, height uint32) {
	// Use the hash funciton
	conf := vn.ring.config
	hash := conf.HashFunc()
	hash.Write([]byte(host))
	binary.Write(hash, binary.BigEndian, height)

	// Use the hash as the ID
	vn.Id = hash.Sum(nil)
	log.Printf("genId(%s) = %s", (host + "@" + strconv.FormatUint(uint64(height), 10)), hex.EncodeToString(vn.Id))
}

// Called to periodically stabilize the vnode
func (vn *localVnode) stabilize() {
	// Clear the timer
	vn.timer = nil

	// Check for shutdown
	if vn.ring.shutdown != nil {
		vn.ring.shutdown <- true
		return
	}

	// Setup the next stabilize timer
	defer vn.schedule()

	// Check for new successor
	if err := vn.checkNewSuccessor(); err != nil {
		log.Printf("[ERR] Error checking for new successor: %s", err)
	}

	// Notify the successor
	if err := vn.notifySuccessor(); err != nil {
		log.Printf("[ERR] Error notifying successor: %s", err)
	}

	// Finger table fix up
	if err := vn.fixFingerTable(); err != nil {
		log.Printf("[ERR] Error fixing finger table: %s", err)
	}

	// Check the predecessor
	if err := vn.checkPredecessor(); err != nil {
		log.Printf("[ERR] Error checking predecessor: %s", err)
	}

	// Set the last stabilized time
	vn.stabilized = time.Now()
}

func (vn *localVnode) keepalive() {
	for {
		for _, n := range vn.successors {
			if n != nil && n.Host != vn.Host {
				vn.ring.transport.Ping(n)
			}
		}

		if vn.predecessor != nil && vn.predecessor.Host != vn.Host {
			vn.ring.transport.Ping(vn.predecessor)
		}

		for i, n := range vn.finger {
			if i > 0 && vn.finger[i-1] == n {
				continue
			}
			if n != nil && n.Host != vn.Host {
				vn.ring.transport.Ping(n)
			}
		}

		time.Sleep(10 * time.Second)
	}
}

// Checks for a new successor
func (vn *localVnode) checkNewSuccessor() error {
	// Ask our successor for it's predecessor
	trans := vn.ring.transport

	succ := vn.successors[0]
	if succ == nil {
		return errors.New("Node has no successor!")
	}
	maybe_suc, err := trans.GetPredecessor(succ)
	known := vn.knownSuccessors()

	for i := 0; i < known; i++ {
		if err == nil {
			break
		}
		nlog.Warn("Get predecessor error:", err)
		// alive, err := trans.Ping(succ)
		// if err == nil && alive {
		// return errors.New("Successor alive but cannot return its predecessor")
		// }
		if i == known-1 {
			nlog.Error("All known successors dead, trying finger tables")
			for i, n := range vn.finger {
				if n == nil || n.String() == vn.String() {
					continue
				}
				if i == 0 || n != vn.finger[i-1] {
					if !betweenRightIncl(vn.Id, vn.successors[0].Id, n.Id) {
						vn.successors[1] = n
						known++
						break
					}
				}
			}
			if i == known-1 {
				panic("All known successors dead!")
			}
		}
		// TODO: add retry before removing successor from list
		copy(vn.successors[0:], vn.successors[1:])
		vn.successors[known-1-i] = nil
		succ = vn.successors[0]
		maybe_suc, err = trans.GetPredecessor(succ)
	}

	// Check if we should replace our successor
	if maybe_suc != nil && between(vn.Id, succ.Id, maybe_suc.Id) {
		// Check if new successor is alive before switching
		alive, err := trans.Ping(maybe_suc)
		if err != nil || !alive {
			nlog.Warnf("Failed to contact potential new successor %s at %s", maybe_suc.String(), maybe_suc.Host)
			vn.ring.transport.Notify(succ, &vn.Vnode)
			return err
		}
		copy(vn.successors[1:], vn.successors[0:len(vn.successors)-1])
		vn.successors[0] = maybe_suc
		_, err = vn.fixFingerTableAtIndex(0)
		if err != nil {
			return err
		}
		if vn.OnNewSuccessor != nil {
			vn.OnNewSuccessor()
		}
	}
	return nil
}

// RPC: Invoked to return out predecessor
func (vn *localVnode) GetPredecessor() (*Vnode, error) {
	return vn.predecessor, nil
}

// Notifies our successor of us, updates successor list
func (vn *localVnode) notifySuccessor() error {
	// Notify successor
	succ := vn.successors[0]
	succ_list, err := vn.ring.transport.Notify(succ, &vn.Vnode)
	if err != nil {
		return err
	}

	// Trim the successors list if too long
	max_succ := vn.ring.config.NumSuccessors
	if len(succ_list) > max_succ-1 {
		succ_list = succ_list[:max_succ-1]
	}

	// Update local successors list
	for idx, s := range succ_list {
		// Ensure we don't set ourselves as a successor!
		if s == nil || s.String() == vn.String() {
			break
		}
		vn.successors[idx+1] = s
	}
	return nil
}

// RPC: Notify is invoked when a Vnode gets notified
func (vn *localVnode) Notify(maybe_pred *Vnode) ([]*Vnode, error) {
	shouldUpdate := false
	// Check if we should update our predecessor
	if vn.predecessor == nil || between(vn.predecessor.Id, vn.Id, maybe_pred.Id) {
		shouldUpdate = true
	} else if CompareId(vn.predecessor.Id, maybe_pred.Id) != 0 {
		alive, err := vn.ring.transport.Ping(vn.predecessor)
		if err != nil || !alive {
			shouldUpdate = true
		}
	}

	if shouldUpdate {
		// Inform the delegate
		conf := vn.ring.config
		old := vn.predecessor
		vn.ring.invokeDelegate(func() {
			conf.Delegate.NewPredecessor(&vn.Vnode, maybe_pred, old)
		})

		vn.predecessor = maybe_pred
	}

	// Return our successors list
	return vn.successors, nil
}

func (vn *localVnode) fixFingerTableAtIndex(idx int) (int, error) {
	// Determine the offset
	hb := vn.ring.config.hashBits
	offset := powerOffset(vn.Id, idx, hb)

	// Find the successor
	nodes, err := vn.FindSuccessors(1, offset)
	if nodes == nil || len(nodes) == 0 || err != nil {
		return idx, err
	}
	node := nodes[0]

	if vn.finger[idx] != nil && vn.finger[idx].String() != node.String() {
		alive, err := vn.ring.transport.Ping(node)
		if err != nil || !alive {
			node = vn.finger[idx]
		}
	}

	// Update the finger table
	vn.finger[idx] = node

	// Try to skip as many finger entries as possible
	for {
		next := idx + 1
		if next >= hb {
			break
		}
		offset := powerOffset(vn.Id, next, hb)

		// While the node is the successor, update the finger entries
		if betweenRightIncl(vn.Id, node.Id, offset) {
			vn.finger[next] = node
			idx = next
		} else {
			break
		}
	}

	var nextIdx int
	if idx+1 == hb {
		nextIdx = 0
	} else {
		nextIdx = idx + 1
	}

	return nextIdx, nil
}

// Fixes up the finger table
func (vn *localVnode) fixFingerTable() error {
	nextIdx, err := vn.fixFingerTableAtIndex(vn.last_finger)
	if err != nil {
		return err
	}

	vn.last_finger = nextIdx

	return nil
}

// Checks the health of our predecessor
func (vn *localVnode) checkPredecessor() error {
	// Check predecessor
	if vn.predecessor != nil {
		alive, err := vn.ring.transport.Ping(vn.predecessor)
		if err != nil || !alive {
			vn.predecessor = nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Finds next N successors. N must be <= NumSuccessors
func (vn *localVnode) FindSuccessors(n int, key []byte) ([]*Vnode, error) {
	if vn.successors == nil || len(vn.successors) == 0 {
		return nil, errors.New("Successor list not initialized")
	}

	// Check if we are the immediate predecessor
	if vn.successors[0] != nil && betweenRightIncl(vn.Id, vn.successors[0].Id, key) {
		if n <= len(vn.successors) {
			return vn.successors[:n], nil
		}
		return vn.successors, nil
	}

	// Try the closest preceeding nodes
	cp := closestPreceedingVnodeIterator{}
	cp.init(vn, key, false, false)
	for {
		// Get the next closest node
		closest := cp.Next()
		if closest == nil {
			break
		}

		// Try that node, break on success
		res, err := vn.ring.transport.FindSuccessors(closest, n, key)
		if err == nil {
			return res, nil
		} else {
			nlog.Warnf("Failed to contact %s. Got %s", closest.String(), err)
		}
	}

	// Determine how many successors we know of
	successors := vn.knownSuccessors()

	// Check if the ID is between us and any non-immediate successors
	for i := 1; i <= successors-n; i++ {
		if vn.successors[i] != nil && betweenRightIncl(vn.Id, vn.successors[i].Id, key) {
			remain := vn.successors[i:]
			if len(remain) > n {
				remain = remain[:n]
			}
			return remain, nil
		}
	}

	// Checked all closer nodes and our successors!
	return nil, fmt.Errorf("Exhausted all preceeding nodes")
}

func (vn *localVnode) FindPredecessor(key []byte) (*Vnode, error) {
	vnodes, err := vn.FindSuccessors(1, key)
	if err != nil {
		return nil, err
	}
	if len(vnodes) == 0 {
		return nil, errors.New("Cannot get successors for key " + hex.EncodeToString(key))
	}

	trans := vn.ring.transport

	pred, err := trans.GetPredecessor(vnodes[0])
	if err != nil {
		return nil, err
	}
	if pred == nil {
		return nil, errors.New("Cannot get predecessor for key " + hex.EncodeToString(key))
	}

	return pred, nil
}

// Instructs the vnode to leave
func (vn *localVnode) leave() error {
	// Inform the delegate we are leaving
	conf := vn.ring.config
	pred := vn.predecessor
	succ := vn.successors[0]
	vn.ring.invokeDelegate(func() {
		conf.Delegate.Leaving(&vn.Vnode, pred, succ)
	})

	// Notify predecessor to advance to their next successor
	var err error
	trans := vn.ring.transport
	if vn.predecessor != nil {
		err = trans.SkipSuccessor(vn.predecessor, &vn.Vnode)
	}

	// Notify successor to clear old predecessor
	err = mergeErrors(err, trans.ClearPredecessor(vn.successors[0], &vn.Vnode))
	return err
}

// Used to clear our predecessor when a node is leaving
func (vn *localVnode) ClearPredecessor(p *Vnode) error {
	if vn.predecessor != nil && vn.predecessor.String() == p.String() {
		// Inform the delegate
		conf := vn.ring.config
		old := vn.predecessor
		vn.ring.invokeDelegate(func() {
			conf.Delegate.PredecessorLeaving(&vn.Vnode, old)
		})
		vn.predecessor = nil
	}
	return nil
}

// Used to skip a successor when a node is leaving
func (vn *localVnode) SkipSuccessor(s *Vnode) error {
	// Skip if we have a match
	if vn.successors[0].String() == s.String() {
		// Inform the delegate
		conf := vn.ring.config
		old := vn.successors[0]
		vn.ring.invokeDelegate(func() {
			conf.Delegate.SuccessorLeaving(&vn.Vnode, old)
		})

		known := vn.knownSuccessors()
		copy(vn.successors[0:], vn.successors[1:])
		vn.successors[known-1] = nil
	}
	return nil
}

// Determine how many successors we know of
func (vn *localVnode) knownSuccessors() (successors int) {
	for i := 0; i < len(vn.successors); i++ {
		if vn.successors[i] != nil {
			successors = i + 1
		}
	}
	return
}

func (vn *localVnode) shouldConnectToHost(host string) bool {
	// successor list
	for _, n := range vn.successors {
		if n != nil && n.Host == host {
			return true
		}
	}

	// predecessor
	if vn.predecessor != nil && vn.predecessor.Host == host {
		return true
	}

	// finger table
	for _, n := range vn.finger {
		if n != nil && n.Host == host {
			return true
		}
	}

	return false
}

func (vn *localVnode) GetNeighbors() []*Vnode {
	seen := make(map[*Vnode]bool)
	neighbors := []*Vnode{}
	allNeighbors := append(vn.finger, vn.successors...)
	allNeighbors = append(allNeighbors, vn.predecessor)

	for _, n := range allNeighbors {
		if n == nil {
			continue
		}
		if n.Host == vn.Host {
			continue
		}
		if _, value := seen[n]; !value {
			seen[n] = true
			neighbors = append(neighbors, n)
		}
	}

	return neighbors
}

func (vn *localVnode) ShouldAddrInNeighbors(addr []byte) bool {
	allNeighbors := append(vn.finger, vn.successors...)
	allNeighbors = append(allNeighbors, vn.predecessor)

	for _, n := range allNeighbors {
		if n == nil {
			continue
		}
		if n.Host == vn.Host {
			continue
		}
		if CompareId(n.Id, addr) == 0 {
			return true
		}
	}

	// hb := vn.ring.config.hashBits
	// dist := distance(addr, vn.Id, hb)
	// offset := powerOffset(addr, dist.BitLen()-1, hb)
	// if vn.predecessor != nil && betweenRightIncl(vn.predecessor.Id, vn.Id, offset) {
	// 	return true
	// }

	return false
}

func (vn *localVnode) ClosestNeighborIterator(key []byte) (closestPreceedingVnodeIterator, error) {
	cp := closestPreceedingVnodeIterator{}
	cp.init(vn, key, false, true)
	return cp, nil
}

// Extract marshalable data from localVnode struct
func (vn *localVnode) toData() *localVnodeData {
	if vn == nil {
		return nil
	}

	succ := make([]*VnodeData, len(vn.successors))
	finger := make([]*VnodeData, len(vn.finger))

	for i, n := range vn.successors {
		succ[i] = n.ToData()
	}

	last := vn.finger[0]
	for i, n := range vn.finger {
		if n != last { // Skip same item, Only save the last until met new
			finger[i-1] = last.ToData() // n != last is impossible when i==0
		}
		last = n // Update last
	}
	finger[len(vn.finger)-1] = last.ToData() // save last to end of lst

	return &localVnodeData{
		*vn.Vnode.ToData(),
		succ,
		finger,
		vn.predecessor.ToData(),
		vn.last_finger,
	}
}
