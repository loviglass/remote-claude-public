package sftp

import (
	"encoding"
	"sort"
	"sync"
)

// The goal of the packetManager is to keep the outgoing packets in the same
// order as the incoming as is requires by section 7 of the RFC.

type packetManager struct {
	requests        chan orderedPacket
	responses       chan orderedPacket
	fini            chan struct{}
	incoming        orderedPackets
	outgoing        orderedPackets
	sender          packetSender // connection object
	working         *sync.WaitGroup
	packetCount     uint32
	concurrentReads bool
	readOnlyHandle  func(string) bool
	handleMu        sync.Mutex
	handles         map[string]*handleQueue
	completions     map[uint32]handleCompletion
	// it is not nil if the allocator is enabled
	alloc *allocator
}

type packetSender interface {
	sendPacket(encoding.BinaryMarshaler) error
}

func newPktMgr(sender packetSender) *packetManager {
	s := &packetManager{
		requests:  make(chan orderedPacket, SftpServerWorkerCount),
		responses: make(chan orderedPacket, SftpServerWorkerCount),
		fini:      make(chan struct{}),
		incoming:  make([]orderedPacket, 0, SftpServerWorkerCount),
		outgoing:  make([]orderedPacket, 0, SftpServerWorkerCount),
		sender:    sender,
		working:   &sync.WaitGroup{},
	}
	go s.controller()
	return s
}

// // packet ordering
func (s *packetManager) newOrderID() uint32 {
	s.packetCount++
	return s.packetCount
}

// returns the next orderID without incrementing it.
// This is used before receiving a new packet, with the allocator enabled, to associate
// the slice allocated for the received packet with the orderID that will be used to mark
// the allocated slices for reuse once the request is served
func (s *packetManager) getNextOrderID() uint32 {
	return s.packetCount + 1
}

type orderedRequest struct {
	requestPacket
	orderid uint32
}

func (s *packetManager) newOrderedRequest(p requestPacket) orderedRequest {
	return orderedRequest{requestPacket: p, orderid: s.newOrderID()}
}
func (p orderedRequest) orderID() uint32 { return p.orderid }

type orderedResponse struct {
	responsePacket
	orderid uint32
}

func (s *packetManager) newOrderedResponse(p responsePacket, id uint32,
) orderedResponse {
	return orderedResponse{responsePacket: p, orderid: id}
}
func (p orderedResponse) orderID() uint32 { return p.orderid }

type orderedPacket interface {
	id() uint32
	orderID() uint32
}
type orderedPackets []orderedPacket

func (o orderedPackets) Sort() {
	sort.Slice(o, func(i, j int) bool {
		return o[i].orderID() < o[j].orderID()
	})
}

// // packet registry
// register incoming packets to be handled
func (s *packetManager) incomingPacket(pkt orderedRequest) {
	s.working.Add(1)
	s.requests <- pkt
}

// register outgoing packets as being ready
func (s *packetManager) readyPacket(pkt orderedResponse) {
	if s.concurrentReads {
		s.completeHandle(pkt.orderID())
	}
	s.responses <- pkt
	s.working.Done()
}

// shut down packetManager controller
func (s *packetManager) close() {
	// pause until current packets are processed
	s.working.Wait()
	close(s.fini)
}

// Passed a worker function, returns a channel for incoming packets.
// Keep process packet responses in the order they are received while
// maximizing throughput of file transfers.
func (s *packetManager) workerChan(runWorker func(chan orderedRequest),
) chan orderedRequest {
	// multiple workers for faster read/writes
	rwChan := make(chan orderedRequest, SftpServerWorkerCount)
	for i := 0; i < SftpServerWorkerCount; i++ {
		runWorker(rwChan)
	}

	// single worker to enforce sequential processing of everything else
	cmdChan := make(chan orderedRequest)
	runWorker(cmdChan)

	pktChan := make(chan orderedRequest, SftpServerWorkerCount)
	go func() {
		waiting := make(chan struct{}, SftpServerWorkerCount)
		for pkt := range pktChan {
			if s.concurrentReads {
				readOnly := concurrentRead(pkt.requestPacket)
				if close, ok := pkt.requestPacket.(*sshFxpClosePacket); ok && s.readOnlyHandle != nil {
					readOnly = s.readOnlyHandle(close.getHandle())
				}
				if readOnly {
					s.incomingPacket(pkt)
					dependencies := s.handleDependencies(pkt)
					if len(dependencies) == 0 {
						rwChan <- pkt
					} else {
						// Wait outside the worker pool so a pending close or
						// directory cursor cannot occupy all workers or stall
						// unrelated cached metadata. Bound queued waiters.
						waiting <- struct{}{}
						go func() {
							defer func() { <-waiting }()
							for _, done := range dependencies {
								<-done
							}
							rwChan <- pkt
						}()
					}
				} else {
					// Every operation that can change file contents or metadata
					// waits for prior reads and finishes before later requests.
					s.working.Wait()
					s.incomingPacket(pkt)
					cmdChan <- pkt
					s.working.Wait()
				}
				continue
			}
			switch pkt.requestPacket.(type) {
			case *sshFxpReadPacket, *sshFxpWritePacket:
				s.incomingPacket(pkt)
				rwChan <- pkt
				continue
			case *sshFxpClosePacket:
				// wait for reads/writes to finish when file is closed
				// incomingPacket() call must occur after this
				s.working.Wait()
			}
			s.incomingPacket(pkt)
			// all non-RW use sequential cmdChan
			cmdChan <- pkt
		}
		if s.concurrentReads {
			// Dependency waiters can still be sending requests to rwChan.
			s.working.Wait()
		}
		close(rwChan)
		close(cmdChan)
		s.close()
	}()

	return pktChan
}

func concurrentRead(p requestPacket) bool {
	switch p := p.(type) {
	case *sshFxpReadPacket, *sshFxpStatPacket, *sshFxpLstatPacket, *sshFxpFstatPacket,
		*sshFxpRealpathPacket, *sshFxpReadlinkPacket, *sshFxpOpendirPacket, *sshFxpReaddirPacket:
		return true
	case *sshFxpOpenPacket:
		return p.Pflags&sshFxfRead != 0 && p.Pflags&(sshFxfWrite|sshFxfAppend|sshFxfCreat|sshFxfTrunc|sshFxfExcl) == 0
	}
	return false
}

type handleQueue struct {
	pending map[uint32]chan struct{}
	barrier chan struct{}
}

type handleCompletion struct {
	handle string
	queue  *handleQueue
	done   chan struct{}
}

// Reads of the same file may run concurrently, but a read-only close waits for
// all earlier operations on that handle. READDIR serializes its cursor only.
func (s *packetManager) handleDependencies(pkt orderedRequest) []<-chan struct{} {
	h, ok := pkt.requestPacket.(hasHandle)
	if !ok {
		return nil
	}
	s.handleMu.Lock()
	defer s.handleMu.Unlock()
	if s.handles == nil {
		s.handles = make(map[string]*handleQueue)
		s.completions = make(map[uint32]handleCompletion)
	}
	handle := h.getHandle()
	queue := s.handles[handle]
	if queue == nil {
		queue = &handleQueue{pending: make(map[uint32]chan struct{})}
		s.handles[handle] = queue
	}
	var dependencies []<-chan struct{}
	done := make(chan struct{})
	switch pkt.requestPacket.(type) {
	case *sshFxpClosePacket, *sshFxpReaddirPacket:
		for _, previous := range queue.pending {
			dependencies = append(dependencies, previous)
		}
		queue.barrier = done
	default:
		if queue.barrier != nil {
			dependencies = append(dependencies, queue.barrier)
		}
	}
	queue.pending[pkt.orderID()] = done
	s.completions[pkt.orderID()] = handleCompletion{handle, queue, done}
	return dependencies
}

func (s *packetManager) completeHandle(id uint32) {
	s.handleMu.Lock()
	defer s.handleMu.Unlock()
	if request, ok := s.completions[id]; ok {
		delete(s.completions, id)
		delete(request.queue.pending, id)
		if request.queue.barrier == request.done {
			request.queue.barrier = nil
		}
		if len(request.queue.pending) == 0 {
			delete(s.handles, request.handle)
		}
		close(request.done)
	}
}

// process packets
func (s *packetManager) controller() {
	for {
		select {
		case pkt := <-s.requests:
			debug("incoming id (oid): %v (%v)", pkt.id(), pkt.orderID())
			s.incoming = append(s.incoming, pkt)
			s.incoming.Sort()
		case pkt := <-s.responses:
			debug("outgoing id (oid): %v (%v)", pkt.id(), pkt.orderID())
			s.outgoing = append(s.outgoing, pkt)
			s.outgoing.Sort()
		case <-s.fini:
			return
		}
		s.maybeSendPackets()
	}
}

// send as many packets as are ready
func (s *packetManager) maybeSendPackets() {
	if s.concurrentReads {
		// A ready metadata response must not wait behind a slow file read.
		for len(s.outgoing) > 0 {
			out := s.outgoing[0]
			match := -1
			for i, in := range s.incoming {
				if in.orderID() == out.orderID() {
					match = i
					break
				}
			}
			if match < 0 {
				return // Its incoming registration has not arrived yet.
			}
			s.sender.sendPacket(out.(encoding.BinaryMarshaler))
			if s.alloc != nil {
				s.alloc.ReleasePages(out.orderID())
			}
			s.incoming = append(s.incoming[:match], s.incoming[match+1:]...)
			s.outgoing[0] = nil
			s.outgoing = s.outgoing[1:]
		}
		return
	}
	for {
		if len(s.outgoing) == 0 || len(s.incoming) == 0 {
			debug("break! -- outgoing: %v; incoming: %v",
				len(s.outgoing), len(s.incoming))
			break
		}
		out := s.outgoing[0]
		in := s.incoming[0]
		// debug("incoming: %v", ids(s.incoming))
		// debug("outgoing: %v", ids(s.outgoing))
		if in.orderID() == out.orderID() {
			debug("Sending packet: %v", out.id())
			s.sender.sendPacket(out.(encoding.BinaryMarshaler))
			if s.alloc != nil {
				// mark for reuse the slices allocated for this request
				s.alloc.ReleasePages(in.orderID())
			}
			// pop off heads
			copy(s.incoming, s.incoming[1:])            // shift left
			s.incoming[len(s.incoming)-1] = nil         // clear last
			s.incoming = s.incoming[:len(s.incoming)-1] // remove last
			copy(s.outgoing, s.outgoing[1:])            // shift left
			s.outgoing[len(s.outgoing)-1] = nil         // clear last
			s.outgoing = s.outgoing[:len(s.outgoing)-1] // remove last
		} else {
			break
		}
	}
}

// func oids(o []orderedPacket) []uint32 {
// 	res := make([]uint32, 0, len(o))
// 	for _, v := range o {
// 		res = append(res, v.orderId())
// 	}
// 	return res
// }
// func ids(o []orderedPacket) []uint32 {
// 	res := make([]uint32, 0, len(o))
// 	for _, v := range o {
// 		res = append(res, v.id())
// 	}
// 	return res
// }
