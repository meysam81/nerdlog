package core

import "time"

type lstreamCmd struct {
	// respCh must be either nil, or 1-buffered and it'll receive exactly one
	// message.
	respCh chan lstreamCmdRes

	// Exactly one of the fields below must be non-nil.

	bootstrap *lstreamCmdBootstrap
	ping      *lstreamCmdPing
	queryLogs *lstreamCmdQueryLogs
}

type lstreamCmdCtx struct {
	cmd lstreamCmd

	idx int

	bootstrapCtx *lstreamCmdCtxBootstrap
	pingCtx      *lstreamCmdCtxPing
	queryLogsCtx *lstreamCmdCtxQueryLogs

	// Initially, stdoutDoneIdx and stderrDoneIdx are set to false. Once we
	// receive the "command_done" marker from either stdout or stderr, we set the
	// corresponding bool here to true. Once both are set, we consider the
	// command execution done, and parse the results.
	stdoutDone bool
	stderrDone bool

	// errs contains all errors accumulated during command execution. This
	// includes errors printed by the nerdlog_agent.sh (lines starting from
	// "error:", on either stderr or stdout), as well as any errors generated
	// on the Go side, e.g. failure to parse some other output.
	errs []error

	exitCode string

	// unhandledStdout and unhandledStderr contain the lines which the Go app did
	// not make sense of. These are usually ignored, but if the the
	// nerdlog_agent.sh returns an error code, and there are no specific errors
	// printed (lines with the "error:" prefix), then we'll print all these
	// as an error message.
	unhandledStdout []string
	unhandledStderr []string
}

type lstreamCmdRes struct {
	hostname string

	err  error
	resp interface{}
}

type lstreamCmdBootstrap struct{}

type lstreamCmdCtxBootstrap struct {
	receivedSuccess bool
	receivedFailure bool
}

type lstreamCmdPing struct{}

type lstreamCmdCtxPing struct {
}

type lstreamCmdQueryLogs struct {
	maxNumLines int

	from time.Time
	to   time.Time

	query string

	// If linesUntil is not zero, it'll be passed to nerdlog_agent.sh as --lines-until.
	// Effectively, only logs BEFORE this log line (not including it) will be output.
	linesUntil int
}

type lstreamCmdCtxQueryLogs struct {
	Resp *LogResp

	logfiles []logfileWithStartingLinenumber
	lastTime time.Time
}

type logfileWithStartingLinenumber struct {
	filename       string
	fromLinenumber int
}
