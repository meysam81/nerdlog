package core

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juju/errors"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/dimonomid/nerdlog/log"
)

const connectionTimeout = 5 * time.Second

// Setting useGzip to false is just a simple way to disable gzip, for debugging
// purposes or w/e, since it's still experimental. Maybe we need to add a flag
// for it, we'll see.
const useGzip = true

const (
	// gzipStartMarker and gzipEndMarker are echoed in the beginning and the end
	// of the gzipped output. Effectively we're doing this:
	//
	//   $ echo gzip_start ; whatever command we need to run | gzip ; echo gzip_end
	//
	// and the scanner func (returned by getScannerFunc) sees those markers and
	// buffers gzipped output until it's done, then gunzips it and sends to the
	// clients, so it's totally opaque for them.
	gzipStartMarker = "gzip_start"
	gzipEndMarker   = "gzip_end"
)

// queryLogsArgsTimeLayout is used to format the --from and --to arguments for
// nerdlog_agent.sh.
//
// TODO: make it dynamic; e.g. generating that day string like "02" requires
// some extra logic in the agent script for the traditional syslog format
// (which has it space-padded, not zero-padded).
const queryLogsArgsTimeLayout = "2006-01-02-15:04"

//go:embed nerdlog_agent.sh
var nerdlogAgentSh string

var syslogRegex = regexp.MustCompile(`^(\S+)\s+(\S+?)(?:\[(\d+)\])?:\s+(.*)`)

type LStreamClient struct {
	params LStreamClientParams

	connectResCh chan lstreamConnRes
	enqueueCmdCh chan lstreamCmd

	// timezone is a string received from the logstream
	timezone string
	// location is loaded based on the timezone. If failed, it'll be UTC.
	location *time.Location

	// exampleLogLines are the lines that we received during logstream bootstrap.
	// We'll try to do log format autodetection based on that.
	exampleLogLines []string
	timeFormat      *TimeFormatDescr

	numConnAttempts int

	state     LStreamClientState
	busyStage BusyStage

	conn *connCtx

	cmdQueue   []lstreamCmd
	curCmdCtx  *lstreamCmdCtx
	nextCmdIdx int

	// disconnectReqCh is sent to when Close is called.
	disconnectReqCh chan disconnectReq
	tearingDown     bool
	// disconnectedBeforeTeardownCh is closed once tearingDown is true and we're
	// fully disconnected.
	disconnectedBeforeTeardownCh chan struct{}

	//debugFile *os.File
}

// disconnectReq represents a request to abort whatever connection we have,
// and either teardown or reconnect again.
type disconnectReq struct {
	// If teardown is true, it means the LStreamClient should completely stop. Otherwise,
	// after disconnecting, it will reconnect.
	teardown bool

	// If changeName is non-empty, the LStreamClient's Name will be updated;
	// it's useful for teardowns, to distinguish from potentially-existing
	// another LStreamClient with the same (old) name.
	changeName string
}

type connCtx struct {
	sshClient  *ssh.Client
	sshSession *ssh.Session
	stdinBuf   io.WriteCloser

	stdoutLinesCh chan string
	stderrLinesCh chan string
}

type BusyStage struct {
	// Num is just a stage number. Its meaning depends on the kind of command the
	// host is executing, but a general rule is that this number starts from 1
	// and then increases, as the process goes on, so we can compare different
	// nodes and e.g. find the slowest one.
	Num int

	// Title is just a human-readable description of the stage.
	Title string

	// Percentage is a percentage of the current stage.
	Percentage int
}

type ConnDetails struct {
	// Err is an error message from the last connection attempt.
	Err string
}

type BootstrapDetails struct {
	// Err is an error message from the last bootstrap attempt.
	Err string
}

func (c *connCtx) getStdoutLinesCh() chan string {
	if c == nil {
		return nil
	}

	return c.stdoutLinesCh
}

func (c *connCtx) getStderrLinesCh() chan string {
	if c == nil {
		return nil
	}

	return c.stderrLinesCh
}

// LStreamClientUpdate represents an update from logstream client. Name is always
// populated and it's the logstream's name, and from all the other fields, exactly
// one field must be non-nil.
type LStreamClientUpdate struct {
	Name string

	State *LStreamClientUpdateState

	ConnDetails      *ConnDetails
	BootstrapDetails *BootstrapDetails
	BusyStage        *BusyStage

	// If TornDown is true, it means it's the last update from that client.
	TornDown bool
}

type LStreamClientUpdateState struct {
	OldState LStreamClientState
	NewState LStreamClientState
}

type LStreamClientParams struct {
	LogStream LogStream

	Logger *log.Logger

	// ClientID is just an arbitrary string (should be filename-friendly though)
	// which will be appended to the nerdlog_agent.sh and its index filenames.
	//
	// Needed to make sure that different clients won't get conflicts over those
	// files when using the tool concurrently on the same nodes.
	ClientID string

	UpdatesCh chan<- *LStreamClientUpdate
}

func NewLStreamClient(params LStreamClientParams) *LStreamClient {
	params.Logger = params.Logger.WithNamespaceAppended(
		fmt.Sprintf("LSClient_%s", params.LogStream.Name),
	)

	lsc := &LStreamClient{
		params: params,

		timezone: "UTC",
		location: time.UTC,

		state:        LStreamClientStateDisconnected,
		enqueueCmdCh: make(chan lstreamCmd, 32),

		disconnectReqCh:              make(chan disconnectReq, 1),
		disconnectedBeforeTeardownCh: make(chan struct{}),
	}

	//debugFile, _ := os.Create("/tmp/lsclient_debug.log")
	//lsc.debugFile = debugFile

	lsc.changeState(LStreamClientStateConnecting)

	go lsc.run()

	return lsc
}

func (lsc *LStreamClient) SendFoo() {
}

type LStreamClientState string

const (
	LStreamClientStateDisconnected  LStreamClientState = "disconnected"
	LStreamClientStateConnecting    LStreamClientState = "connecting"
	LStreamClientStateDisconnecting LStreamClientState = "disconnecting"
	LStreamClientStateConnectedIdle LStreamClientState = "connected_idle"
	LStreamClientStateConnectedBusy LStreamClientState = "connected_busy"
)

func isStateConnected(state LStreamClientState) bool {
	return state == LStreamClientStateConnectedIdle || state == LStreamClientStateConnectedBusy
}

func (lsc *LStreamClient) changeState(newState LStreamClientState) {
	oldState := lsc.state

	// Properly leave old state

	if isStateConnected(oldState) && !isStateConnected(newState) {
		// Initiate disconnect
		lsc.conn.stdinBuf.Close()
		lsc.conn.sshSession.Close()
		lsc.conn.sshClient.Close()
	}

	switch oldState {
	case LStreamClientStateConnecting:
		lsc.connectResCh = nil
	case LStreamClientStateConnectedBusy:
		lsc.curCmdCtx = nil
		lsc.busyStage = BusyStage{}
	}

	// Enter new state

	lsc.state = newState
	lsc.sendUpdate(&LStreamClientUpdate{
		State: &LStreamClientUpdateState{
			OldState: oldState,
			NewState: newState,
		},
	})

	switch lsc.state {
	case LStreamClientStateConnecting:
		lsc.numConnAttempts++
		lsc.connectResCh = make(chan lstreamConnRes, 1)
		go connectToLogStream(lsc.params.Logger, lsc.params.LogStream, lsc.connectResCh)

	case LStreamClientStateConnectedIdle:
		if len(lsc.cmdQueue) > 0 {
			nextCmd := lsc.cmdQueue[0]
			lsc.cmdQueue = lsc.cmdQueue[1:]

			lsc.startCmd(nextCmd)
		}

	case LStreamClientStateDisconnected:
		lsc.conn = nil
	}
}

func (lsc *LStreamClient) sendBusyStageUpdate() {
	upd := lsc.busyStage
	lsc.sendUpdate(&LStreamClientUpdate{
		BusyStage: &upd,
	})
}

func (lsc *LStreamClient) sendCmdResp(resp interface{}, err error) {
	if lsc.curCmdCtx == nil {
		return
	}

	if lsc.curCmdCtx.cmd.respCh == nil {
		return
	}

	lsc.curCmdCtx.cmd.respCh <- lstreamCmdRes{
		hostname: lsc.params.LogStream.Name,
		resp:     resp,
		err:      err,
	}
}

func (lsc *LStreamClient) run() {
	ticker := time.NewTicker(1 * time.Second)
	var connectAfter time.Time
	var lastUpdTime time.Time

	for {
		select {
		case res := <-lsc.connectResCh:
			if res.err != nil {
				lsc.sendUpdate(&LStreamClientUpdate{
					ConnDetails: &ConnDetails{
						Err: fmt.Sprintf("attempt %d: %s", lsc.numConnAttempts, res.err.Error()),
					},
				})

				lsc.changeState(LStreamClientStateDisconnected)
				if lsc.tearingDown {
					close(lsc.disconnectedBeforeTeardownCh)
					continue
				}

				connectAfter = time.Now().Add(2 * time.Second)
				continue
			}

			lsc.numConnAttempts = 0

			lastUpdTime = time.Now()

			lsc.conn = res.conn
			lsc.changeState(LStreamClientStateConnectedIdle)

			// Send bootstrap command
			lsc.startCmd(lstreamCmd{
				bootstrap: &lstreamCmdBootstrap{},
			})

		case cmd := <-lsc.enqueueCmdCh:
			// Require a connection.
			if !isStateConnected(lsc.state) {
				lsc.sendCmdResp(nil, errors.Errorf("not connected"))
				continue
			}

			// And then, depending on whether we're busy or idle, either act
			// right away, or enqueue for later.
			if lsc.state == LStreamClientStateConnectedIdle {
				lsc.startCmd(cmd)
			} else {
				lsc.addCmdToQueue(cmd)
			}

		case line, ok := <-lsc.conn.getStdoutLinesCh():
			if !ok {
				// Stdout was just closed
				lsc.params.Logger.Verbose3f("Stdout was closed (%s)", lsc.params.LogStream.Name)

				lsc.conn.stdoutLinesCh = nil
				lsc.checkIfDisconnected()
				continue
			}

			lsc.params.Logger.Verbose3f("Got stdout line(%s): %s", lsc.params.LogStream.Name, line)

			lastUpdTime = time.Now()

			switch lsc.state {
			case LStreamClientStateConnectedBusy:
				cmdCtx := lsc.curCmdCtx

				if cmdCtx == nil {
					// We received some line before printing any command, must be
					// just standard welcome message, but we're not interested in that.
					continue
				}

				if lsc.checkCommandDone(line, cmdCtx, false) {
					continue
				}

				if lsc.checkResetOutput(line, cmdCtx, false) {
					continue
				}

				if lsc.checkError(line, cmdCtx) {
					continue
				}

				if lsc.checkDebug(line, cmdCtx) {
					continue
				}

				if lsc.checkExitCode(line, cmdCtx) {
					continue
				}

				switch {
				case cmdCtx.cmd.bootstrap != nil:
					tzPrefix := "host_timezone:"
					logLinePrefix := "example_log_line:"

					if strings.HasPrefix(line, tzPrefix) {
						tz := strings.TrimPrefix(line, tzPrefix)
						lsc.params.Logger.Verbose1f("Got logstream timezone: %s\n", tz)

						location, err := time.LoadLocation(tz)
						if err != nil {
							lsc.params.Logger.Errorf("Error: failed to load location %s, will use UTC\n", tz)
							// TODO: send an update and then the receiver should show a message
							// to the user
						} else {
							lsc.timezone = tz
							lsc.location = location
						}
					} else if strings.HasPrefix(line, logLinePrefix) {
						exampleLogLine := strings.TrimPrefix(line, logLinePrefix)
						lsc.params.Logger.Verbose1f("Got example log line: %s\n", exampleLogLine)

						lsc.exampleLogLines = append(lsc.exampleLogLines, exampleLogLine)
					} else if line == "bootstrap ok" {
						cmdCtx.bootstrapCtx.receivedSuccess = true
					} else if line == "bootstrap failed" {
						cmdCtx.bootstrapCtx.receivedFailure = true
					} else {
						cmdCtx.unhandledStdout = append(cmdCtx.unhandledStdout, line)
					}

				case cmdCtx.cmd.ping != nil:
					// Nothing special to do
					cmdCtx.unhandledStdout = append(cmdCtx.unhandledStdout, line)

				case cmdCtx.cmd.queryLogs != nil:
					respCtx := cmdCtx.queryLogsCtx
					resp := respCtx.Resp

					switch {
					case strings.HasPrefix(line, "s:"):
						parts := strings.Split(strings.TrimPrefix(line, "s:"), ",")
						if len(parts) < 2 {
							err := errors.Errorf("malformed mstats %q: expected at least 2 parts", line)
							cmdCtx.errs = append(cmdCtx.errs, err)
							continue
						}

						t, err := time.ParseInLocation(lsc.timeFormat.MinuteKeyLayout, parts[0], lsc.location)
						if err != nil {
							cmdCtx.errs = append(cmdCtx.errs, errors.Annotatef(err, "parsing mstats"))
							continue
						}

						t = InferYear(t)
						t = t.UTC()

						n, err := strconv.Atoi(parts[1])
						if err != nil {
							cmdCtx.errs = append(cmdCtx.errs, errors.Annotatef(err, "parsing mstats"))
							continue
						}

						resp.MinuteStats[t.Unix()] = MinuteStatsItem{
							NumMsgs: n,
						}

					case strings.HasPrefix(line, "logfile:"):
						msg := strings.TrimPrefix(line, "logfile:")
						idx := strings.IndexRune(msg, ':')
						if idx <= 0 {
							cmdCtx.errs = append(cmdCtx.errs, errors.Errorf("parsing logfile msg: no number of lines %q", line))
							continue
						}

						logFilename := msg[:idx]
						logNumberOfLinesStr := msg[idx+1:]
						logNumberOfLines, err := strconv.Atoi(logNumberOfLinesStr)
						if err != nil {
							cmdCtx.errs = append(cmdCtx.errs, errors.Annotatef(err, "parsing logfile msg: invalid number in %q", line))
							continue
						}

						respCtx.logfiles = append(respCtx.logfiles, logfileWithStartingLinenumber{
							filename:       logFilename,
							fromLinenumber: logNumberOfLines,
						})

					case strings.HasPrefix(line, "m:"):
						// msg:Mar 26 17:08:34 localhost myapp[21134]: Mar 26 17:08:34.476329 foo bar foo bar
						msg := strings.TrimPrefix(line, "m:")
						idx := strings.IndexRune(msg, ':')
						if idx <= 0 {
							cmdCtx.errs = append(cmdCtx.errs, errors.Errorf("parsing log msg: no line number in %q", line))
							continue
						}

						logLinenoStr := msg[:idx]
						msg = msg[idx+1:]

						logLinenoCombined, err := strconv.Atoi(logLinenoStr)
						if err != nil {
							cmdCtx.errs = append(cmdCtx.errs, errors.Annotatef(err, "parsing log msg: invalid line number in %q", line))
							continue
						}

						var logFilename string
						logLineno := logLinenoCombined

						for i := len(respCtx.logfiles) - 1; i >= 0; i-- {
							logfile := respCtx.logfiles[i]
							if logLineno > logfile.fromLinenumber {
								logLineno -= logfile.fromLinenumber
								logFilename = logfile.filename
								break
							}
						}

						// Put together a basic LogMsg, for now with the raw message and
						// without even the Time parsed, and then give it to parseLine,
						// which will encirch it.
						logMsg := LogMsg{
							// Time will be set later

							LogFilename:   logFilename,
							LogLinenumber: logLineno,

							CombinedLinenumber: logLinenoCombined,

							Msg: msg,
							Context: map[string]string{
								"lstream": lsc.params.LogStream.Name,
							},

							OrigLine: msg,
						}

						err = lsc.parseLine(&logMsg)
						if err != nil {
							cmdCtx.errs = append(cmdCtx.errs, errors.Annotatef(err, "parsing log msg %q", line))
							continue
						}

						if logMsg.Time.Before(respCtx.lastTime) {
							// Time has decreased: this might happen if the previous log line
							// had a precise timestamp with microseconds (coming from the app
							// level), but the current line only has a second precision
							// (e.g. coming from rsyslog level). Then we just hackishly set the
							// current timestamp to be the same.
							logMsg.Time = respCtx.lastTime
							logMsg.DecreasedTimestamp = true
						}

						resp.Logs = append(resp.Logs, logMsg)

						respCtx.lastTime = logMsg.Time

						// NOTE: the "p:" lines (process-related) are in stderr and thus
						// are handled below. Why they are in stderr, see comments there.
					default:
						cmdCtx.unhandledStdout = append(cmdCtx.unhandledStdout, line)
					}

				default:
					panic("invalid cmdCtx.cmd: no subcontext")
				}
			}

		case line, ok := <-lsc.conn.getStderrLinesCh():
			if !ok {
				// Stderr was just closed
				lsc.params.Logger.Verbose3f("Stderr was closed (%s)", lsc.params.LogStream.Name)

				lsc.conn.stderrLinesCh = nil
				lsc.checkIfDisconnected()
				continue
			}

			lsc.params.Logger.Verbose3f("Got stderr line(%s): %s", lsc.params.LogStream.Name, line)

			lastUpdTime = time.Now()

			// NOTE: the "p:" lines (process-related) are here in stderr, because
			// stdout is gzipped and thus we don't have any partial results (we get
			// them all at once), but for the process info, we actually want it right
			// when it's printed by the nerdlog_agent.sh.
			switch lsc.state {
			case LStreamClientStateConnectedBusy:
				cmdCtx := lsc.curCmdCtx

				if cmdCtx == nil {
					// We received some line before printing any command, just ignore that.
					continue
				}

				if lsc.checkCommandDone(line, cmdCtx, true) {
					continue
				}

				if lsc.checkResetOutput(line, cmdCtx, true) {
					continue
				}

				if lsc.checkError(line, cmdCtx) {
					continue
				}

				if lsc.checkDebug(line, cmdCtx) {
					continue
				}

				switch {
				case cmdCtx.cmd.bootstrap != nil:
					cmdCtx.unhandledStderr = append(cmdCtx.unhandledStderr, line)
				case cmdCtx.cmd.ping != nil:
					cmdCtx.unhandledStderr = append(cmdCtx.unhandledStderr, line)
				case cmdCtx.cmd.queryLogs != nil:
					switch {
					case strings.HasPrefix(line, "p:"):
						// "p:" means process
						processLine := strings.TrimPrefix(line, "p:")

						switch {
						case strings.HasPrefix(processLine, "stage:"):
							stageLine := strings.TrimPrefix(processLine, "stage:")
							parts := strings.Split(stageLine, ":")
							if len(parts) < 2 {
								cmdCtx.errs = append(cmdCtx.errs, errors.Errorf("received malformed p:stage line: %s (expected at least 2 parts, got %d)", line, len(parts)))
								continue
							}

							num, err := strconv.Atoi(parts[0])
							if err != nil {
								cmdCtx.errs = append(cmdCtx.errs, errors.Annotatef(err, "received malformed p:stage line: %s", line))
								continue
							}

							lsc.busyStage = BusyStage{
								Num:   num,
								Title: parts[1],
							}
							lsc.sendBusyStageUpdate()

						case strings.HasPrefix(processLine, "p:"):
							// second "p:" means percentage

							percentage, err := strconv.Atoi(strings.TrimPrefix(processLine, "p:"))
							if err != nil {
								cmdCtx.errs = append(cmdCtx.errs, errors.Annotatef(err, "received malformed p:p line:", line))
								continue
							}

							lsc.busyStage.Percentage = percentage
							lsc.sendBusyStageUpdate()
						default:
							cmdCtx.unhandledStderr = append(cmdCtx.unhandledStderr, line)
						}
					default:
						cmdCtx.unhandledStderr = append(cmdCtx.unhandledStderr, line)
					}

				default:
					panic("invalid cmdCtx.cmd: no subcontext")
				}
			}

			//case data := <-lsc.stdinCh:
			//lsc.stdinBuf.Write([]byte(data))
			//if len(data) > 0 && data[len(data)-1] != '\n' {
			//lsc.stdinBuf.Write([]byte("\n"))
			//}

		case <-ticker.C:
			if lsc.state == LStreamClientStateConnectedIdle && time.Since(lastUpdTime) > 40*time.Second {
				lsc.startCmd(lstreamCmd{
					ping: &lstreamCmdPing{},
				})
			} else if !connectAfter.IsZero() {
				connectAfter = time.Time{}
				lsc.changeState(LStreamClientStateConnecting)
			}

		case req := <-lsc.disconnectReqCh:
			lsc.params.Logger.Infof("Received disconnect message (teardown:%v)", req.teardown)

			if req.teardown {
				lsc.tearingDown = true
			}

			if req.changeName != "" {
				lsc.params.LogStream.Name = req.changeName
			}

			// If we're already disconnected, consider ourselves torn-down already.
			// Otherwise, initiate disconnection.
			if lsc.state == LStreamClientStateDisconnected {
				if req.teardown {
					close(lsc.disconnectedBeforeTeardownCh)
				}
			} else {
				lsc.changeState(LStreamClientStateDisconnecting)
			}

		case <-lsc.disconnectedBeforeTeardownCh:
			lsc.params.Logger.Infof("Teardown completed")
			lsc.sendUpdate(&LStreamClientUpdate{
				TornDown: true,
			})
			return
		}
	}
}

func (lsc *LStreamClient) sendUpdate(upd *LStreamClientUpdate) {
	upd.Name = lsc.params.LogStream.Name
	lsc.params.UpdatesCh <- upd
}

type lstreamConnRes struct {
	conn *connCtx
	err  error
}

func connectToLogStream(
	logger *log.Logger,
	logStream LogStream,
	resCh chan<- lstreamConnRes,
) (res lstreamConnRes) {
	defer func() {
		if res.err != nil {
			logger.Errorf("Connection failed: %s", res.err)
		}

		resCh <- res
	}()

	var sshClient *ssh.Client

	conf := getClientConfig(logger, logStream.Host.User)

	if logStream.Jumphost != nil {
		logger.Infof("Connecting via jumphost")
		// Use jumphost
		jumphost, err := getJumphostClient(logger, logStream.Jumphost)
		if err != nil {
			logger.Errorf("Jumphost connection failed: %s", err)
			res.err = errors.Annotatef(err, "getting jumphost client")
			return res
		}

		conn, err := dialWithTimeout(jumphost, "tcp", logStream.Host.Addr, connectionTimeout)
		if err != nil {
			res.err = errors.Trace(err)
			return res
		}

		authConn, chans, reqs, err := ssh.NewClientConn(conn, logStream.Host.Addr, conf)
		if err != nil {
			res.err = errors.Trace(err)
			return res
		}

		sshClient = ssh.NewClient(authConn, chans, reqs)
	} else {
		logger.Infof("Connecting to %s (%+v)", logStream.Host.Addr, conf)
		var err error
		sshClient, err = ssh.Dial("tcp", logStream.Host.Addr, conf)
		if err != nil {
			res.err = errors.Trace(err)
			return res
		}
	}
	//defer client.Close()

	logger.Infof("Connected to %s", logStream.Host.Addr)

	sshSession, err := sshClient.NewSession()
	if err != nil {
		res.err = errors.Trace(err)
		return res
	}

	//defer sess.Close()

	stdinBuf, err := sshSession.StdinPipe()
	if err != nil {
		res.err = errors.Trace(err)
		return res
	}

	stdoutBuf, err := sshSession.StdoutPipe()
	if err != nil {
		res.err = errors.Trace(err)
		return res
	}

	stderrBuf, err := sshSession.StderrPipe()
	if err != nil {
		res.err = errors.Trace(err)
		return res
	}

	err = sshSession.Shell()
	if err != nil {
		res.err = errors.Trace(err)
		return res
	}

	stdoutLinesCh := make(chan string, 32)
	stderrLinesCh := make(chan string, 32)

	go getScannerFunc("stdout", stdoutBuf, stdoutLinesCh)()
	go getScannerFunc("stderr", stderrBuf, stderrLinesCh)()

	res.conn = &connCtx{
		sshClient:  sshClient,
		sshSession: sshSession,
		stdinBuf:   stdinBuf,

		stdoutLinesCh: stdoutLinesCh,
		stderrLinesCh: stderrLinesCh,
	}

	return res
}

func getClientConfig(logger *log.Logger, username string) *ssh.ClientConfig {
	auth, err := getSSHAgentAuth(logger)
	if err != nil {
		panic(err.Error())
	}

	return &ssh.ClientConfig{
		User: username,
		Auth: []ssh.AuthMethod{auth},

		// TODO: fix it
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),

		Timeout: connectionTimeout,
	}
}

var (
	jumphostsShared    = map[string]*ssh.Client{}
	jumphostsSharedMtx sync.Mutex
)

func getJumphostClient(logger *log.Logger, jhConfig *ConfigHost) (*ssh.Client, error) {
	jumphostsSharedMtx.Lock()
	defer jumphostsSharedMtx.Unlock()

	key := jhConfig.Key()
	jh := jumphostsShared[key]
	if jh == nil {
		logger.Infof("Connecting to jumphost... %+v", jhConfig)

		parts := strings.Split(jhConfig.Addr, ":")
		if len(parts) != 2 {
			return nil, errors.Errorf("malformed jumphost address %q", jhConfig.Addr)
		}

		addrs, err := net.LookupHost(parts[0])
		if err != nil {
			return nil, errors.Trace(err)
		}

		if len(addrs) != 1 {
			return nil, errors.New("Address not found")
		}

		conf := getClientConfig(logger, jhConfig.User)

		jh, err = ssh.Dial("tcp", jhConfig.Addr, conf)
		if err != nil {
			return nil, errors.Trace(err)
		}

		jumphostsShared[key] = jh

		logger.Infof("Jumphost ok")
	}

	return jh, nil
}

var (
	sshAuthMethodShared    ssh.AuthMethod
	sshAuthMethodSharedMtx sync.Mutex
)

func getSSHAgentAuth(logger *log.Logger) (ssh.AuthMethod, error) {
	sshAuthMethodSharedMtx.Lock()
	defer sshAuthMethodSharedMtx.Unlock()

	if sshAuthMethodShared == nil {
		logger.Infof("Initializing sshAuthMethodShared...")
		sshAgent, err := net.Dial("unix", os.Getenv("SSH_AUTH_SOCK"))
		if err != nil {
			logger.Infof("Failed to initialize sshAuthMethodShared: %s", err.Error())
			return nil, errors.Trace(err)
		}

		sshAuthMethodShared = ssh.PublicKeysCallback(agent.NewClient(sshAgent).Signers)
	}

	return sshAuthMethodShared, nil
}

// dialWithTimeout is a hack needed to get a timeout for the ssh client.
// https://stackoverflow.com/questions/31554196/ssh-connection-timeout
//
// It's possible we could accomplish the same thing by using NewClient() with Conn.SetDeadline(), but that requires
// some refactoring.
func dialWithTimeout(client *ssh.Client, protocol, hostAddr string, timeout time.Duration) (net.Conn, error) {
	finishedChan := make(chan net.Conn)
	errChan := make(chan error)
	go func() {
		conn, err := client.Dial(protocol, hostAddr)
		if err != nil {
			errChan <- err
			return
		}
		finishedChan <- conn
	}()

	select {
	case conn := <-finishedChan:
		return conn, nil

	case err := <-errChan:
		return nil, errors.Trace(err)

	case <-time.After(connectionTimeout):
		// Don't close the connection here since it's reused
		return nil, errors.New("ssh client dial timed out")
	}
}

// scanLinesPreserveCarriageReturn is the same as bufio.ScanLines, but it does
// not strip the \r characters: it's just a hack to support gzipping. In fact,
// since we sometimes read text lines and sometimes gzipped data, we'd better
// use some other custom scanner, but for now just this simple hack.
func scanLinesPreserveCarriageReturn(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		// We have a full newline-terminated line.
		return i + 1, data[0:i], nil
	}
	// If we're at EOF, we have a final, non-terminated line. Return it.
	if atEOF {
		return len(data), data, nil
	}
	// Request more data.
	return 0, nil, nil
}

func getScannerFunc(name string, reader io.Reader, linesCh chan<- string) func() {
	return func() {
		defer func() {
			close(linesCh)
		}()

		scanner := bufio.NewScanner(reader)

		// See comments for scanLinesPreserveCarriageReturn for details why we need
		// this custom split function.
		scanner.Split(scanLinesPreserveCarriageReturn)

		// TODO: also defer signal to reconnect

		// inGzip is true when we're receiving gzipped data.
		// gzipBuf accumulates that data, and once we receive the gzipEndMarker,
		// we gunzip all this data and feed the lines to the channel.
		//
		// TODO: instead of accumulating it and then unpacking all at once, do it
		// gradually as we receive data. Idk how much of an improvement it'd be in
		// practice though, since we're not receiving some huge chunks of data,
		// just a bit nicer.
		inGzip := false
		var gzipBuf bytes.Buffer

		for scanner.Scan() {
			lineBytes := scanner.Bytes()
			line := string(lineBytes)

			if !inGzip && line == gzipStartMarker {
				// Gzipped data begins
				inGzip = true
				gzipBuf.Reset()

				// We also need to continue loop iteration now so that we don't
				// add this gzipStartMarker line to the gzipBuf below.
				continue
			} else if inGzip && strings.HasSuffix(line, gzipEndMarker) {
				// We just reached the end of the gzipped data
				inGzip = false

				// Append this last piece
				gzipBuf.Write(lineBytes[:len(lineBytes)-len(gzipEndMarker)])

				var err error

				// Gunzip the data and feed all the lines to linesCh
				var r io.Reader
				r, err = gzip.NewReader(&gzipBuf)
				if err != nil {
					linesCh <- fmt.Sprintf("error:failed to gunzip data: %s", err.Error())
					return
				}

				scanner := bufio.NewScanner(r)
				for scanner.Scan() {
					linesCh <- scanner.Text()
				}

				continue
			}

			if !inGzip {
				// We're not in gzipped data, so just feed this line directly.
				linesCh <- line
			} else {
				// We're reading gzipped data now, so for now just add it to the
				// gzipBuf (together with the \n which was stripped by the scanner).
				gzipBuf.Write(lineBytes)
				gzipBuf.WriteByte('\n')
			}
		}

		if err := scanner.Err(); err != nil {
			return
		}
	}
}

func (lsc *LStreamClient) EnqueueCmd(cmd lstreamCmd) {
	lsc.enqueueCmdCh <- cmd
}

// Close initiates the shutdown. It doesn't wait for the shutdown to complete;
// client code needs to wait for the corresponding event (with TornDown: true).
//
// If changeName is non-empty, the LStreamClient's Name will be updated; it's
// useful to distinguish this LStreamClient from potentially-existing another one
// with the same (old) name.
func (lsc *LStreamClient) Close(changeName string) {
	select {
	case lsc.disconnectReqCh <- disconnectReq{
		teardown:   true,
		changeName: changeName,
	}:
	default:
	}
}

func (lsc *LStreamClient) Reconnect() {
	select {
	case lsc.disconnectReqCh <- disconnectReq{
		teardown: false,
	}:
	default:
	}
}

func (lsc *LStreamClient) addCmdToQueue(cmd lstreamCmd) {
	lsc.cmdQueue = append(lsc.cmdQueue, cmd)
}

func (lsc *LStreamClient) startCmd(cmd lstreamCmd) {
	cmdCtx := &lstreamCmdCtx{
		cmd: cmd,
		idx: lsc.nextCmdIdx,
	}

	lsc.curCmdCtx = cmdCtx
	lsc.nextCmdIdx++

	switch {
	case cmdCtx.cmd.bootstrap != nil:
		cmdCtx.bootstrapCtx = &lstreamCmdCtxBootstrap{}

		lsc.conn.stdinBuf.Write([]byte("echo reset_output\n"))
		lsc.conn.stdinBuf.Write([]byte("echo reset_output 1>&2\n"))
		lsc.conn.stdinBuf.Write([]byte("("))
		lsc.conn.stdinBuf.Write([]byte("  cat <<- 'EOF' > " + lsc.getLStreamNerdlogAgentPath() + "\n" + nerdlogAgentSh + "EOF\n"))
		lsc.conn.stdinBuf.Write([]byte("  if [[ $? != 0 ]]; then echo 'bootstrap failed'; exit 1; fi\n"))

		var parts []string
		parts = append(
			parts,
			"bash", shellQuote(lsc.getLStreamNerdlogAgentPath()),
			"logstream_info",
			"--logfile-last", shellQuote(lsc.params.LogStream.LogFileLast()),
		)

		if logFilePrev, ok := lsc.params.LogStream.LogFilePrev(); ok {
			parts = append(parts, "--logfile-prev", shellQuote(logFilePrev))
		}

		lsc.conn.stdinBuf.Write([]byte(strings.Join(parts, " ") + "\n"))
		lsc.conn.stdinBuf.Write([]byte("  if [[ $? != 0 ]]; then echo 'bootstrap failed'; exit 1; fi\n"))

		lsc.conn.stdinBuf.Write([]byte("  echo 'bootstrap ok'\n"))
		lsc.conn.stdinBuf.Write([]byte(")\n"))
		lsc.conn.stdinBuf.Write([]byte("echo exit_code:$?\n"))

	case cmdCtx.cmd.ping != nil:
		cmdCtx.pingCtx = &lstreamCmdCtxPing{}

		cmd := "whoami\n"
		lsc.conn.stdinBuf.Write([]byte(cmd))
		lsc.conn.stdinBuf.Write([]byte("echo exit_code:$?\n"))

	case cmdCtx.cmd.queryLogs != nil:
		cmdCtx.queryLogsCtx = &lstreamCmdCtxQueryLogs{
			Resp: &LogResp{
				MinuteStats: map[int64]MinuteStatsItem{},
			},
		}

		var parts []string

		if useGzip {
			parts = append(parts, "echo", gzipStartMarker, ";")
		}

		parts = append(
			parts,
			"bash", shellQuote(lsc.getLStreamNerdlogAgentPath()),
			"query",
			"--index-file", shellQuote(lsc.getLStreamIndexFilePath()),
			"--max-num-lines", shellQuote(strconv.Itoa(cmdCtx.cmd.queryLogs.maxNumLines)),
			"--logfile-last", shellQuote(lsc.params.LogStream.LogFileLast()),
		)

		if logFilePrev, ok := lsc.params.LogStream.LogFilePrev(); ok {
			parts = append(parts, "--logfile-prev", shellQuote(logFilePrev))
		}

		if !cmdCtx.cmd.queryLogs.from.IsZero() {
			parts = append(parts, "--from", shellQuote(cmdCtx.cmd.queryLogs.from.In(lsc.location).Format(queryLogsArgsTimeLayout)))
		}

		if !cmdCtx.cmd.queryLogs.to.IsZero() {
			parts = append(parts, "--to", shellQuote(cmdCtx.cmd.queryLogs.to.In(lsc.location).Format(queryLogsArgsTimeLayout)))
		}

		if cmdCtx.cmd.queryLogs.linesUntil > 0 {
			parts = append(parts, "--lines-until", shellQuote(strconv.Itoa(cmdCtx.cmd.queryLogs.linesUntil)))
		}

		parts = append(parts, agentQueryTimeFormatArgs(&lsc.timeFormat.AWKExpr)...)

		if cmdCtx.cmd.queryLogs.query != "" {
			parts = append(parts, shellQuote(cmdCtx.cmd.queryLogs.query))
		}

		if useGzip {
			parts = append(parts, "|", "gzip", ";", "echo", gzipEndMarker)
		}

		cmd := strings.Join(parts, " ") + "\n"
		lsc.params.Logger.Verbose2f("Executing query command(%s): %s", lsc.params.LogStream.Name, cmd)

		lsc.conn.stdinBuf.Write([]byte(cmd))

		// NOTE: we don't print the "exit_code:" here, because we can't reliably
		// do that across all possible shells, due to gzipping: the agent script
		// is not the last one in the pipeline.
		//
		// Instead, the agent script itself has a trap which prints this line for
		// us.

	default:
		panic(fmt.Sprintf("invalid command %+v", cmdCtx.cmd))
	}

	lsc.conn.stdinBuf.Write([]byte(fmt.Sprintf("echo 'command_done:%d'\n", cmdCtx.idx)))
	lsc.conn.stdinBuf.Write([]byte(fmt.Sprintf("echo 'command_done:%d' 1>&2\n", cmdCtx.idx)))

	lsc.changeState(LStreamClientStateConnectedBusy)
}

// getLStreamNerdlogAgentPath returns the logstream-side path to the nerdlog_agent.sh
// for the particular log stream.
func (lsc *LStreamClient) getLStreamNerdlogAgentPath() string {
	return fmt.Sprintf(
		"/tmp/nerdlog_agent_%s_%s.sh",
		lsc.params.ClientID,
		filepathToId(lsc.params.LogStream.LogFileLast()),
	)
}

// getLStreamIndexFilePath returns the logstream-side path to the index file for
// the particular log stream.
func (lsc *LStreamClient) getLStreamIndexFilePath() string {
	return fmt.Sprintf(
		"/tmp/nerdlog_agent_index_%s_%s",
		lsc.params.ClientID,
		filepathToId(lsc.params.LogStream.LogFileLast()),
	)
}

// filepathToId takes a path and returns a string suitable to be used as
// part of a filename (with all slashes removed).
func filepathToId(p string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_")
	return replacer.Replace(p)
}

func (lsc *LStreamClient) checkIfDisconnected() {
	if lsc.conn.stderrLinesCh == nil && lsc.conn.stdoutLinesCh == nil {
		// We're fully disconnected
		lsc.changeState(LStreamClientStateDisconnected)

		if lsc.tearingDown {
			close(lsc.disconnectedBeforeTeardownCh)
		} else {
			lsc.changeState(LStreamClientStateConnecting)
		}
	}
}

func (lsc *LStreamClient) checkCommandDone(
	line string, cmdCtx *lstreamCmdCtx, isStderr bool,
) bool {
	if !strings.HasPrefix(line, "command_done:") {
		return false
	}

	_, err := parseCommandDoneLine(line, cmdCtx.idx)
	if err != nil {
		lsc.params.Logger.Errorf("Got malformed command_done line: %s (%s)", line, err.Error())
		return true
	}

	if isStderr {
		cmdCtx.stderrDone = true
	} else {
		cmdCtx.stdoutDone = true
	}

	lsc.handleCommandResultsIfDone(cmdCtx)
	return true
}

func (lsc *LStreamClient) checkError(
	line string, cmdCtx *lstreamCmdCtx,
) bool {
	if !strings.HasPrefix(line, "error:") {
		return false
	}

	// The agent script printed an error; it means that the whole execution will
	// be considered failed once it's done. For now we just add the error to the
	// resulting response.
	errMsg := strings.TrimPrefix(line, "error:")
	cmdCtx.errs = append(cmdCtx.errs, errors.New(errMsg))

	return true
}

func (lsc *LStreamClient) checkDebug(
	line string, cmdCtx *lstreamCmdCtx,
) bool {
	if !strings.HasPrefix(line, "debug:") {
		return false
	}

	// TODO: save it somewhere. For now, it's ignored.

	return true
}

func (lsc *LStreamClient) checkExitCode(
	line string, cmdCtx *lstreamCmdCtx,
) bool {
	if !strings.HasPrefix(line, "exit_code:") {
		return false
	}

	cmdCtx.exitCode = strings.TrimPrefix(line, "exit_code:")
	lsc.params.Logger.Verbose1f("Received exit code: %q", cmdCtx.exitCode)
	return true
}

func (lsc *LStreamClient) checkResetOutput(
	line string, cmdCtx *lstreamCmdCtx, isStderr bool,
) bool {
	if line != "reset_output" {
		return false
	}

	if isStderr {
		cmdCtx.unhandledStderr = nil
	} else {
		cmdCtx.unhandledStdout = nil
	}

	return true
}

// handleCommandResultsIfDone should be called whenever the previously ran
// command is done.
func (lsc *LStreamClient) handleCommandResultsIfDone(cmdCtx *lstreamCmdCtx) {
	if !cmdCtx.stdoutDone || !cmdCtx.stderrDone {
		return
	}

	// Command is done.

	switch {
	case cmdCtx.cmd.bootstrap != nil:
		if cmdCtx.bootstrapCtx.receivedSuccess && len(cmdCtx.errs) == 0 {
			// Bootstrap script has ran successfully, let's now try to autodetect the
			// envelope log format.
			timeFormat, err := GetTimeFormatDescrFromLogLines(lsc.exampleLogLines)
			if err != nil {
				cmdCtx.errs = append(cmdCtx.errs, err)
			} else {
				// All good
				lsc.params.Logger.Infof(
					"Detected time format based on %d log lines: %q",
					len(lsc.exampleLogLines),
					timeFormat.TimestampLayout,
				)
				lsc.timeFormat = timeFormat
				lsc.changeState(LStreamClientStateConnectedIdle)
				return
			}
		}

		// There was an issue with bootstrapping.

		err := summaryCmdError(cmdCtx)
		// If error is nil (which means, there were no "error:" printed, and
		// bootstrap exited with 0 code, but since there was also no "bootstrap
		// ok" marker, this is very weird), then still generate an error saying
		// that there was no "bootstrap ok" message.
		if err == nil {
			err = errorFromStdoutStderr(
				"there was no 'bootstrap ok' message",
				cmdCtx.unhandledStdout,
				cmdCtx.unhandledStderr,
			)
		}

		lsc.sendUpdate(&LStreamClientUpdate{
			BootstrapDetails: &BootstrapDetails{
				Err: err.Error(),
			},
		})

		lsc.changeState(LStreamClientStateDisconnected)

	case cmdCtx.cmd.ping != nil:
		lsc.sendCmdResp(nil, nil)
		lsc.changeState(LStreamClientStateConnectedIdle)

	case cmdCtx.cmd.queryLogs != nil:
		resp := cmdCtx.queryLogsCtx.Resp
		lsc.sendCmdResp(resp, summaryCmdError(cmdCtx))
		lsc.changeState(LStreamClientStateConnectedIdle)

	default:
		panic(fmt.Sprintf("unhandled cmd %+v", cmdCtx.cmd))
	}
}

// InferYear infers year from the month of the given timestamp, and the current
// time. Resulting timestamp (with the year populated) is then returned.
//
// Most of the time it just uses the current year, but on the year boundary
// it can return previous or next year.
func InferYear(t time.Time) time.Time {
	now := time.Now()

	// If month of the syslog being parsed is the same as the current month, just
	// use the current year.
	if now.Month() == t.Month() {
		return timeWithYear(t, now.Year())
	}

	// Month of the syslog is different from the current month, so we need to
	// have logic for the boundary of the year.

	if t.Month() == time.December && now.Month() == time.January {
		// We're in January now and we're parsing some logs from December.
		return timeWithYear(t, now.Year()-1)
	} else if t.Month() == time.January && now.Month() == time.December {
		// We're in December now and we're parsing some logs from January.
		// It's weird to get timestamp from the future, but better to have a case
		// for that.
		return timeWithYear(t, now.Year()+1)
	}

	// For all other cases, still use the current year.
	return timeWithYear(t, now.Year())
}

func timeWithYear(t time.Time, year int) time.Time {
	return time.Date(
		year,
		t.Month(),
		t.Day(),

		t.Hour(),
		t.Minute(),
		t.Second(),
		t.Nanosecond(),
		t.Location(),
	)
}

type parseLineResult struct {
	// time might or might not be populated: most of our messages contain an
	// extra (more precise) timestamp, so in this case it'll be populated here,
	// and client code should use it.
	time               time.Time
	decreasedTimestamp bool

	// msg is the actual log message.
	msg string

	// ctxMap contains context for the message.
	ctxMap map[string]string
}

func (lsc *LStreamClient) parseLine(logMsg *LogMsg) error {
	if err := lsc.parseLogMsgTimestamp(logMsg); err != nil {
		return errors.Annotatef(err, "parsing time")
	}

	// TODO: offload envelope parsing to Lua (and make it usable from
	// the user Lua scripts as well).
	if err := lsc.parseLogMsgEnvelopeDefault(logMsg); err != nil {
		return errors.Annotatef(err, "parsing envelope")
	}

	// TODO: offload the custom parsing to Lua
	if err := lsc.parseLogMsgLevelDefault(logMsg); err != nil {
		return errors.Annotatef(err, "custom parsing")
	}

	// TODO: invoke user Lua script, if present.

	return nil
}

func (lsc *LStreamClient) parseLogMsgTimestamp(logMsg *LogMsg) error {
	msg := logMsg.Msg

	timeLayout := lsc.timeFormat.TimestampLayout
	timestampLen := len(timeLayout)

	// If the layout ends with the offset like "Z07" or "Z07:00", but the
	// actual timestamp string is in UTC and it ends with just "Z", we then
	// need to remove that extra
	zIdx := strings.Index(timeLayout, "Z07")
	if zIdx >= 0 && len(msg) >= zIdx && msg[zIdx] == 'Z' {
		// We have a Z in the timestamp, so there should be no offset after it.
		timestampLen = zIdx + 1
	}

	if len(msg) < timestampLen {
		return errors.Errorf("line %q is too short to have a timestamp", msg)
	}

	t, err := time.ParseInLocation(timeLayout, msg[:timestampLen], lsc.location)
	if err != nil {
		return errors.Annotatef(err, "parsing time in log msg")
	}

	// If the location we get from the actual logs doesn't match what we have,
	// trust the logs more.
	//
	// NOTE: this new timezone is still not fully effective until the next query,
	// because even the request itself was likely done wrong. Also, if mstats are
	// already parsed, their timestamp is also wrong (that one can technically be
	// solved by printing mstats after logs). Maybe we should somehow show a
	// warning that the timezone was overridden.

	// TODO: uncomment when it's well tested; right now it doesn't work well
	// because e.g. if the timezone is "America/New_York", but then in the logs
	// we have "-05:00", which is a different timezone (not New York, but a fixed
	// one), and so we'll always be overwriting this timezone here. Most of the
	// time it's pretty much harmless, but on the edge of DST it will do the
	// wrong thing.
	//if t.Location() != lsc.location {
	//lsc.params.Logger.Infof(
	//"Log line timestamp has a different location: %s instead of %s; trusting logs more",
	//t.Location(), lsc.location,
	//)
	//lsc.location = t.Location()
	//}

	if t.Year() == 0 {
		t = InferYear(t)
	}
	t = t.UTC()

	// Parsed the time successfully; update it in the LogMsg, and also remove the
	// leading timestamp from the message.
	logMsg.Time = t
	logMsg.Msg = strings.TrimSpace(msg[len(timeLayout):])

	return nil
}

// parseLogMsgEnvelopeDefault takes the LogMsg where the time was already
// stripped from the Msg, so for syslog, it looks like this:
//
//	"myhost myprogram[1234]: Something happened"
//
// If the Msg indeed looks like a syslog message, it extracts the hostname,
// program and pid from it, populates them in the Context, and updates the
// message to contain the rest of the payload.
//
// If the Msg doesn't have this structure, parseLogMsgEnvelopeDefault is a
// no-op.
func (lsc *LStreamClient) parseLogMsgEnvelopeDefault(logMsg *LogMsg) error {
	matches := syslogRegex.FindStringSubmatch(logMsg.Msg)
	if len(matches) == 0 {
		// Message doesn't match syslog pattern, no-op
		// TODO: we might want to support more formats
		return nil
	}

	// Extract fields from regex match
	hostname := matches[1]
	program := matches[2]
	pid := matches[3]
	rest := matches[4]

	logMsg.Context["hostname"] = hostname
	logMsg.Context["program"] = program
	logMsg.Context["pid"] = pid

	logMsg.Msg = rest
	return nil
}

// parseLogMsgLevelDefault tries to guess what the level of the message could
// be, based on commonly used patterns in the message like "error", "info",
// "[E]", "[I]" etc.
func (lsc *LStreamClient) parseLogMsgLevelDefault(logMsg *LogMsg) error {
	msg := strings.ToLower(logMsg.Msg)

	switch {
	case strings.Contains(msg, "[f]"):
		logMsg.Level = LogLevelError
	case strings.Contains(msg, "[e]"):
		logMsg.Level = LogLevelError
	case strings.Contains(msg, "[w]"):
		logMsg.Level = LogLevelWarn
	case strings.Contains(msg, "[i]"):
		logMsg.Level = LogLevelInfo
	case strings.Contains(msg, "[d]"):
		logMsg.Level = LogLevelDebug
	default:
		logMsg.Level = LogLevelUnknown
	}

	if logMsg.Level != LogLevelUnknown {
		return nil
	}

	// Regex patterns for whole words or bracketed levels
	patterns := []struct {
		regex *regexp.Regexp
		level LogLevel
	}{
		{regexp.MustCompile(`\berror\b|\berro\b|\berr\b|\bcrit\b|\bcritical\b|\bfatal\b`), LogLevelError},
		{regexp.MustCompile(`\bwarn(ing)?\b`), LogLevelWarn},
		{regexp.MustCompile(`\binfo\b`), LogLevelInfo},
		{regexp.MustCompile(`\bdebu(g)?\b`), LogLevelDebug},
	}

	for _, p := range patterns {
		if p.regex.MatchString(msg) {
			logMsg.Level = p.level
			return nil
		}
	}

	logMsg.Level = LogLevelUnknown
	return nil
}

func combineErrors(errs []error) error {
	var err error
	if len(errs) == 1 {
		err = errs[0]
	} else if len(errs) > 0 {
		ss := []string{}
		for _, e := range errs {
			ss = append(ss, e.Error())
		}

		err = errors.Errorf("%d errors: %s", len(errs), strings.Join(ss, "; "))
	}

	return err
}

func errorFromStdoutStderr(prefix string, stdout []string, stderr []string) error {
	var sb strings.Builder

	sb.WriteString(prefix)
	sb.WriteString("\n")

	if len(stdout) > 0 {
		sb.WriteString("stdout:\n")
		for _, line := range stdout {
			sb.WriteString(line)
			sb.WriteString("\n")
		}
	}

	if len(stderr) > 0 {
		if sb.Len() > 0 {
			sb.WriteString("------\n")
		}
		sb.WriteString("stderr:\n")
		for _, line := range stderr {
			sb.WriteString(line)
			sb.WriteString("\n")
		}
	}

	return errors.New(sb.String())
}

func summaryCmdError(cmdCtx *lstreamCmdCtx) error {
	if len(cmdCtx.errs) > 0 {
		return combineErrors(cmdCtx.errs)
	} else if cmdCtx.exitCode != "0" {
		return errorFromStdoutStderr(
			fmt.Sprintf("agent exited with non-zero code '%s'", cmdCtx.exitCode),
			cmdCtx.unhandledStdout,
			cmdCtx.unhandledStderr,
		)
	} else {
		return nil
	}
}

type commandDoneDetails struct {
	idx int
}

func parseCommandDoneLine(line string, expectedIdx int) (*commandDoneDetails, error) {
	parts := strings.Split(line, ":")
	if len(parts) != 2 {
		return nil, errors.Errorf("expected two parts, got %d", len(parts))
	}

	rxIdx, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, errors.Annotatef(err, "parsing idx as integer")
	}

	if rxIdx != expectedIdx {
		return nil, errors.Errorf("received unexpected index with command_done: waiting for %d, got %d", expectedIdx, rxIdx)
	}

	return &commandDoneDetails{
		idx: rxIdx,
	}, nil
}

func shellQuote(s string) string {
	return fmt.Sprintf("'%s'", strings.Replace(s, "'", "'\"'\"'", -1))
}

func agentQueryTimeFormatArgs(awkExpr *TimeFormatAWKExpr) []string {
	return []string{
		"--awktime-month", shellQuote(awkExpr.Month),
		"--awktime-year", shellQuote(awkExpr.Year),
		"--awktime-day", shellQuote(awkExpr.Day),
		"--awktime-hhmm", shellQuote(awkExpr.HHMM),
		"--awktime-minute-key", shellQuote(awkExpr.MinuteKey),
	}
}
