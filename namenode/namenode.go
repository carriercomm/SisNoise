// Package datanode contains the functionality to run a namenode in SisNoise
package namenode

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	simplejson "github.com/bitly/go-simplejson"
	. "github.com/pokerG/SisNoise/common"
)

// Config Options
var host string         // listen host
var port string         // listen port
var SIZEOFBLOCK int     //size of block in bytes
var id string           // the namenode id
var metadatapath string //the meta-data directory

var headerChannel chan BlockHeader   // processes headers into filesystem
var sendChannel chan Packet          //  enqueued packets for transmission
var sendMap map[string]*json.Encoder // maps DatanodeIDs to their connections
var sendMapLock sync.Mutex
var clientMap map[BlockHeader]string // maps requested Blocks to the client ID which requested them, based on Blockheader
var clientMapLock sync.Mutex

var blockReceiverChannel chan Block        // used to fetch blocks on user request
var blockRequestorChannel chan BlockHeader // used to send block requests

var root *filenode                             // the filesystem
var filemap map[string](map[int][]BlockHeader) // filenames to blocknumbers to headers
var datanodemap map[string]*datanode           // filenames to datanodes
var fsTree *simplejson.Json                    //the File system tree

// filenodes compose an internal tree representation of the filesystem
type filenode struct {
	path     string
	parent   *filenode
	children []*filenode
}

// Represent connected Datanodes
// Hold file and connection information
type datanode struct {
	ID     string
	listed bool
	size   int64
}

// By is used to select the fields used when comparing datanodes
type By func(p1, p2 *datanode) bool

// Sort sorts an array of datanodes by the function By
func (by By) Sort(nodes []datanode) {
	s := &datanodeSorter{
		nodes: nodes,
		by:    by, // The Sort method's receiver is the function (closure) that defines the sort order.
	}
	sort.Sort(s)
}

// structure to sort datanodes
type datanodeSorter struct {
	nodes []datanode
	by    func(p1, p2 *datanode) bool // Closure used in the Less method.
}

func (s *datanodeSorter) Len() int {
	return len(s.nodes)
}
func (s *datanodeSorter) Swap(i, j int) {
	s.nodes[i], s.nodes[j] = s.nodes[j], s.nodes[i]
}

// Less is part of sort.Interface. It is implemented by calling the "by" closure in the sorter.
func (s *datanodeSorter) Less(i, j int) bool {
	return s.by(&s.nodes[i], &s.nodes[j])
}

// Sendpacket abstracts packet sending details
func (dn *datanode) SendPacket(p Packet) {
	sendChannel <- p
}

// HandleBlockHeaders reads incoming BlockHeaders and merges them into the filesystem
func HandleBlockHeaders() {
	for h := range headerChannel {
		MergeNode(h)
	}
}

// ContainsHeader searches a BlockHeader for a given BlockHeader
func ContainsHeader(arr []BlockHeader, h BlockHeader) bool {
	for _, v := range arr {
		if h == v {
			return true
		}
	}
	return false
}

type errorString struct {
	s string
}

func (e *errorString) Error() string {
	return e.s
}

// New returns an error that formats as the given text.
func New(text string) error {
	return &errorString{text}
}

// listFiles is a recursive helper for ListFiles
func listFiles(node string, input string, depth int) string {

	pre := fsTree.Get(node)
	s, _ := pre.String()
	if s == "file" {
		input = strings.Repeat(" ", depth) + node + "\n"
		return input
	} else {
		input = strings.Repeat(" ", depth-1) + "-" + node + "\n"
		arr, _ := pre.Array()
		for _, v := range arr {
			input += listFiles(v.(string), input, depth+1)
		}
	}

	return input

}

func ListFiles(path string) string {
	paths := strings.Split(path, "/")
	var node string
	if len(paths) == 2 && paths[0] == paths[1] == "" { // "/"
		node = "/"
	} else if paths[len(paths)-1] == "" { //"/1/2/"
		node = paths[len(paths)-2]
	} else {
		node = paths[len(paths)-1] //"/1/2"
	}
	input := ""
	pre := fsTree.Get(node)
	s, _ := pre.String()
	if s == "file" {
		input = node + "\n"
	} else {
		input = node + "\n"
		arr, _ := pre.Array()
		for _, v := range arr {
			input += "    " + arr.(string) + "\n"
		}
	}
	// input += listFiles("/", input, 1)

	return input

}

// Mergenode adds a BlockHeader entry to the filesystem, in its correct location
func MergeNode(h BlockHeader) error {

	if &h == nil || h.DatanodeID == "" || h.Filename == "" || h.Size < 0 || h.BlockNum < 0 || h.NumBlocks < h.BlockNum {
		return errors.New("Invalid header input")
	}

	dn, ok := datanodemap[h.DatanodeID]
	if !ok {
		return errors.New("BlockHeader DatanodeID: " + h.DatanodeID + " does not exist in map")
	}

	path := h.Filename
	path_arr := strings.Split(path, "/")
	q := root

	for i, _ := range path_arr {
		//skip
		if i == 0 {
			continue
		} else {

			partial := strings.Join(path_arr[0:i+1], "/")
			exists := false
			for _, v := range q.children {
				if v.path == partial {
					q = v
					exists = true
					break
				}
			}
			if exists {
				// If file already been added, we add the BlockHeader to the map
				if partial == path {
					blks, ok := filemap[path]
					if !ok {
						return errors.New("Attempted to add to filenode that does not exist!")
					}
					_, ok = blks[h.BlockNum]
					if !ok {
						filemap[partial][h.BlockNum] = make([]BlockHeader, 1, 1)
						filemap[partial][h.BlockNum][0] = h

					} else {
						if !ContainsHeader(filemap[path][h.BlockNum], h) {
							filemap[path][h.BlockNum] = append(filemap[path][h.BlockNum], h)

						}
					}
					dn.size += int64(h.Size)
					//fmt.Println("adding Block header # ", h.BlockNum, "to filemap at ", path)
				}

				//else it is a directory
				continue
			} else {

				//  if we are at file, create the map entry
				n := &filenode{partial, q, make([]*filenode, 1)}
				if partial == path {
					filemap[partial] = make(map[int][]BlockHeader)
					filemap[partial][h.BlockNum] = make([]BlockHeader, 1, 1)
					filemap[partial][h.BlockNum][0] = h
					dn.size += int64(h.Size)
					//fmt.fmt("creating Block header # ", h.BlockNum, "to filemap at ", path)
				}

				n.parent = q
				q.children = append(q.children, n)
				q = n
			}
		}
	}
	return nil
}

// AssignBlocks creates packets based on BlockHeader metadata and enqueues them for transmission
func AssignBlocks(bls []Block) {
	for _, b := range bls {
		AssignBlock(b)
	}
}

// AssignBlocks chooses a datanode which balances the load across nodes for a block and enqueues
// the block for distribution
func AssignBlock(b Block) (Packet, error) {
	p := new(Packet)

	if &b == nil || &b.Header == nil || &b.Data == nil || b.Header.Filename == "" ||
		b.Header.Size <= 0 || b.Header.BlockNum < 0 || b.Header.NumBlocks <= b.Header.BlockNum {
		return *p, errors.New("Invalid Block input")
	}

	if len(datanodemap) < 1 {
		return *p, errors.New("Cannot distribute Block, no datanodes are connected")
	}

	// Create Packet and send block
	p.SRC = id
	p.CMD = BLOCK

	//Random load balancing
	nodeIDs := make([]string, len(datanodemap), len(datanodemap))
	i := 0
	for _, v := range datanodemap {
		nodeIDs[i] = v.ID
		i++
	}
	rand.Seed(time.Now().UTC().UnixNano())
	nodeindex := rand.Intn(len(nodeIDs))
	p.DST = nodeIDs[nodeindex]
	b.Header.DatanodeID = p.DST

	p.Data = Block{b.Header, b.Data}

	return *p, nil

}

// SendPackets encodes packets and transmits them to their proper recipients
func SendPackets() {
	for p := range sendChannel {

		sendMapLock.Lock()
		encoder, ok := sendMap[p.DST]
		if !ok {
			fmt.Println("Could not find encoder for ", p.DST)
			continue
		}
		err := encoder.Encode(p)
		if err != nil {
			fmt.Println("Error sending", p.DST)
		}
		sendMapLock.Unlock()
	}
}

// WriteJSON writes a JSON encoded interface to disc
func WriteJSON(fileName string, key interface{}) {
	outFile, err := os.Create(fileName)
	if err != nil {
		fmt.Println("Error opening JSON ", err)
		return
	}
	defer outFile.Close()
	encoder := json.NewEncoder(outFile)
	err = encoder.Encode(key)
	if err != nil {
		fmt.Println("Error encoding JSON ", err)
		return
	}
}

// Handle handles a packet and performs the proper action based on its contents

func HandlePacket(p Packet) {

	defer func() {
		if r := recover(); r != nil {
			fmt.Println("Recovered from panic ", r)
			fmt.Println("Unable to handle packet ", p)
			return
		}
	}()

	if p.SRC == "" {
		fmt.Println("Could not identify packet")
		return
	}

	r := Packet{id, p.SRC, ACK, "", *new(Block), make([]BlockHeader, 0)}

	if p.SRC == "C" {

		switch p.CMD {
		case HB:
			fmt.Println("Received client connection", p.SRC)
			return
		case LIST:
			fmt.Println("Received List Request")
			r.Message = ListFiles(p.Message)
			r.CMD = LIST
			fmt.Println(r)

		case DISTRIBUTE:
			b := p.Data
			FSBuilding(b.Header.Filename)
			fmt.Println("Distributing Block ", b.Header.Filename, "/", b.Header.BlockNum, " to ", b.Header.DatanodeID)
			p, err := AssignBlock(b)
			if err != nil {
				r.CMD = ERROR
				r.Message = err.Error()
			}
			sendChannel <- p

			r.CMD = ACK
		case RETRIEVEBLOCK:
			r.CMD = RETRIEVEBLOCK
			if p.Headers == nil || len(p.Headers) != 1 {
				r.CMD = ERROR
				r.Message = "Invalid Header received"
				fmt.Println("Invalid RETRIEVEBLOCK Packet , ", p)
				break
			}

			r.DST = p.Headers[0].DatanodeID // Block to retrieve is specified by given header
			fmt.Println("Retrieving Block for client ", p.SRC, "from node ", r.DST)

			r.Headers = p.Headers
			// specify client that is requesting a block when it arrives
			clientMapLock.Lock()
			clientMap[p.Headers[0]] = p.SRC
			clientMapLock.Unlock()

		case GETHEADERS:
			r.CMD = GETHEADERS
			if p.Headers == nil || len(p.Headers) != 1 {
				r.CMD = ERROR
				r.Message = "Invalid Header received"
				fmt.Println("Received invalid Header Packet, ", p)
				break
			}

			fmt.Println("Retrieving headers for client using ", p.Headers[0])

			fname := p.Headers[0].Filename
			blockMap, ok := filemap[fname]
			if !ok {
				r.CMD = ERROR
				r.Message = "File not found " + fname
				fmt.Println("Requested file in filesystem not found, ", fname)
				break
			}

			_, ok = blockMap[0]
			if !ok {
				r.CMD = ERROR
				r.Message = "Could not locate first block in file"
				break
			}
			numBlocks := blockMap[0][0].NumBlocks

			headers := make([]BlockHeader, numBlocks, numBlocks)
			for i, _ := range headers {

				_, ok = blockMap[i]
				if !ok {
					r.CMD = ERROR
					r.Message = "Could not find needed block in file "
					break
				}
				headers[i] = blockMap[i][0] // grab the first available BlockHeader for each block number
			}
			r.Headers = headers
			fmt.Println("Retrieved headers ")
		}

	} else {
		dn := datanodemap[p.SRC]
		listed := dn.listed

		switch p.CMD {
		case HB:

			fmt.Println("Received Heartbeat from ", p.SRC)
			if !listed {
				r.CMD = LIST
			} else {
				r.CMD = ACK
			}

		case LIST:

			fmt.Println("Received BlockHeaders from ", p.SRC)
			list := p.Headers
			for _, h := range list {
				headerChannel <- h
			}
			dn.listed = true
			r.CMD = ACK

		case BLOCKACK:
			// receive acknowledgement for single Block header as being stored
			if p.Headers != nil && len(p.Headers) == 1 {
				headerChannel <- p.Headers[0]
			}
			r.CMD = ACK
			fmt.Println("Received BLOCKACK from ", p.SRC)

		case BLOCK:
			fmt.Println("Received Block Packet with header", p.Data.Header)

			// TODO map multiple clients
			//clientMapLock.Lock()
			//cID,ok := clientMap[p.Data.Header]
			//clientMapLock.Unlock()
			//if !ok {
			//	fmt.Println("Header not found in clientMap  ", p.Data.Header)
			//  return
			//}
			r.DST = "C"
			r.CMD = BLOCK
			r.Data = p.Data

		}
	}

	// send response
	sendChannel <- r

}

// Checkconnection adds or updates a connection to the namenode and handles its first packet
func CheckConnection(conn net.Conn, p Packet) {

	// C is the client(hardcode for now)
	if p.SRC == "C" {
		fmt.Println("Adding new client connection")
		sendMapLock.Lock()
		sendMap[p.SRC] = json.NewEncoder(conn)
		sendMapLock.Unlock()
	} else {
		dn, ok := datanodemap[p.SRC]
		if !ok {
			fmt.Println("Adding new datanode :", p.SRC)
			datanodemap[p.SRC] = &datanode{p.SRC, false, 0}
		} else {
			fmt.Printf("Datanode %s reconnected \n", dn.ID)
		}
		sendMapLock.Lock()
		sendMap[p.SRC] = json.NewEncoder(conn)
		sendMapLock.Unlock()
		dn = datanodemap[p.SRC]
	}
	HandlePacket(p)
}

// Handle Connection initializes the connection and performs packet retrieval
func HandleConnection(conn net.Conn) {

	// receive first Packet and add datanode if necessary
	var p Packet
	decoder := json.NewDecoder(conn)
	err := decoder.Decode(&p)
	if err != nil {
		fmt.Println("Unable to communicate with node")
	}
	CheckConnection(conn, p)
	dn := datanodemap[p.SRC]

	// receive packets and handle
	for {
		var p Packet
		err := decoder.Decode(&p)
		if err != nil {
			fmt.Println("Datanode ", dn.ID, " disconnected!")
			return
		}
		HandlePacket(p)
	}
}

func FSBuilding(filename string) {
	paths := strings.Split(filename, "#")
	changeflag := false
	var prepath string
	for k, v := range paths {
		_, isExist := fsTree.CheckGet(v)
		if !isExist {
			changeflag = true
			subnode, _ := fsTree.Get(prepath).Array()
			fsTree.Set(prepath, append(subnode, v))
			if k == len(paths)-1 {
				fsTree.Set(v, "file")
			} else {
				fsTree.Set(v, new([]string))
			}
		}
		prepath = v
	}
	if changeflag {
		b, _ := fsTree.MarshalJSON()
		fmt.Println(string(b))
		ioutil.WriteFile(metadatapath+"/meta", b, 0666)
	}

}

// Parse Config sets up the node with the provided XML file
func ParseConfigXML(configpath string) error {
	xmlFile, err := os.Open(configpath)
	if err != nil {
		return err
	}
	defer xmlFile.Close()

	var list ConfigOptionList
	err = xml.NewDecoder(xmlFile).Decode(&list)
	if err != nil {
		return err
	}

	for _, o := range list.ConfigOptions {
		switch o.Key {
		case "namenodeid":
			id = o.Value
		case "listenhost":
			host = o.Value
		case "listenport":
			port = o.Value
		case "sizeofblock":
			n, err := strconv.Atoi(o.Value)
			if err != nil {
				return err
			}

			if n < 4096 {
				return errors.New("Buffer size must be greater than or equal to 4096 bytes")
			}
			SIZEOFBLOCK = n
		case "metadatapath":
			metadatapath = o.Value
		default:
			return errors.New("Bad ConfigOption received Key : " + o.Key + " Value : " + o.Value)
		}
	}

	return nil
}

// Parse the struct tree of file system
func ParseCatalogue(path string) error {
	b, _ := ioutil.ReadFile(path)
	var err error
	fsTree, err = simplejson.NewJson(b)
	if err == nil {

		s, _ := fsTree.MarshalJSON()
		fmt.Println(string(s))
	} else {
		log.Fatal("Fatal error ", err.Error())
	}
	return nil
}

// Init initializes all internal structures to run a namnode and handles incoming connections
func Init(configpath string) {

	// Read config
	err := ParseConfigXML(configpath)

	if err != nil {
		log.Fatal("Fatal error ", err.Error())
	}

	// setup filesystem
	root = &filenode{"/", nil, make([]*filenode, 0, 1)}
	filemap = make(map[string]map[int][]BlockHeader)
	err = os.Chdir(metadatapath)
	if os.IsNotExist(err) {
		os.Mkdir(metadatapath, 0700)
		os.Create(metadatapath + "/meta")
		os.Create(metadatapath + "/datanode")
		jsonStr := "{\"/\":[]}"
		fsTree, _ = simplejson.NewJson([]byte(jsonStr))
		b, _ := fsTree.MarshalJSON()
		ioutil.WriteFile(metadatapath+"/meta", b, 0666)
	} else {
		ParseCatalogue(metadatapath + "/meta")
	}

	// setup communication
	headerChannel = make(chan BlockHeader)
	sendChannel = make(chan Packet)
	sendMap = make(map[string]*json.Encoder)
	sendMapLock = sync.Mutex{}
	clientMap = make(map[BlockHeader]string)
	clientMapLock = sync.Mutex{}

	datanodemap = make(map[string]*datanode)

}

// Run starts the namenode
func Run(configpath string) {

	// setup filesystem
	Init(configpath)

	// Start communication
	go HandleBlockHeaders()
	go SendPackets()

	listener, err := net.Listen("tcp", host+":"+port)
	if err != nil {
		log.Fatal("Fatal error ", err.Error())
	}

	// listen for datanode connections
	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Println("Connection error ", err.Error())
			continue
		}
		go HandleConnection(conn)
	}
}
