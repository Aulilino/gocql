package gocql

import (
	"log"
	"net"
	"sync"
	"time"
)

type eventDeouncer struct {
	name   string
	timer  *time.Timer
	mu     sync.Mutex
	events []frame

	callback func([]frame)
	quit     chan struct{}
}

func newEventDeouncer(name string, eventHandler func([]frame)) *eventDeouncer {
	e := &eventDeouncer{
		name:     name,
		quit:     make(chan struct{}),
		timer:    time.NewTimer(eventDebounceTime),
		callback: eventHandler,
	}
	e.timer.Stop()
	go e.flusher()

	return e
}

func (e *eventDeouncer) stop() {
	e.quit <- struct{}{} // sync with flusher
	close(e.quit)
}

func (e *eventDeouncer) flusher() {
	for {
		select {
		case <-e.timer.C:
			e.mu.Lock()
			e.flush()
			e.mu.Unlock()
		case <-e.quit:
			return
		}
	}
}

const (
	eventBufferSize   = 1000
	eventDebounceTime = 1 * time.Second
)

// flush must be called with mu locked
func (e *eventDeouncer) flush() {
	log.Printf("%s: flushing %d events\n", e.name, len(e.events))
	if len(e.events) == 0 {
		return
	}

	// if the flush interval is faster than the callback then we will end up calling
	// the callback multiple times, probably a bad idea. In this case we could drop
	// frames?
	go e.callback(e.events)
	e.events = make([]frame, 0, eventBufferSize)
}

func (e *eventDeouncer) debounce(frame frame) {
	e.mu.Lock()
	e.timer.Reset(eventDebounceTime)

	// TODO: probably need a warning to track if this threshold is too low
	if len(e.events) < eventBufferSize {
		log.Printf("%s: buffering event: %v", e.name, frame)
		e.events = append(e.events, frame)
	} else {
		log.Printf("%s: buffer full, dropping event frame: %s", e.name, frame)
	}

	e.mu.Unlock()
}

func (s *Session) handleNodeEvent(frames []frame) {
	type nodeEvent struct {
		change string
		host   net.IP
		port   int
	}

	events := make(map[string]*nodeEvent)

	for _, frame := range frames {
		// TODO: can we be sure the order of events in the buffer is correct?
		switch f := frame.(type) {
		case *topologyChangeEventFrame:
			event, ok := events[f.host.String()]
			if !ok {
				event = &nodeEvent{change: f.change, host: f.host, port: f.port}
				events[f.host.String()] = event
			}
			event.change = f.change

		case *statusChangeEventFrame:
			event, ok := events[f.host.String()]
			if !ok {
				event = &nodeEvent{change: f.change, host: f.host, port: f.port}
				events[f.host.String()] = event
			}
			event.change = f.change
		}
	}

	for addr, f := range events {
		log.Printf("NodeEvent: handling debounced event: %q => %s", addr, f.change)

		switch f.change {
		case "NEW_NODE":
			s.handleNewNode(f.host, f.port)
		case "REMOVED_NODE":
			s.handleRemovedNode(f.host, f.port)
		case "MOVED_NODE":
		// java-driver handles this, not mentioned in the spec
		// TODO(zariel): refresh token map
		case "UP":
			s.handleNodeUp(f.host, f.port)
		case "DOWN":
			s.handleNodeDown(f.host, f.port)
		}
	}
}

func (s *Session) handleEvent(framer *framer) {
	// TODO(zariel): need to debounce events frames, and possible also events
	defer framerPool.Put(framer)

	frame, err := framer.parseFrame()
	if err != nil {
		// TODO: logger
		log.Printf("gocql: unable to parse event frame: %v\n", err)
		return
	}
	log.Println(frame)

	// TODO: handle medatadata events
	switch f := frame.(type) {
	case *schemaChangeKeyspace:
	case *schemaChangeFunction:
	case *schemaChangeTable:
	case *topologyChangeEventFrame, *statusChangeEventFrame:
		s.nodeEvents.debounce(frame)
	default:
		log.Printf("gocql: invalid event frame (%T): %v\n", f, f)
	}

}

func (s *Session) handleNewNode(host net.IP, port int) {
	// TODO(zariel): need to be able to filter discovered nodes
	if s.control == nil {
		return
	}

	hostInfo, err := s.control.fetchHostInfo(host, port)
	if err != nil {
		log.Printf("gocql: unable to fetch host info for %v: %v\n", host, err)
		return
	}

	// should this handle token moving?
	if existing, ok := s.ring.addHostIfMissing(hostInfo); !ok {
		log.Printf("already have host=%v existing=%v, updating\n", hostInfo, existing)
		existing.update(hostInfo)
		hostInfo = existing
	}

	s.pool.addHost(hostInfo)
	s.hostSource.refreshRing()
}

func (s *Session) handleRemovedNode(ip net.IP, port int) {
	// we remove all nodes but only add ones which pass the filter
	addr := ip.String()
	s.pool.removeHost(addr)
	s.ring.removeHost(addr)

	s.hostSource.refreshRing()
}

func (s *Session) handleNodeUp(ip net.IP, port int) {
	addr := ip.String()
	host := s.ring.getHost(addr)
	if host != nil {
		host.setState(NodeUp)
		s.pool.hostUp(host)
		return
	}

	// TODO: this could infinite loop
	s.handleNewNode(ip, port)
}

func (s *Session) handleNodeDown(ip net.IP, port int) {
	addr := ip.String()
	host := s.ring.getHost(addr)
	if host != nil {
		host.setState(NodeDown)
	}

	s.pool.hostDown(addr)
}
