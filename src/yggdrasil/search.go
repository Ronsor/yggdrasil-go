package yggdrasil

// This thing manages search packets

// The basic idea is as follows:
//  We may know a NodeID (with a mask) and want to connect
//  We begin a search by initializing a list of all nodes in our DHT, sorted by closest to the destination
//  We then iteratively ping nodes from the search, marking each pinged node as visited
//  We add any unvisited nodes from ping responses to the search, truncating to some maximum search size
//  This stops when we either run out of nodes to ping (we hit a dead end where we can't make progress without going back), or we reach the destination
//  A new search packet is sent immediately after receiving a response
//  A new search packet is sent periodically, once per second, in case a packet was dropped (this slowly causes the search to become parallel if the search doesn't timeout but also doesn't finish within 1 second for whatever reason)

// TODO?
//  Some kind of max search steps, in case the node is offline, so we don't crawl through too much of the network looking for a destination that isn't there?

import (
	"errors"
	"sort"
	"time"

	"github.com/yggdrasil-network/yggdrasil-go/src/crypto"
)

// This defines the time after which we time out a search (so it can restart).
const search_RETRY_TIME = 3 * time.Second
const search_STEP_TIME = 100 * time.Millisecond

// Information about an ongoing search.
// Includes the target NodeID, the bitmask to match it to an IP, and the list of nodes to visit / already visited.
type searchInfo struct {
	searches *searches
	dest     crypto.NodeID
	mask     crypto.NodeID
	time     time.Time
	visited  crypto.NodeID // Closest address visited so far
	callback func(*sessionInfo, error)
	// TODO context.Context for timeout and cancellation
	send uint64 // log number of requests sent
	recv uint64 // log number of responses received
}

// This stores a map of active searches.
type searches struct {
	router   *router
	searches map[crypto.NodeID]*searchInfo
}

// Initializes the searches struct.
func (s *searches) init(r *router) {
	s.router = r
	s.searches = make(map[crypto.NodeID]*searchInfo)
}

func (s *searches) reconfigure() {
	// This is where reconfiguration would go, if we had anything to do
}

// Creates a new search info, adds it to the searches struct, and returns a pointer to the info.
func (s *searches) createSearch(dest *crypto.NodeID, mask *crypto.NodeID, callback func(*sessionInfo, error)) *searchInfo {
	info := searchInfo{
		searches: s,
		dest:     *dest,
		mask:     *mask,
		time:     time.Now(),
		callback: callback,
	}
	s.searches[*dest] = &info
	return &info
}

////////////////////////////////////////////////////////////////////////////////

// Checks if there's an ongoing search related to a dhtRes.
// If there is, it adds the response info to the search and triggers a new search step.
// If there's no ongoing search, or we if the dhtRes finished the search (it was from the target node), then don't do anything more.
func (sinfo *searchInfo) handleDHTRes(res *dhtRes) {
	if res != nil {
		sinfo.recv++
		if sinfo.checkDHTRes(res) {
			return // Search finished successfully
		}
		// Use results to start an additional search thread
		infos := append([]*dhtInfo(nil), res.Infos...)
		infos = sinfo.getAllowedInfos(infos)
		if len(infos) > 0 {
			sinfo.continueSearch(infos)
		}
	}
}

// If there has been no response in too long, then this cleans up the search.
// Otherwise, it pops the closest node to the destination (in keyspace) off of the toVisit list and sends a dht ping.
func (sinfo *searchInfo) doSearchStep(infos []*dhtInfo) {
	if len(infos) > 0 {
		// Send to the next search target
		next := infos[0]
		rq := dhtReqKey{next.key, sinfo.dest}
		sinfo.searches.router.dht.addCallback(&rq, sinfo.handleDHTRes)
		sinfo.searches.router.dht.ping(next, &sinfo.dest)
		sinfo.send++
	}
}

// Get a list of search targets that are close enough to the destination to try
// Requires an initial list as input
func (sinfo *searchInfo) getAllowedInfos(infos []*dhtInfo) []*dhtInfo {
	sort.SliceStable(infos, func(i, j int) bool {
		// Should return true if i is closer to the destination than j
		return dht_ordered(&sinfo.dest, infos[i].getNodeID(), infos[j].getNodeID())
	})
	// Remove anything too far away to be useful
	for idx, info := range infos {
		if !dht_ordered(&sinfo.dest, info.getNodeID(), &sinfo.visited) {
			infos = infos[:idx]
			break
		}
	}
	return infos
}

// Run doSearchStep and schedule another continueSearch to happen after search_RETRY_TIME.
// Must not be called with an empty list of infos
func (sinfo *searchInfo) continueSearch(infos []*dhtInfo) {
	sinfo.doSearchStep(infos)
	infos = infos[1:] // Remove the node we just tried
	// In case there's no response, try the next node in infos later
	time.AfterFunc(search_STEP_TIME, func() {
		sinfo.searches.router.Act(nil, func() {
			// FIXME this keeps the search alive forever if not for the searches map, fix that
			newSearchInfo := sinfo.searches.searches[sinfo.dest]
			if newSearchInfo != sinfo {
				return
			}
			// Get good infos here instead of at the top, to make sure we can always start things off with a continueSearch call to ourself
			infos = sinfo.getAllowedInfos(infos)
			if len(infos) > 0 {
				sinfo.continueSearch(infos)
			}
		})
	})
}

// Initially start a search
func (sinfo *searchInfo) startSearch() {
	loc := sinfo.searches.router.core.switchTable.getLocator()
	var infos []*dhtInfo
	infos = append(infos, &dhtInfo{
		key:    sinfo.searches.router.core.boxPub,
		coords: loc.getCoords(),
	})
	// Start the search by asking ourself, useful if we're the destination
	sinfo.continueSearch(infos)
	// Start a timer to clean up the search if everything times out
	var cleanupFunc func()
	cleanupFunc = func() {
		sinfo.searches.router.Act(nil, func() {
			// FIXME this keeps the search alive forever if not for the searches map, fix that
			newSearchInfo := sinfo.searches.searches[sinfo.dest]
			if newSearchInfo != sinfo {
				return
			}
			elapsed := time.Since(sinfo.time)
			if elapsed > search_RETRY_TIME {
				// cleanup
				delete(sinfo.searches.searches, sinfo.dest)
				sinfo.callback(nil, errors.New("search reached dead end"))
				return
			}
			time.AfterFunc(search_RETRY_TIME-elapsed, cleanupFunc)
		})
	}
	time.AfterFunc(search_RETRY_TIME, cleanupFunc)
}

// Calls create search, and initializes the iterative search parts of the struct before returning it.
func (s *searches) newIterSearch(dest *crypto.NodeID, mask *crypto.NodeID, callback func(*sessionInfo, error)) *searchInfo {
	sinfo := s.createSearch(dest, mask, callback)
	sinfo.visited = s.router.dht.nodeID
	return sinfo
}

// Checks if a dhtRes is good (called by handleDHTRes).
// If the response is from the target, get/create a session, trigger a session ping, and return true.
// Otherwise return false.
func (sinfo *searchInfo) checkDHTRes(res *dhtRes) bool {
	from := dhtInfo{key: res.Key, coords: res.Coords}
	if *from.getNodeID() != sinfo.visited && dht_ordered(&sinfo.dest, from.getNodeID(), &sinfo.visited) {
		// Closer to the destination, so update visited
		sinfo.searches.router.core.log.Debugln("Updating search:", &sinfo.dest, from.getNodeID(), sinfo.send, sinfo.recv)
		sinfo.visited = *from.getNodeID()
		sinfo.time = time.Now()
	}
	them := from.getNodeID()
	var destMasked crypto.NodeID
	var themMasked crypto.NodeID
	for idx := 0; idx < crypto.NodeIDLen; idx++ {
		destMasked[idx] = sinfo.dest[idx] & sinfo.mask[idx]
		themMasked[idx] = them[idx] & sinfo.mask[idx]
	}
	if themMasked != destMasked {
		return false
	}
	finishSearch := func(sess *sessionInfo, err error) {
		if sess != nil {
			// FIXME (!) replay attacks could mess with coords? Give it a handle (tstamp)?
			sess.Act(sinfo.searches.router, func() { sess.coords = res.Coords })
			sess.ping(sinfo.searches.router)
		}
		if err != nil {
			sinfo.callback(nil, err)
		} else {
			sinfo.callback(sess, nil)
		}
		// Cleanup
		if _, isIn := sinfo.searches.searches[sinfo.dest]; isIn {
			sinfo.searches.router.core.log.Debugln("Finished search:", &sinfo.dest, sinfo.send, sinfo.recv)
			delete(sinfo.searches.searches, res.Dest)
		}
	}
	// They match, so create a session and send a sessionRequest
	var err error
	sess, isIn := sinfo.searches.router.sessions.getByTheirPerm(&res.Key)
	if !isIn {
		// Don't already have a session
		sess = sinfo.searches.router.sessions.createSession(&res.Key)
		if sess == nil {
			err = errors.New("session not allowed")
		} else if _, isIn := sinfo.searches.router.sessions.getByTheirPerm(&res.Key); !isIn {
			panic("This should never happen")
		}
	} else {
		err = errors.New("session already exists")
	}
	finishSearch(sess, err)
	return true
}
