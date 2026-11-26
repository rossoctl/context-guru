package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ErrHostedEraseAll is EraseAll's answer on a hosted (multi-tenant) table: the table and its
// pending writes are shared by every tenant, so "every session" is not one caller's to erase. A
// hosted host purges a tenant (DeleteTenantState). EraseAll changes nothing when it returns this.
var ErrHostedEraseAll = errors.New("store: EraseAll refused on a hosted table; purge per tenant")

// OwnPrefix is the namespace of the stash ownership records: OwnPrefix + session + ":" + key says
// that session stashed the payload under key (offload's recordOwner; GET /expand checks it). It is
// here so EraseSession can find a session's records and the payloads only that session owns.
const OwnPrefix = "cg:own:"

// eraseDeleteTimeout bounds an erase's disk delete. It is Flush's bound, not the 3 s read bound:
// EraseAll deletes a whole table.
const eraseDeleteTimeout = 10 * time.Second

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

// EraseSession removes one session's data, for a host that erases the session. It returns how
// many disk rows went. It removes:
//   - the session's state keys, every thread included: in memory, in the writes still waiting for
//     the disk, and on disk (DeleteSessionState);
//   - its stash ownership records (OwnPrefix), and each rewind payload that no other session's
//     record names. A payload another session also stashed stays: it is that session's too;
//   - its sticky set.
//
// It does not remove the global namespaces (XResultPrefix, SweepExpandPrefix, and the offload
// recurrence marks): they are keyed by content, not by session, so a session's entries cannot be
// told apart. EraseAll removes them.
//
// The order is what makes it complete:
//  1. Memory and the pending writes go together, under the memory lock, so a concurrent Put
//     cannot add a pending write between the two.
//  2. It waits for a flush already running: that flush took its batch before step 1, and would
//     write the rows back after the delete.
//  3. It deletes the rows.
//  4. It repeats step 1 for the state keys. A LoadSession that started after step 1 read rows that
//     step 3 then deleted: if it put them in memory already, they go now; if not, the second
//     erase count makes it keep none of them (eraseGen).
//
// A request for the session that arrives during the erase is new traffic. What it writes before
// step 4 goes; what it writes after stays, as in the host's own session store.
func (m *Memory) EraseSession(session string) (int64, error) {
	if m == nil || session == "" {
		return 0, nil
	}
	sp := sessionPrefixes()
	state := func(k string) bool { return boundaryOK(k, session, sp) }
	m.mu.Lock()
	m.dropLocked(m.sessionMatchLocked(session, state))
	m.forgetSessionLocked(session)
	d := m.disk
	m.mu.Unlock()
	if d == nil {
		return 0, nil
	}
	n, err := d.deleteSession(session)
	m.mu.Lock()
	m.dropLocked(state)
	m.forgetSessionLocked(session)
	m.mu.Unlock()
	return n, err
}

// sessionMatchLocked is the first pass's match for EraseSession: the session's state keys, its
// ownership records, and the payloads only it owns. The caller holds m.mu.
func (m *Memory) sessionMatchLocked(session string, state func(string) bool) func(string) bool {
	own := OwnPrefix + session + ":"
	mine := map[string]bool{} // payload keys this session's records name
	for k := range m.items {
		if strings.HasPrefix(k, own) {
			mine[k[len(own):]] = true
		}
	}
	if len(mine) > 0 {
		// A payload another session also owns stays. Its record is OwnPrefix + other + ":" + key,
		// and a key may hold ':', so test each of this session's keys as a suffix.
		for k := range m.items {
			if !strings.HasPrefix(k, OwnPrefix) || strings.HasPrefix(k, own) {
				continue
			}
			for p := range mine {
				if strings.HasSuffix(k, ":"+p) {
					delete(mine, p)
				}
			}
		}
	}
	return func(k string) bool {
		if state(k) || strings.HasPrefix(k, own) {
			return true
		}
		if !mine[k] {
			return false
		}
		el := m.items[k]
		return el != nil && el.Value.(*entry).stash
	}
}

// forgetSessionLocked ends the erased session's load marks and sticky set, and counts the erase,
// so a disk read that spans it keeps nothing. The caller holds m.mu.
func (m *Memory) forgetSessionLocked(session string) {
	m.eraseGen++
	m.unmarkLoaded(session)
	delete(m.loadFailedAt, session)
	delete(m.sticky, session)
}

// deleteSession waits for a running flush, then deletes the session's rows.
func (d *Disk) deleteSession(session string) (int64, error) {
	d.flushMu.Lock()
	defer d.flushMu.Unlock()
	return deleteSessionState(d.db, session, eraseDeleteTimeout)
}

// EraseAll removes everything this store holds, for a host that erases every session: every key in
// memory (state, rewind payloads, ownership and recurrence marks, the global namespaces), the
// pending writes, every row of the session_state table, the sticky sets and the diagnostic marks.
// It returns how many disk rows went. The order is EraseSession's, with step 4 for every key.
//
// On a hosted table it returns ErrHostedEraseAll and changes nothing: the table and its pending
// writes are shared by every tenant.
func (m *Memory) EraseAll() (int64, error) {
	if m == nil {
		return 0, nil
	}
	all := func(string) bool { return true }
	m.mu.Lock()
	d := m.disk
	if d != nil && d.hosted {
		m.mu.Unlock()
		return 0, ErrHostedEraseAll
	}
	m.dropLocked(all)
	m.forgetAllLocked()
	m.mu.Unlock()
	if d == nil {
		return 0, nil
	}
	n, err := d.deleteAll()
	m.mu.Lock()
	m.dropLocked(all)
	m.forgetAllLocked()
	m.mu.Unlock()
	return n, err
}

// forgetAllLocked is forgetSessionLocked for every session, plus the diagnostic marks. The caller
// holds m.mu.
func (m *Memory) forgetAllLocked() {
	m.eraseGen++
	clear(m.loadedAt)
	m.loadedOrder = nil
	clear(m.loadFailedAt)
	clear(m.sticky)
	clear(m.lostFrozen)
	m.lostOrder = nil
	clear(m.reclaimed)
	m.reclaimedOrder = nil
}

// deleteAll waits for a running flush, then empties the table. A missing table is not an error.
func (d *Disk) deleteAll() (int64, error) {
	d.flushMu.Lock()
	defer d.flushMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), eraseDeleteTimeout)
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
