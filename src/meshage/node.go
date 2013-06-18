package meshage

import (
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"math/rand"
	log "minilog"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	DEFAULT_TIMEOUT     = 5 // wait for ACK for 5 seconds
	DEFAULT_TTL         = 1
	RECEIVE_BUFFER      = 1024
	DEFAULT_MSA_TIMEOUT = 10
)

type mesh map[string][]string

// A Node object contains the network information for a given node. Creating a
// Node object with a non-zero degree will cause it to begin broadcasting for
// connections automatically.
type Node struct {
	name             string             // node name, must be unique on a network
	degree           uint               // degree for this node, set to 0 to force node not to broadcast
	network          mesh               // adjacency list for the known topology for this node
	effectiveNetwork mesh               // effective topology for pairwise connections from the network
	routes           map[string]string  // one-hop routes for every node on the network, including this node
	receive          chan *Message      // channel of incoming messages, A program will read this channel for incoming messages to this node
	clients          map[string]*client // list of clients to this node
	port             int                // port to operate on, uses both tcp and udp
	timeout          time.Duration      // timeout for response waits
	msaTimeout       uint
	ttl              int        // time to live
	errors           chan error // channel of asynchronous errors generated by meshage
	messagePump      chan *Message
	sequences        map[string]uint64
	clientLock       sync.Mutex
	sequenceLock     sync.Mutex
	degreeLock       sync.Mutex
	meshLock         sync.Mutex
}

func init() {
	gob.Register(mesh{})
}

// NewNode returns a new node, receiver channel, and error channel with a given name
// and degree. If degree is non-zero, the node will automatically begin broadcasting
// for connections.
func NewNode(name string, degree uint, port int) (*Node, chan *Message) {
	log.Debug("NewNode: %v %v %v", name, degree, port)
	n := &Node{
		name:             name,
		degree:           degree,
		network:          make(mesh),
		effectiveNetwork: make(mesh),
		routes:           make(map[string]string),
		receive:          make(chan *Message, RECEIVE_BUFFER),
		clients:          make(map[string]*client),
		port:             port,
		timeout:          time.Duration(DEFAULT_TIMEOUT * time.Second),
		msaTimeout:       DEFAULT_MSA_TIMEOUT,
		ttl:              DEFAULT_TTL,
		errors:           make(chan error),
		messagePump:      make(chan *Message, RECEIVE_BUFFER),
		sequences:        make(map[string]uint64),
	}

	go n.connectionListener()
	go n.broadcastListener()
	go n.messageHandler()
	go n.checkDegree()
	go n.periodicMSA()

	return n, n.receive
}

// Dial connects a node to another, regardless of degree. Error is nil on success.
func (n *Node) Dial(addr string) error {
	return n.dial(addr, false)
}

// SetDegree sets the degree for the current node. If the degree increases beyond
// the current number of connected clients, it will begin broadcasting for connections.
func (n *Node) SetDegree(degree uint) {
	n.degree = degree
	go n.checkDegree()
}

// GetDegree returns the current degree for the node.
func (n *Node) GetDegree() uint {
	return n.degree
}

// Mesh returns the current known topology as an adjacency list.
func (n *Node) Mesh() mesh {
	n.meshLock.Lock()
	defer n.meshLock.Unlock()

	ret := make(mesh)
	for k, v := range n.effectiveNetwork {
		ns := make([]string, len(v))
		copy(ns, v)
		ret[k] = ns
	}
	return ret
}

// connectionListener accepts connections on tcp/port for both solicited and unsolicited
// client connections.
func (n *Node) connectionListener() {
	log.Debugln("connectionListener")
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", n.port))
	if err != nil {
		log.Fatalln(err)
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Warnln(err)
			continue
		}
		n.newConnection(conn)
	}
}

// newConnection processes a new incoming connection from another node, processes the connection
// handshake, adds the connection to the client list, and starts the client message handler.
func (n *Node) newConnection(conn net.Conn) {
	log.Debug("newConnection: %v", conn.RemoteAddr().String())

	// are we soliciting connections?
	var solicited bool
	if uint(len(n.clients)) < n.degree {
		solicited = true
	} else {
		solicited = false
	}
	log.Debug("solicited: %v", solicited)

	c := &client{
		conn: conn,
		enc:  gob.NewEncoder(conn),
		dec:  gob.NewDecoder(conn),
		ack:  make(chan uint64, RECEIVE_BUFFER),
	}

	// the handshake involves the following:
	// 1.  We send our name and our solicitation status
	// 2a. If the connection is solicited but we're all full, the remote node simply hangs up
	// 2b. If the connection is unsolicited or solicited and we are still soliciting connections, the remote node responds with its name
	// 3.  The connection is valid, add it to our client list and broadcast a MSA announcing the new connection.
	// 4.  The remote node does the same as 3.
	err := c.enc.Encode(n.name)
	if err != nil {
		log.Errorln(err)
		return
	}

	err = c.enc.Encode(solicited)
	if err != nil {
		log.Errorln(err)
		return
	}

	var resp string
	err = c.dec.Decode(&resp)
	if err != nil {
		if err != io.EOF {
			log.Errorln(err)
		}
		return
	}

	c.name = resp
	log.Debug("handshake from: %v", c.name)

	n.clientLock.Lock()
	n.clients[resp] = c
	n.clientLock.Unlock()

	go n.clientHandler(resp)
}

// broadcastListener listens for broadcast connection solicitations and connects to
// soliciting nodes.
func (n *Node) broadcastListener() {
	listenAddr := net.UDPAddr{
		IP:   net.IPv4(0, 0, 0, 0),
		Port: n.port,
	}
	ln, err := net.ListenUDP("udp4", &listenAddr)
	if err != nil {
		log.Fatalln(err)
	}
	for {
		d := make([]byte, 1024)
		read, _, err := ln.ReadFromUDP(d)
		data := strings.Split(string(d[:read]), ":")
		if len(data) != 2 {
			err = fmt.Errorf("got malformed udp data: %v\n", data)
			log.Warnln(err)
			continue
		}
		if data[0] != "meshage" {
			err = fmt.Errorf("got malformed udp data: %v\n", data)
			log.Warnln(err)
			continue
		}
		host := data[1]
		if host == n.name {
			log.Debugln("got solicitation from myself, dropping")
			continue
		}
		log.Debug("got solicitation from %v", host)
		go n.dial(host, true)
	}
}

// checkDegree broadcasts connection solicitations with exponential backoff until
// the degree is met, then returns. checkDegree locks and will cause the caller to block
// until the degree is met. It should only be run as a goroutine.
func (n *Node) checkDegree() {
	// check degree only if we're not already running
	n.degreeLock.Lock()
	defer n.degreeLock.Unlock()

	var backoff uint = 1
	s := rand.NewSource(time.Now().UnixNano())
	r := rand.New(s)
	for uint(len(n.clients)) < n.degree {
		log.Debugln("soliciting connections")
		b := net.IPv4(255, 255, 255, 255)
		addr := net.UDPAddr{
			IP:   b,
			Port: n.port,
		}
		socket, err := net.DialUDP("udp4", nil, &addr)
		if err != nil {
			log.Errorln(err)
			break
		}
		message := fmt.Sprintf("meshage:%s", n.name)
		_, err = socket.Write([]byte(message))
		if err != nil {
			log.Errorln(err)
			break
		}
		wait := r.Intn(1 << backoff)
		time.Sleep(time.Duration(wait) * time.Second)
		if backoff < 7 { // maximum wait won't exceed 128 seconds
			backoff++
		}
	}
}

// dial another node, perform a handshake, and add the client to the client list if successful
func (n *Node) dial(host string, solicited bool) error {
	addr := fmt.Sprintf("%s:%d", host, n.port)
	log.Debug("dialing: %v", addr)

	conn, err := net.DialTimeout("tcp", addr, DEFAULT_TIMEOUT*time.Second)
	if err != nil {
		if solicited {
			log.Errorln(err)
		}
		return err
	}

	c := &client{
		conn: conn,
		enc:  gob.NewEncoder(conn),
		dec:  gob.NewDecoder(conn),
		ack:  make(chan uint64, RECEIVE_BUFFER),
	}

	var remoteHost string
	err = c.dec.Decode(&remoteHost)
	if err != nil {
		if solicited {
			log.Errorln(err)
		}
		return err
	}

	var remoteSolicited bool
	err = c.dec.Decode(&remoteSolicited)
	if err != nil {
		if solicited {
			log.Errorln(err)
		}
		return err
	}

	// are we already connected to this node?
	for k, _ := range n.clients {
		if k == remoteHost {
			conn.Close()
			err = errors.New("already connected")
			return err
		}
	}

	// we should hangup if the connection no longer wants solicited connections and we're solicited
	if solicited && !remoteSolicited {
		conn.Close()
		return nil
	}

	err = c.enc.Encode(n.name)
	if err != nil {
		if solicited {
			log.Errorln(err)
		}
		return err
	}

	c.name = remoteHost
	log.Debug("handshake from: %v", remoteHost)

	n.clientLock.Lock()
	n.clients[remoteHost] = c
	n.clientLock.Unlock()

	go n.clientHandler(remoteHost)
	return nil
}

// MSA issues a Meshage State Annoucement, which contains a list of all the nodes connected to the broadcaster
func (n *Node) MSA() {
	log.Debugln("MSA")

	n.clientLock.Lock()
	var clients []string
	for k, _ := range n.clients {
		clients = append(clients, k)
	}
	n.clientLock.Unlock()

	sort.Strings(clients)

	n.meshLock.Lock()
	diff := false
	if len(n.network[n.name]) != len(clients) {
		diff = true
	} else {
		for i, v := range n.network[n.name] {
			if clients[i] != v {
				diff = true
				break
			}
		}
	}
	if diff {
		log.Debugln("client list changed, recalculating topology")
		n.network[n.name] = clients
		n.generateEffectiveNetwork()
	}
	n.meshLock.Unlock()

	log.Debug("client list: %v", clients)

	m := &Message{
		Source:       n.name,
		CurrentRoute: []string{n.name},
		ID:           n.sequence(),
		Command:      MSA,
		Body:         clients,
	}
	n.flood(m)
}

func (n *Node) sequence() uint64 {
	log.Debugln("sequence")
	n.sequenceLock.Lock()
	defer n.sequenceLock.Unlock()
	n.sequences[n.name]++
	ret := n.sequences[n.name]
	return ret
}

func (n *Node) handleMSA(m *Message) {
	log.Debug("handleMSA: %v", m)

	if len(n.network[m.Source]) == len(m.Body.([]string)) {
		diff := false
		for i, v := range n.network[m.Source] {
			if m.Body.([]string)[i] != v {
				diff = true
				break
			}
		}
		if !diff {
			log.Debugln("MSA discarded, client data hasn't changed")
			return
		}
	}

	n.meshLock.Lock()
	defer n.meshLock.Unlock()

	n.routes = make(map[string]string)
	n.network[m.Source] = m.Body.([]string)

	log.Debug("new network is: %v", n.network)

	n.generateEffectiveNetwork()
}

func (n *Node) periodicMSA() {
	for {
		time.Sleep(time.Duration(n.msaTimeout) * time.Second)
		n.MSA()
	}
}

func (n *Node) SetMSATimeout(timeout uint) {
	n.msaTimeout = timeout
}

func (n *Node) GetMSATimeout() uint {
	return n.msaTimeout
}
