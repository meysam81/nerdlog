package main

import (
	"github.com/dimonomid/nerdlog/shellescape"
	"github.com/juju/errors"
)

// QueryFull contains everything that defines a query: the logstreams filter, time range,
// and the query to filter logs.
type QueryFull struct {
	LStreams string
	Time     string
	Query    string

	SelectQuery SelectQuery
}

var execName = "nerdlog"

// numShellParts defines how many shell parts should be in the
// shell-command-marshalled form. It looks like this:
//
//	nerdlog --lstreams <value> --time <value> --pattern <value>
//
// Therefore, there are 7 parts.
var numShellParts = 1 + 3*2

func (qf *QueryFull) MarshalShellCmd() string {
	parts := qf.MarshalShellCmdParts()
	return shellescape.Escape(parts)
}

func (qf *QueryFull) UnmarshalShellCmd(cmd string) error {
	parts, err := shellescape.Parse(cmd)
	if err != nil {
		return errors.Trace(err)
	}

	if err := qf.UnmarshalShellCmdParts(parts); err != nil {
		return errors.Trace(err)
	}

	return nil
}

func (qf *QueryFull) MarshalShellCmdParts() []string {
	parts := make([]string, 0, numShellParts)

	parts = append(parts, execName)
	parts = append(parts, "--lstreams", qf.LStreams)
	parts = append(parts, "--time", qf.Time)
	parts = append(parts, "--pattern", qf.Query)
	parts = append(parts, "--selquery", string(qf.SelectQuery))

	return parts
}

// UnmarshalShellCmdParts unmarshals shell command parts to the receiver
// QueryFull.  Note that no checks are performed as to whether LStreams,
// Time or Query are actually valid strings.
func (qf *QueryFull) UnmarshalShellCmdParts(parts []string) error {
	if len(parts) < numShellParts {
		return errors.Errorf(
			"not enough parts; should be at least %d, got %d", numShellParts, len(parts),
		)
	}

	if parts[0] != execName {
		return errors.Errorf("command should begin from %q, but it's %q", execName, parts[0])
	}

	parts = parts[1:]

	var lstreamsSet, timeSet, querySet, selectQuerySet bool

	for ; len(parts) >= 2; parts = parts[2:] {
		switch parts[0] {
		case "--lstreams":
			qf.LStreams = parts[1]
			lstreamsSet = true
		case "--time":
			qf.Time = parts[1]
			timeSet = true
		case "--pattern":
			qf.Query = parts[1]
			querySet = true
		case "--selquery":
			qf.SelectQuery = SelectQuery(parts[1])
			selectQuerySet = true
		}
	}

	if !lstreamsSet {
		return errors.Errorf("--lstreams is missing")
	}

	if !timeSet {
		return errors.Errorf("--time is missing")
	}

	if !querySet {
		return errors.Errorf("--pattern is missing")
	}

	if !selectQuerySet {
		// NOTE: we can't return an error here since selquery was not there from the beginning
		qf.SelectQuery = DefaultSelectQuery
	}

	return nil
}
