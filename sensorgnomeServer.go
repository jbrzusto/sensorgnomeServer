package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/fsnotify/fsnotify"
	"github.com/jbrzusto/mbus"
	_ "github.com/mattn/go-sqlite3"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	//	"os"
	"os/exec"
	//	"path"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// customization constants
const (
	MotusUser            = "sg@sgdata.motus.org"                                                // user on sgdata.motus.org; this is who ssh makes us be
	MotusUserKey         = "/home/sg_remote/.ssh/id_ed25519_sgorg_sgdata"                       // ssh key to use for sync on sgdata.motus.org
	MotusControlPath     = "/home/sg_remote/sgdata.ssh"                                         // control path for multiplexing port mappings to sgdata.motus.org
	MotusSyncTemplate    = "/sgm_local/sync/method=%d,serno=%s"                                 // template for file touched on sgdata.motus.org to cause sync; %d=port, %s=serno
	MotusGetProjectsUrl  = "https://motus.org/api/projects"                                     // URL for motus info on projects
	MotusGetReceiversUrl = "https://motus.org/api/receivers/deployments"                        // URL for motus info on receivers
	MotusMinLatency      = 300                                                                  // minimum time (seconds) between queries to the motus metadata server
	SernoRE              = "SG-[0-9A-Za-z]{12}"                                                 // regular expression matching SG serial number
	SyncWaitLo           = 30                                                                   // minimum time between syncs of a receiver (minutes)
	SyncWaitHi           = 90                                                                   // maximum time between syncs of a receiver (minutes)
	SyncTimeDir          = "/home/sg_remote/last_sync"                                          // directory with one file per SG; mtime is last sync time
	SGDBFile             = "/home/sg_remote/sg_remote.sqlite"                                   // sqlite database with receiver info
	ConnectionSemPath    = "/dev/shm"                                                           // directory where sshd maintains semaphores indicating connected SGs
	ConnectionSemRE      = "sem.(" + SernoRE + ")"                                              // regular expression for matching SG semaphores (capture group is serno)
	StatusPagePath       = "/home/johnb/src/sensorgnome-server/website/content/status/index.md" //path to generated page (needs group write permission and ownership by sg_remote group)
	ShortTimestampFormat = "Jan _2 15:04"                                                       // timestamp format for sync times etc. on status page
)

// The type for messages.
type SGMsg struct {
	ts     time.Time // timestamp; if 0, means not set
	sender string    // typically the SG serial number, but can be "me" for internally generated
	text   string    // typically a JSON- or CSV- formatted message
}

// type representing an SG message topic
type MsgTopic string

// Message Topics
// These are typically the first character of the message from the SG,
// but we add some synthetic ones generated by the system
const (
	MsgSGDisconnect  = "0" // connected via ssh
	MsgSGConnect     = "1" // disconnected from ssh
	MsgSGSync        = "2" // data sync with motus.org was launched
	MsgSGSyncPending = "3" // data sync with motus.org has been scheduled for a future time
	MsgGPS           = "G" // from SG: GPS fix
	MsgMachineInfo   = "M" // from SG: machine information
	MsgTimeSync      = "C" // from SG: time sync
	MsgDeviceSetting = "S" // from SG: setting for a device
	MsgDevAdded      = "A" // from SG: device was added
	MsgDevRemoved    = "R" // from SG: device was removed
	MsgTag           = "p" // from SG: tag was detected
)

// global message bus
//
// all messages are published on this bus under one of the MsgTopics
var Bus mbus.Mbus

// type representing an SG serial number
type Serno string

// regular expression matching an SG serial number
var SernoRegexp = regexp.MustCompile(SernoRE)

// an SG we have seen recently
type ActiveSG struct {
	Serno      Serno     // serial number; e.g. "SG-1234BBBK9812"
	TsConn     time.Time // time at which connected
	TsLastSync time.Time // time at which last synced with motus
	TsNextSync time.Time // time at which next to be synced with motus
	TunnelPort int       // ssh tunnel port, if applicable
	Connected  bool      // actually connected?  once we've seen a receiver, we keep this struct in memory,
	// but set this field to false when it disconnects
	lock sync.Mutex // lock for any read or write access to fields in this struct
}

// map from serial number to pointer to active SG structure
//
// We use a sync.Map to allow safe multi-threaded access to
// the pointers. Also, we never remove entries from the map, so a
// pointer to an ActiveSG is always valid once created.
var activeSGs sync.Map

// read one line at a time from an io.Reader
type LineReader struct {
	dest   *[]byte   // where a single line is written
	buf    []byte    // buffer for reading
	bufp   int       // position of next character in buf to use
	buflen int       // number of characters left in buffer
	rdr    io.Reader // connection being read from
}

// constructor
func NewLineReader(rdr net.Conn, dest *[]byte) *LineReader {
	r := new(LineReader)
	r.rdr = rdr
	r.dest = dest
	r.buf = make([]byte, len(*dest))
	r.bufp = 0
	r.buflen = 0
	return r
}

// get a line into the dest buffer.  The trailing newline is stripped.
// len(dest) is set to the number of bytes in the string.

func (r *LineReader) getLine() (err error) {
	n := 0
	r.buf = r.buf[:cap(r.buf)]
	var dst = (*r.dest)[:cap(*r.dest)]
	for n < len(dst) {
		for r.bufp >= r.buflen {
			// note: Read(buf) reads at most len(buf), not cap(buf)!
			m, err := r.rdr.Read(r.buf)
			if m == 0 && err != nil {
				return err
			}
			r.bufp = 0
			r.buflen = m
		}
		c := r.buf[r.bufp]
		if c == '\n' {
			r.bufp++
			break
		}
		dst[n] = c
		n++
		r.bufp++
	}
	*r.dest = (*r.dest)[:n]
	return nil
}

// Handle messages from a trusted stream and send them
// the dst channel.  The first line in a trusted stream
// provides the sender, which is used in the SGMsgs generated
// from all subsequent lines.
// FIXME: add context.Context
func handleTrustedStream(conn net.Conn) {
	buff := make([]byte, 4096)
	var addr = conn.RemoteAddr()
	var lr = NewLineReader(conn, &buff)
	_ = lr.getLine()
	var sender string = string(buff)
	for {
		err := lr.getLine()
		if err != nil {
			fmt.Printf("connection from %s@%s closed\n", sender, addr)
			return
		}
		// send a message on the bus; the topic is the first character of the message from the SG
		Bus.Pub(mbus.Msg{mbus.Topic(string(buff[0])), SGMsg{ts: time.Now(), sender: sender, text: string(buff)}})
	}
}

// listen for trusted streams and dispatch them to a handler
func TrustedStreamSource(ctx context.Context, address string) {
	addr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		print("failed to resolve address " + address)
		return
	}
	srv, err := net.ListenTCP("tcp", addr)
	if err != nil {
		print("failed to listen on " + address)
		return
	}
	defer srv.Close()
	for {
		conn, err := srv.AcceptTCP()
		if err != nil {
			// handle error
			print("problem accepting connection")
			return
		}
		go handleTrustedStream(net.Conn(conn))
	}
	select {
	case <-ctx.Done():
	}
}

// Listen for datagrams on either a trusted or untrusted port.
// Datagrams from the trusted port are treated as authenticated.
// Datagrams from an untrusted port have their signature checked
// and are discarded if this is not valid.
// Datagrams are passed to the dst channel as SGMsgs.
func DgramSource(ctx context.Context, address string, trusted bool) {
	pc, err := net.ListenPacket("udp", address)
	if err != nil {
		print("failed to listen on port " + address)
		return
	}
	defer pc.Close()
	doneChan := make(chan error, 1)
	buff := make([]byte, 1024)
	go func() {
		for {
			_, addr, err := pc.ReadFrom(buff)
			if err != nil {
				doneChan <- err
				return
			}
			var prefix = ""
			if trusted {
				prefix = "not "
			}

			fmt.Printf("Got %s from %s %strusted\n", buff, addr, prefix)
		}
	}()
	select {
	case <-ctx.Done():
		fmt.Println("cancelled")
		err = ctx.Err()
	case err = <-doneChan:
	}
}

// Debug: A goroutine to dump Msgs
func messageDump(ctx context.Context) {
	evt := Bus.Sub("*")
	go func() {
	MsgLoop:
		for {
			select {
			case msg, ok := <-evt.Msgs():
				if !ok {
					break MsgLoop
				}
				m := msg.Msg.(SGMsg)
				fmt.Printf("%s: %s,%s,%s\n", msg.Topic, m.ts, m.sender, m.text)
			case <-ctx.Done():
				break
			}
		}
		evt.Unsub("*")
	}()
}

// Goroutine that records (some) messages to a
// table called "messages" in the global DB.
func DBRecorder(ctx context.Context) {
	stmt, err := DB.Prepare("INSERT INTO messages (ts, sender, message) VALUES (?, ?, ?)")
	if err != nil {
		log.Fatal(err)
	}
	// subscribe to topics of interest
	evt := Bus.Sub("*")
	go func() {
		// create closure that uses stmt, db
		defer stmt.Close()
	MsgLoop:
		for {
			select {
			case msg, ok := <-evt.Msgs():
				if !ok {
					break MsgLoop
				}
				m := msg.Msg.(SGMsg)
				ts, sender, text := m.ts, m.sender, m.text
				// fill in defaults
				if ts.IsZero() {
					ts = time.Now()
				}
				if text == "" {
					text = string(msg.Topic)
				}
				// record timestamp in DB as double seconds;
				_, err := stmt.Exec(float64(ts.UnixNano())/1.0E9, sender, text)
				if err != nil {
					log.Fatal(err)
				}
			case <-ctx.Done():
				break
			}
		}
		evt.Unsub("*")
	}()
}

// get the latest sync time for a serial number
// uses global `DB`; returns 0 if no sync has occurred
func SGSyncTime(serno Serno) (lts time.Time) {
	sqlStmt := fmt.Sprintf(`
                   SELECT max(ts) FROM messages WHERE sender = '%s' and substr(message, 1, 1) == '2'`,
		string(serno))
	rows, err := DB.Query(sqlStmt)
	defer rows.Close()
	if err == nil {
		if rows.Next() {
			var ts float64
			rows.Scan(&ts)
			lts = time.Unix(0, int64(ts*1E9))
		}
	}
	return
}

// get the tunnel port for a serial number
// uses global `DB`; returns 0 on error
func TunnelPort(serno Serno) (t int) {
	sqlStmt := fmt.Sprintf(`
                   SELECT tunnelPort FROM receivers WHERE serno='%s'`,
		string(serno))
	rows, err := DB.Query(sqlStmt)
	defer rows.Close()
	if err == nil {
		if rows.Next() {
			rows.Scan(&t)
		}
	}
	return
}

// generate events for SG connection / disconnection
//
// Watch directory `dir` for creation / deletion of files representing
// connected SGs. Files representing SGs are those matching the first
// capture group of `sgRE`.  After establishing a watch goroutine,
// events are generated for any files already in `dir`, using the file
// mtime.  This creates a race condition under which we might generate
// two SGConnect events for the same SG, so subscribers need to account
// for this.
//
// SGEvents are passed to the global message bus Mbus under the topic "SGEvent"

func ConnectionWatcher(ctx context.Context, dir string, sgRE string) {
	re := regexp.MustCompile(sgRE)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()

	err = watcher.Add(dir)
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				parts := re.FindStringSubmatch(event.Name)
				if parts != nil {
					msg := mbus.Msg{mbus.Topic(MsgSGConnect), SGMsg{sender: parts[1], ts: time.Now()}}
					if event.Op&fsnotify.Remove == fsnotify.Remove {
						msg.Topic = mbus.Topic(MsgSGDisconnect)
					}
					Bus.Pub(msg)
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Println("error:", err)
			case <-ctx.Done():
				return
			}
		}
	}()
	// generate "connection" events from existing connection semaphores
	// (these are receivers already connected when this sensorgnomeServer started)
	files, err := ioutil.ReadDir(dir)
	if err == nil {
		for _, finfo := range files {
			parts := re.FindStringSubmatch(finfo.Name())
			if parts != nil {
				Bus.Pub(mbus.Msg{mbus.Topic(MsgSGConnect), SGMsg{sender: parts[1], ts: finfo.ModTime()}})
			}
		}
	}
}

// goroutine to maintain a list of active SGs and their status
//
// Subscribes to all messages but only handles those with a valid
// SGserno as sender.

func SGMinder(ctx context.Context) {
	evt := Bus.Sub("*")
	go func() {
	MsgLoop:
		for {
			select {
			case msg, ok := <-evt.Msgs():
				t, m := MsgTopic(msg.Topic), msg.Msg.(SGMsg)
				if !SernoRegexp.MatchString(m.sender) {
					// not an SG message
					continue MsgLoop
				}
				serno := Serno(m.sender)
				sgp, ok := activeSGs.Load(serno)

				if !ok {
					// not in current list, so populate what is known from DB
					sgp = &ActiveSG{Serno: serno, TsConn: m.ts, TsLastSync: SGSyncTime(serno), TunnelPort: TunnelPort(serno), Connected: true}
					activeSGs.Store(serno, sgp)
				}
				sg := sgp.(*ActiveSG)
				sg.lock.Lock()
				switch t {
				case MsgSGConnect:
					sg.TsConn = m.ts
					sg.Connected = true
				case MsgSGDisconnect:
					sg.Connected = false
				case MsgSGSync:
					sg.TsLastSync = m.ts
				case MsgSGSyncPending:
					sg.TsNextSync = m.ts
				}
				sg.lock.Unlock()
			case <-ctx.Done():
				break
			}
		}
		evt.Unsub("*")
	}()
}

// manage repeated sync jobs for a single SG
// emit a message each time a receiver sync is launched
func SyncWorker(ctx context.Context, serno Serno) {
	// grab receiver info pointer
	sgp, ok := activeSGs.Load(serno)
	if !ok {
		return // should never happen!
	}
	sg := sgp.(*ActiveSG)
	sg.lock.Lock()
	cp := fmt.Sprintf("-oControlPath=%s", MotusControlPath)
	pf := fmt.Sprintf("-R%d:localhost:%d", sg.TunnelPort, sg.TunnelPort)
	tf := fmt.Sprintf(MotusSyncTemplate, sg.TunnelPort, string(serno))
	sg.lock.Unlock()
SyncLoop:
	for {
		// set up a wait uniformly distributed between lo and hi times
		delay := time.Duration(SyncWaitLo+rand.Int31n(SyncWaitHi-SyncWaitLo)) * time.Minute
		wait := time.NewTimer(delay)
		Bus.Pub(mbus.Msg{mbus.Topic(MsgSGSyncPending), SGMsg{sender: string(serno), ts: time.Now().Add(delay)}})
		select {
		case synctime := <-wait.C:
			// if receiver is not still connected, end this goroutine
			sg.lock.Lock()
			quit := !sg.Connected
			sg.lock.Unlock()
			if quit {
				break SyncLoop
			}
			cmd := exec.Command("ssh", "-i", MotusUserKey, "-f", "-N", "-T",
				"-oStrictHostKeyChecking=no", "-oExitOnForwardFailure=yes", "-oControlMaster=auto",
				"-oServerAliveInterval=5", "-oServerAliveCountMax=3",
				cp, pf, MotusUser)
			err := cmd.Run()
			// ignoring error; it is likely just the failure to map an already mapped port
			cmd = exec.Command("ssh", "-i", MotusUserKey, "-oControlMaster=auto",
				cp, MotusUser, "touch", tf)
			err = cmd.Run()
			if err == nil {
				Bus.Pub(mbus.Msg{mbus.Topic(MsgSGSync), SGMsg{ts: synctime, sender: string(serno)}})
			} else {
				fmt.Println(err.Error())
			}

		case <-ctx.Done():
			wait.Stop()
			break SyncLoop
		}
	}
}

// manage motus data sync for SGs
//
// Subscribe to topic "SGEvent" on the global message bus.  Handle these like so:
//
// - `SGConnect`: start a SyncWorker (receiver-specific goroutine) that periodically starts a sync job to send new data
// to sgdata.motus.org.   Multiple `SGConnect` events for the same receiver are collapsed into the
// first one.
// - `SGDisconnect`: stop the asssociated SyncWorker
//

func SyncManager(ctx context.Context) {
	syncCancels := make(map[Serno]context.CancelFunc)
	evt := Bus.Sub(MsgSGConnect, MsgSGDisconnect)
	go func() {
	ConnLoop:
		for {
			select {
			case e, ok := <-evt.Msgs():
				if ok {
					m := e.Msg.(SGMsg)
					serno := Serno(m.sender)
					_, have := syncCancels[serno]
					switch e.Topic {
					case MsgSGConnect:
						if have {
							continue ConnLoop
						}
						newctx, cf := context.WithCancel(ctx)
						syncCancels[serno] = cf
						go SyncWorker(newctx, serno)
					case MsgSGDisconnect:
						if !have {
							continue ConnLoop
						}
						syncCancels[serno]()
						delete(syncCancels, serno)
					}
				}
			case <-ctx.Done():
				break ConnLoop
			}
		}
		// closing, so shut down all sync goroutines
		for serno, sc := range syncCancels {
			sc()
			delete(syncCancels, serno)
		}
	}()
}

// global database pointer
var DB *sql.DB

// open/create the main database
func OpenDB(path string) (db *sql.DB) {
	var err error
	db, err = sql.Open("sqlite3", SGDBFile)
	if err != nil {
		log.Fatal(err)
	}
	stmts := [...]string{
		`CREATE TABLE IF NOT EXISTS messages (
                    ts DOUBLE,
                    sender TEXT,
                    message TEXT
                )`,
		`CREATE INDEX IF NOT EXISTS messages_ts ON messages(ts)`,
		`CREATE INDEX IF NOT EXISTS messages_sender ON messages(sender)`,
		`CREATE INDEX IF NOT EXISTS messages_sender_ts ON messages(sender, ts)`,
		`CREATE INDEX IF NOT EXISTS messages_sender_type_ts ON messages(sender, substr(message, 1, 1), ts)`,
		`CREATE TABLE IF NOT EXISTS receivers (
                 serno        TEXT UNIQUE PRIMARY KEY, -- only one entry per receiver
                 creationdate REAL,                    -- timestamp when this entry was created
                 tunnelport   INTEGER UNIQUE,          -- port used on server for reverse tunnel back to sensorgnome
                 pubkey       TEXT,                    -- unique public/private key pair used by sensorgnome to login to server
                 privkey      TEXT,
                 verified     INTEGER DEFAULT 0        -- has this SG been verified to belong to a real user?
                 )`,
		`CREATE INDEX IF NOT EXISTS receivers_tunnelport ON receivers(tunnelport)`,
		`CREATE TABLE IF NOT EXISTS deleted_receivers (
                 ts           REAL,                    -- deletion timestamp
                 serno        TEXT,                    -- possibly multiple entries per receiver
                 creationdate REAL,                    -- timestamp when this entry was created
                 tunnelport   INTEGER,                 -- port used on server for reverse tunnel back to sensorgnome
                 pubkey       TEXT,                    -- unique public/private key pair used by sensorgnome to login to server
                 privkey      TEXT,
                 verified     INTEGER DEFAULT 0        -- non-zero when verified
                 )`,
		`CREATE INDEX IF NOT EXISTS deleted_receivers_tunnelport ON deleted_receivers(tunnelport)`,
		`PRAGMA busy_timeout = 60000`} // set a very generous 1-minute timeout for busy wait

	for _, s := range stmts {
		_, err = db.Exec(s)
		if err != nil {
			log.Printf("error: %s\n", s)
			log.Fatal(err)
		}
	}
	return
}

const (
	CMD_WHO = iota
	CMD_PORT
	CMD_SERNO
	CMD_JSON
	CMD_QUIT
)

// Handle status requests.
// The request is a one line format, such as "json\n".
// The reply is a summary of active receiver status in that format.
// Posssible formats:
// - `json`: full summary of active receiver status; an object indexed
//   by serial numbers
// - `port`: list of tunnelPorts of connected receivers, one per line
// - `serno`: list of serial numbers of connected receivers, one per line

func handleStatusConn(conn net.Conn) {
	buff := make([]byte, 4096)
	var lr = NewLineReader(conn, &buff)
	cmds := map[string]int8{
		"who":    CMD_WHO,
		"port":   CMD_PORT,
		"ports":  CMD_PORT,
		"serno":  CMD_SERNO,
		"sernos": CMD_SERNO,
		"status": CMD_JSON,
		"json":   CMD_JSON,
		"quit":   CMD_QUIT}
ConnLoop:
	for {
		err := lr.getLine()
		if err != nil {
			break ConnLoop
		}
		var b string
		cmd, ok := cmds[string(buff)]
		if !ok {
			b = "Error: command must be one of: "
			for c, _ := range cmds {
				b += "\n" + c
			}

		} else {
			switch cmd {
			case CMD_QUIT:
				break ConnLoop
			case CMD_JSON:
				bb := make([]byte, 0, 1000)
				bb = append(bb, '{')
				activeSGs.Range(func(serno interface{}, sgp interface{}) bool {
					sg := sgp.(*ActiveSG)
					sg.lock.Lock()
					js, err := json.Marshal(sg)
					sg.lock.Unlock()
					if err == nil {
						if len(bb) > 1 {
							bb = append(bb, ',')
						}
						bb = append(bb, "\""+string(serno.(Serno))+"\":"...)
						bb = append(bb, js...)
					}
					return true
				})
				bb = append(bb, '}')
				b = string(bb)
			case CMD_WHO, CMD_PORT, CMD_SERNO:
				activeSGs.Range(func(serno interface{}, sgp interface{}) bool {
					sg := sgp.(*ActiveSG)
					sg.lock.Lock()
					if sg.Connected {
						if cmd == CMD_SERNO || cmd == CMD_WHO {
							b += string(sg.Serno)
						}
						if cmd == CMD_WHO {
							b += ","
						}
						if cmd == CMD_PORT || cmd == CMD_WHO {
							b += strconv.Itoa(sg.TunnelPort)
						}
						b += "\n"
					}
					sg.lock.Unlock()
					return true
				})
			}
		}
		_, err = io.WriteString(conn, b)
		if err != nil {
			break ConnLoop
		}
	}
	conn.Close()
}

// listen for trusted streams and dispatch them to a handler
func StatusServer(ctx context.Context, address string) {
	addr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		print("failed to resolve address " + address)
		return
	}
	srv, err := net.ListenTCP("tcp", addr)
	if err != nil {
		print("failed to listen on " + address)
		return
	}
	defer srv.Close()
	for {
		conn, err := srv.AcceptTCP()
		if err != nil {
			// handle error
			print("problem accepting connection")
			return
		}
		go handleStatusConn(net.Conn(conn))
	}
	select {
	case <-ctx.Done():
	}
}

// func StatusPageGenerator(ctx context.Context, path string, regen <-chan bool) {
// 	MotusRefreshMetadata()
// }

func main() {
	rand.Seed(time.Now().UnixNano())
	Bus = mbus.NewMbus()
	var ctx, _ = context.WithCancel(context.Background())
	DB = OpenDB(SGDBFile)
	DBRecorder(ctx)
	SGMinder(ctx)
	// messageDump(ctx)
	SyncManager(ctx)
	// go StatusPageGenerator(ctx, StatusPagePath)
	// launch goroutines which are message generators
	// (subscribers are launched above)
	ConnectionWatcher(ctx, ConnectionSemPath, ConnectionSemRE)
	go StatusServer(ctx, "localhost:59055")
	go TrustedStreamSource(ctx, "localhost:59054")
	go DgramSource(ctx, ":59052", false)
	go DgramSource(ctx, ":59053", true)

	// wait until cancelled (nothing does this, though)
	<-ctx.Done()
}
