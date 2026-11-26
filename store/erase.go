package store

import (
	"context"
	"errors"
	"strings"
)

// ErrHostedEraseAll is EraseAll's answer on a hosted (multi-tenant) table: the table is shared, so
// "every session" is not one caller's to erase. A hosted host purges a tenant (DeleteTenantState).
var ErrHostedEraseAll = errors.New("store: EraseAll refused on a hosted table; purge per tenant")

// sessionPrefixes is every namespace whose key carries a session id: StatePrefixes without the
// global ones.
func sessionPrefixes() []string {
	out := make([]string, 0, len(StatePrefixes))
	for _, p := range StatePrefixes {
		if !hasAnyPrefix(p, globalPrefixes) {
			out = append(out, p)
		}
	}
	return out
}

// EraseSession removes one session's state, for a host that erases the session: its keys in
// memory, its writes still waiting for the disk, and its rows on disk (DeleteSessionState), every
// thread included. It returns how many disk rows went.
//
// The order is what makes it complete:
//  1. Memory and the pending writes go together, under the memory lock, so a concurrent Put
//     cannot add a pending write between the two.
//  2. It waits for a flush already running: that flush took its batch before step 1, and would
//     write the rows back after the delete.
//  3. It deletes the rows.
//
// A LoadSession or global read that is reading the disk during the erase keeps none of what it
// read (eraseGen). A request for the session that arrives after step 1 is new traffic: it creates
// new state, as it does in the host's own session store.
//
// The global namespaces (XResultPrefix, SweepExpandPrefix) are keyed by content, not by session,
// so EraseSession cannot find a session's rows there; EraseAll removes them.
func (m *Memory) EraseSession(session string) (int64, error) {
	if m == nil || session == "" {
		return 0, nil
	}
	sp := sessionPrefixes()
	m.mu.Lock()
	m.dropLocked(func(k string) bool { return boundaryOK(k, session, sp) })
	m.eraseGen++
	m.unmarkLoaded(session)
	delete(m.loadFailedAt, session)
	d := m.disk
	m.mu.Unlock()
	if d == nil {
		return 0, nil
	}
	d.flushMu.Lock()
	defer d.flushMu.Unlock()
	return DeleteSessionState(d.db, session)
}

// EraseAll removes every state key, for a host that erases every session: memory, the pending
// writes, and every row of the session_state table, the global namespaces included. It returns how
// many disk rows went. The order is EraseSession's. On a hosted table it erases memory only and
// returns ErrHostedEraseAll.
func (m *Memory) EraseAll() (int64, error) {
	if m == nil {
		return 0, nil
	}
	m.mu.Lock()
	m.dropLocked(isStateKey)
	m.eraseGen++
	clear(m.loadedAt)
	m.loadedOrder = nil
	clear(m.loadFailedAt)
	d := m.disk
	m.mu.Unlock()
	if d == nil {
		return 0, nil
	}
	if d.hosted {
		return 0, ErrHostedEraseAll
	}
	d.flushMu.Lock()
	defer d.flushMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), d.loadTimeout)
	defer cancel()
	res, err := d.db.ExecContext(ctx, `DELETE FROM session_state`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return 0, nil
		}
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
