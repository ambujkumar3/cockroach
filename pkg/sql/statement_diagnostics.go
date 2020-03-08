// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package sql

import (
	"context"
	"encoding/binary"
	"errors"

	"github.com/cockroachdb/cockroach/pkg/gossip"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/gogo/protobuf/jsonpb"
)

// StmtDiagnosticsRequester is the interface into stmtDiagnosticsRequestRegistry
// used by AdminUI endpoints.
type StmtDiagnosticsRequester interface {
	// InsertRequest adds an entry to system.statement_diagnostics_requests for
	// tracing a query with the given fingerprint. Once this returns, calling
	// shouldCollectDiagnostics() on the current node will return true for the given
	// fingerprint.
	InsertRequest(ctx context.Context, fprint string) error
}

// stmtDiagnosticsRequestRegistry maintains a view on the statement fingerprints
// on which data is to be collected (i.e. system.statement_diagnostics_requests)
// and provides utilities for checking a query against this list and satisfying
// the requests.
type stmtDiagnosticsRequestRegistry struct {
	mu struct {
		// NOTE: This lock can't be held while the registry runs any statements
		// internally; it'd deadlock.
		syncutil.Mutex
		// requests waiting for the right query to come along.
		requestFingerprints map[stmtDiagRequestID]string
		// ids of requests that this node is in the process of servicing.
		ongoing map[stmtDiagRequestID]struct{}

		// epoch is observed before reading system.statement_diagnostics_requests, and then
		// checked again before loading the tables contents. If the value changed in
		// between, then the table contents might be stale.
		epoch int
	}
	ie     *InternalExecutor
	db     *kv.DB
	gossip *gossip.Gossip
	nodeID roachpb.NodeID
}

func newStmtDiagnosticsRequestRegistry(
	ie *InternalExecutor, db *kv.DB, g *gossip.Gossip, nodeID roachpb.NodeID,
) *stmtDiagnosticsRequestRegistry {
	r := &stmtDiagnosticsRequestRegistry{
		ie:     ie,
		db:     db,
		gossip: g,
		nodeID: nodeID,
	}
	// Some tests pass a nil gossip.
	if g != nil {
		g.RegisterCallback(gossip.KeyGossipStatementDiagnosticsRequest, r.gossipNotification)
	}
	return r
}

// stmtDiagRequestID is the ID of a diagnostics request, corresponding to the id
// column in statement_diagnostics_requests.
// A zero ID is invalid.
type stmtDiagRequestID int

// addRequestInternalLocked adds a request to r.mu.requests. If the request is
// already present, the call is a noop.
func (r *stmtDiagnosticsRequestRegistry) addRequestInternalLocked(
	ctx context.Context, id stmtDiagRequestID, queryFingerprint string,
) {
	if r.findRequestLocked(id) {
		// Request already exists.
		return
	}
	if r.mu.requestFingerprints == nil {
		r.mu.requestFingerprints = make(map[stmtDiagRequestID]string)
	}
	r.mu.requestFingerprints[id] = queryFingerprint
}

func (r *stmtDiagnosticsRequestRegistry) findRequestLocked(requestID stmtDiagRequestID) bool {
	_, ok := r.mu.requestFingerprints[requestID]
	if ok {
		return true
	}
	_, ok = r.mu.ongoing[requestID]
	return ok
}

// InsertRequest is part of the StmtDiagnosticsRequester interface.
func (r *stmtDiagnosticsRequestRegistry) InsertRequest(ctx context.Context, fprint string) error {
	_, err := r.insertRequestInternal(ctx, fprint)
	return err
}

func (r *stmtDiagnosticsRequestRegistry) insertRequestInternal(
	ctx context.Context, fprint string,
) (stmtDiagRequestID, error) {
	var requestID stmtDiagRequestID
	err := r.db.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		// Check if there's already a pending request for this fingerprint.
		row, err := r.ie.QueryRowEx(ctx, "stmt-diag-check-pending", txn,
			sqlbase.InternalExecutorSessionDataOverride{
				User: security.RootUser,
			},
			"SELECT count(1) FROM system.statement_diagnostics_requests "+
				"WHERE completed = false AND statement_fingerprint = $1",
			fprint)
		if err != nil {
			return err
		}
		count := int(*row[0].(*tree.DInt))
		if count != 0 {
			return errors.New("a pending request for the requested fingerprint already exists")
		}

		row, err = r.ie.QueryRowEx(ctx, "stmt-diag-insert-request", txn,
			sqlbase.InternalExecutorSessionDataOverride{
				User: security.RootUser,
			},
			"INSERT INTO system.statement_diagnostics_requests (statement_fingerprint, requested_at) "+
				"VALUES ($1, $2) RETURNING id",
			fprint, timeutil.Now())
		if err != nil {
			return err
		}
		requestID = stmtDiagRequestID(*row[0].(*tree.DInt))
		return nil
	})
	if err != nil {
		return 0, err
	}

	// Manually insert the request in the (local) registry. This lets this node
	// pick up the request quickly if the right query comes around, without
	// waiting for the poller.
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mu.epoch++
	r.addRequestInternalLocked(ctx, requestID, fprint)

	// Notify all the other nodes that they have to poll.
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, uint64(requestID))
	if err := r.gossip.AddInfo(gossip.KeyGossipStatementDiagnosticsRequest, buf, 0 /* ttl */); err != nil {
		log.Warningf(ctx, "error notifying of diagnostics request: %s", err)
	}

	return requestID, nil
}

// shouldCollectDiagnostics checks whether any data should be collected for the
// given query. If data is to be collected, the returned function needs to be
// called once the data was collected.
//
// Once shouldCollectDiagnostics returns true, it will not return true again on
// this node for the same diagnostics request.
func (r *stmtDiagnosticsRequestRegistry) shouldCollectDiagnostics(
	ctx context.Context, ast tree.Statement,
) (bool, func(ctx context.Context, trace tracing.Recording)) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Return quickly if we have no requests to trace.
	if len(r.mu.requestFingerprints) == 0 {
		return false, nil
	}

	fprint := tree.AsStringWithFlags(ast, tree.FmtHideConstants)

	var reqID stmtDiagRequestID
	for id, fingerprint := range r.mu.requestFingerprints {
		if fingerprint == fprint {
			reqID = id
			break
		}
	}
	if reqID == 0 {
		return false, nil
	}

	// Remove the request.
	delete(r.mu.requestFingerprints, reqID)
	if r.mu.ongoing == nil {
		r.mu.ongoing = make(map[stmtDiagRequestID]struct{})
	}

	r.mu.ongoing[reqID] = struct{}{}

	return true, func(ctx context.Context, trace tracing.Recording) {
		defer func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			// Remove the request from r.mu.ongoing.
			delete(r.mu.ongoing, reqID)
		}()

		if err := r.insertDiagnostics(ctx, reqID, fprint, tree.AsString(ast), trace); err != nil {
			log.Warningf(ctx, "failed to insert trace: %s", err)
		}
	}
}

// insertDiagnostics inserts a trace into system.statement_diagnostics and marks
// the corresponding request as completed in
// system.statement_diagnostics_requests.
func (r *stmtDiagnosticsRequestRegistry) insertDiagnostics(
	ctx context.Context,
	reqID stmtDiagRequestID,
	stmtFingerprint string,
	stmt string,
	trace tracing.Recording,
) error {
	return r.db.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		{
			row, err := r.ie.QueryRowEx(ctx, "stmt-diag-check-completed", txn,
				sqlbase.InternalExecutorSessionDataOverride{User: security.RootUser},
				"SELECT count(1) FROM system.statement_diagnostics_requests WHERE id = $1 AND completed = false",
				reqID)
			if err != nil {
				return err
			}
			cnt := int(*row[0].(*tree.DInt))
			if cnt == 0 {
				// Someone else already marked the request as completed. We've traced for nothing.
				// This can only happen once per node, per request since we're going to
				// remove the request from the registry.
				return nil
			}
		}

		var traceID int
		if json, err := traceToJSON(trace); err != nil {
			row, err := r.ie.QueryRowEx(ctx, "stmt-diag-insert-trace", txn,
				sqlbase.InternalExecutorSessionDataOverride{User: security.RootUser},
				"INSERT INTO system.statement_diagnostics "+
					"(statement_fingerprint, statement, collected_at, error) "+
					"VALUES ($1, $2, $3, $4) RETURNING id",
				stmtFingerprint, stmt, timeutil.Now(), err.Error())
			if err != nil {
				return err
			}
			traceID = int(*row[0].(*tree.DInt))
		} else {
			// Insert the trace into system.statement_diagnostics.
			row, err := r.ie.QueryRowEx(ctx, "stmt-diag-insert-trace", txn,
				sqlbase.InternalExecutorSessionDataOverride{User: security.RootUser},
				"INSERT INTO system.statement_diagnostics "+
					"(statement_fingerprint, statement, collected_at, trace) "+
					"VALUES ($1, $2, $3, $4) RETURNING id",
				stmtFingerprint, stmt, timeutil.Now(), json)
			if err != nil {
				return err
			}
			traceID = int(*row[0].(*tree.DInt))
		}

		// Mark the request from system.statement_diagnostics_request as completed.
		_, err := r.ie.ExecEx(ctx, "stmt-diag-mark-completed", txn,
			sqlbase.InternalExecutorSessionDataOverride{User: security.RootUser},
			"UPDATE system.statement_diagnostics_requests "+
				"SET completed = true, statement_diagnostics_id = $1 WHERE id = $2",
			traceID, reqID)
		return err
	})
}

// pollRequests reads the pending rows from system.statement_diagnostics_requests and
// updates r.mu.requests accordingly.
func (r *stmtDiagnosticsRequestRegistry) pollRequests(ctx context.Context) error {
	var rows []tree.Datums
	// Loop until we run the query without straddling an epoch increment.
	for {
		r.mu.Lock()
		epoch := r.mu.epoch
		r.mu.Unlock()

		var err error
		rows, err = r.ie.QueryEx(ctx, "stmt-diag-poll", nil, /* txn */
			sqlbase.InternalExecutorSessionDataOverride{
				User: security.RootUser,
			},
			"SELECT id, statement_fingerprint FROM system.statement_diagnostics_requests "+
				"WHERE completed = false")
		if err != nil {
			return err
		}

		r.mu.Lock()
		// If the epoch changed it means that a request was added to the registry
		// manually while the query was running. In that case, if we were to process
		// the query results normally, we might remove that manually-added request.
		if r.mu.epoch != epoch {
			r.mu.Unlock()
			continue
		}
		break
	}
	defer r.mu.Unlock()

	var ids util.FastIntSet
	for _, row := range rows {
		id := stmtDiagRequestID(*row[0].(*tree.DInt))
		fprint := string(*row[1].(*tree.DString))

		ids.Add(int(id))
		r.addRequestInternalLocked(ctx, id, fprint)
	}

	// Remove all other requests.
	for id := range r.mu.requestFingerprints {
		if !ids.Contains(int(id)) {
			delete(r.mu.requestFingerprints, id)
		}
	}
	return nil
}

// gossipNotification is called in response to a gossip update informing us that
// we need to poll.
func (r *stmtDiagnosticsRequestRegistry) gossipNotification(s string, value roachpb.Value) {
	if s != gossip.KeyGossipStatementDiagnosticsRequest {
		// We don't expect any other notifications. Perhaps in a future version we
		// added other keys with the same prefix.
		return
	}
	requestID := stmtDiagRequestID(binary.LittleEndian.Uint64(value.RawBytes))
	r.mu.Lock()
	if r.findRequestLocked(requestID) {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	if err := r.pollRequests(context.TODO()); err != nil {
		log.Warningf(context.TODO(), "failed to poll for diagnostics requests: %s", err)
	}
}

func normalizeSpan(s tracing.RecordedSpan, trace tracing.Recording) tracing.NormalizedSpan {
	var n tracing.NormalizedSpan
	n.Operation = s.Operation
	n.StartTime = s.StartTime
	n.Duration = s.Duration
	n.Tags = s.Tags
	n.Logs = s.Logs

	for _, ss := range trace {
		if ss.ParentSpanID != s.SpanID {
			continue
		}
		n.Children = append(n.Children, normalizeSpan(ss, trace))
	}
	return n
}

// traceToJSON converts a trace to a JSON format suitable for the
// system.statement_diagnostics.trace column.
//
// traceToJSON assumes that the first span in the recording contains all the
// other spans.
func traceToJSON(trace tracing.Recording) (string, error) {
	root := normalizeSpan(trace[0], trace)
	marshaller := jsonpb.Marshaler{
		Indent: "  ",
	}
	return marshaller.MarshalToString(&root)
}
