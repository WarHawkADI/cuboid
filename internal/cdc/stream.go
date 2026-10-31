// Package cdc reads the main database's change stream by logical replication
// (pgoutput) and assembles whole transactions. It never polls tables and adds
// no triggers: the main database only writes WAL it writes anyway.
package cdc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"cuboid/internal/engine"
)

// Message prefixes written with pg_logical_emit_message.
const (
	PrefixHeartbeat = "cuboid.heartbeat"
	PrefixFence     = "cuboid.fence"
)

// Handler receives the stream. All calls happen on the stream's goroutine.
type Handler interface {
	// Txn is called for every committed transaction, in commit order.
	Txn(ctx context.Context, txn *engine.Txn) error
	// Message is called for a non-transactional logical decoding message.
	Message(ctx context.Context, prefix, content string, lsn pglogrepl.LSN) error
	// Truncate is called when a published table is truncated.
	Truncate(ctx context.Context, tables []string, lsn pglogrepl.LSN) error
	// Idle is called between messages; the handler flushes when due.
	Idle(ctx context.Context) error
	// Confirmed is the last LSN durably committed in the aggregate store.
	// Only this position is ever reported to the main database.
	Confirmed() pglogrepl.LSN
}

// ErrTruncated stops the stream: rollups are invalid after a TRUNCATE and
// must be rebuilt from the main database.
var ErrTruncated = errors.New("a published table was truncated")

// Stream is one replication connection consuming one slot.
type Stream struct {
	Conn        *pgconn.PgConn
	Slot        string
	Publication string
	Log         *slog.Logger

	relations map[uint32]*pglogrepl.RelationMessage
	cur       *engine.Txn
}

// CreateSlot creates the slot with an exported snapshot. The snapshot stays
// valid only while this connection issues no other command, so the caller
// must finish the backfill before calling Run.
func (s *Stream) CreateSlot(ctx context.Context, temporary bool) (consistent pglogrepl.LSN, snapshot string, err error) {
	res, err := pglogrepl.CreateReplicationSlot(ctx, s.Conn, s.Slot, "pgoutput",
		pglogrepl.CreateReplicationSlotOptions{Temporary: temporary, SnapshotAction: "EXPORT_SNAPSHOT"})
	if err != nil {
		return 0, "", fmt.Errorf("create replication slot %s: %w", s.Slot, err)
	}
	lsn, err := pglogrepl.ParseLSN(res.ConsistentPoint)
	if err != nil {
		return 0, "", err
	}
	return lsn, res.SnapshotName, nil
}

// Run streams from start until ctx ends or the handler fails.
func (s *Stream) Run(ctx context.Context, start pglogrepl.LSN, h Handler) error {
	s.relations = map[uint32]*pglogrepl.RelationMessage{}
	err := pglogrepl.StartReplication(ctx, s.Conn, s.Slot, start, pglogrepl.StartReplicationOptions{
		PluginArgs: []string{"proto_version '1'", fmt.Sprintf("publication_names '%s'", s.Publication), "messages 'true'"},
	})
	if err != nil {
		return fmt.Errorf("start replication on %s at %s: %w", s.Slot, start, err)
	}
	s.Log.Info("replication started", "slot", s.Slot, "from", start.String())
	nextStatus := time.Now()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(nextStatus) {
			if err := s.sendStatus(ctx, h.Confirmed()); err != nil {
				return err
			}
			nextStatus = time.Now().Add(2 * time.Second)
		}
		if err := h.Idle(ctx); err != nil {
			return err
		}
		rctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		raw, err := s.Conn.ReceiveMessage(rctx)
		cancel()
		if err != nil {
			if pgconn.Timeout(err) {
				continue
			}
			return fmt.Errorf("receive replication message: %w", err)
		}
		switch msg := raw.(type) {
		case *pgproto3.ErrorResponse:
			return fmt.Errorf("replication error: %s", msg.Message)
		case *pgproto3.CopyData:
			switch msg.Data[0] {
			case pglogrepl.PrimaryKeepaliveMessageByteID:
				ka, err := pglogrepl.ParsePrimaryKeepaliveMessage(msg.Data[1:])
				if err != nil {
					return err
				}
				if ka.ReplyRequested {
					nextStatus = time.Time{}
				}
			case pglogrepl.XLogDataByteID:
				xld, err := pglogrepl.ParseXLogData(msg.Data[1:])
				if err != nil {
					return err
				}
				if err := s.handle(ctx, xld, h); err != nil {
					return err
				}
			}
		}
	}
}

func (s *Stream) sendStatus(ctx context.Context, lsn pglogrepl.LSN) error {
	err := pglogrepl.SendStandbyStatusUpdate(ctx, s.Conn, pglogrepl.StandbyStatusUpdate{
		WALWritePosition: lsn, WALFlushPosition: lsn, WALApplyPosition: lsn, ClientTime: time.Now(),
	})
	if err != nil {
		return fmt.Errorf("send standby status: %w", err)
	}
	return nil
}

func (s *Stream) handle(ctx context.Context, xld pglogrepl.XLogData, h Handler) error {
	m, err := pglogrepl.Parse(xld.WALData)
	if err != nil {
		return fmt.Errorf("decode pgoutput message: %w", err)
	}
	switch m := m.(type) {
	case *pglogrepl.RelationMessage:
		s.relations[m.RelationID] = m
	case *pglogrepl.BeginMessage:
		s.cur = &engine.Txn{Xid: m.Xid}
	case *pglogrepl.InsertMessage:
		rel, err := s.rel(m.RelationID)
		if err != nil {
			return err
		}
		s.add(engine.Change{Table: rel.RelationName, Kind: engine.Insert, New: decode(rel, m.Tuple, nil)})
	case *pglogrepl.UpdateMessage:
		rel, err := s.rel(m.RelationID)
		if err != nil {
			return err
		}
		var old engine.Row
		if m.OldTuple != nil {
			old = decode(rel, m.OldTuple, nil)
		}
		s.add(engine.Change{Table: rel.RelationName, Kind: engine.Update, Old: old, New: decode(rel, m.NewTuple, old)})
	case *pglogrepl.DeleteMessage:
		rel, err := s.rel(m.RelationID)
		if err != nil {
			return err
		}
		s.add(engine.Change{Table: rel.RelationName, Kind: engine.Delete, Old: decode(rel, m.OldTuple, nil)})
	case *pglogrepl.TruncateMessage:
		var tables []string
		for _, id := range m.RelationIDs {
			if rel, ok := s.relations[id]; ok {
				tables = append(tables, rel.RelationName)
			}
		}
		return h.Truncate(ctx, tables, xld.WALStart)
	case *pglogrepl.CommitMessage:
		if s.cur == nil {
			return errors.New("commit without begin in the replication stream")
		}
		s.cur.CommitLSN = uint64(m.CommitLSN)
		s.cur.CommitTime = m.CommitTime
		txn := s.cur
		s.cur = nil
		return h.Txn(ctx, txn)
	case *pglogrepl.LogicalDecodingMessage:
		if !m.Transactional {
			return h.Message(ctx, m.Prefix, string(m.Content), m.LSN)
		}
	}
	return nil
}

func (s *Stream) rel(id uint32) (*pglogrepl.RelationMessage, error) {
	rel, ok := s.relations[id]
	if !ok {
		return nil, fmt.Errorf("unknown relation id %d in the replication stream", id)
	}
	return rel, nil
}

func (s *Stream) add(c engine.Change) {
	if s.cur != nil {
		s.cur.Changes = append(s.cur.Changes, c)
	}
}

// decode turns a pgoutput tuple into a Row. Unchanged TOAST values ('u')
// are taken from the old row when the stream sent one.
func decode(rel *pglogrepl.RelationMessage, t *pglogrepl.TupleData, old engine.Row) engine.Row {
	row := make(engine.Row, len(t.Columns))
	for i, col := range t.Columns {
		if i >= len(rel.Columns) {
			break
		}
		name := rel.Columns[i].Name
		switch col.DataType {
		case 't':
			row[name] = string(col.Data)
		case 'u':
			if v, ok := old[name]; ok {
				row[name] = v
			}
		}
	}
	return row
}
