package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Slot states (§4.6): the key's live slot has one holder at a time, an
// attached chat or a headless turn.
type SlotState string

const (
	SlotFree   SlotState = "free"
	SlotTurn   SlotState = "headless turn"
	SlotAttach SlotState = "attach"
)

// Slot is the foreman slot of one project and who holds it (§4.6): the
// holder's id (attach hold id, or job id for a turn), its label, since
// when and, for a turn, the events it carries.
type Slot struct {
	ProjectID int64     `json:"project_id"`
	State     SlotState `json:"state"`
	HolderID  int64     `json:"holder_id,omitempty"`
	Label     string    `json:"label,omitempty"`
	Since     string    `json:"since,omitempty"`
	Events    []string  `json:"events,omitempty"`
}

// slotTx reads who holds the project's slot (§4.6). The caller has swept,
// so a hold or lease that lapsed is gone.
func slotTx(tx *sql.Tx, project int64, now time.Time) (Slot, error) {
	slot := Slot{ProjectID: project, State: SlotFree}
	var id int64
	var since string
	err := tx.QueryRow(`SELECT id, created_at FROM attach_holds WHERE project_id = ? AND state = 'live' AND expires_at > ?`,
		project, fmtTime(now)).Scan(&id, &since)
	switch {
	case err == nil:
		return Slot{ProjectID: project, State: SlotAttach, HolderID: id, Label: "attached chat", Since: since}, nil
	case !errors.Is(err, sql.ErrNoRows):
		return slot, fmt.Errorf("read the attach hold of project %d: %w", project, err)
	}
	live, err := liveJobTx(tx, fmt.Sprintf("foreman:%d", project))
	if err != nil {
		return slot, err
	}
	if live != nil {
		since := live.FirstLeased
		if since == "" {
			since = live.QueuedAt
		}
		carried, err := inboxRowsTx(tx, project, inboxDelivered, live.ID)
		if err != nil {
			return slot, err
		}
		events := append([]string{}, carried.Events...)
		for _, m := range carried.UserMessages {
			events = append(events, "user message from "+m.Source)
		}
		return Slot{ProjectID: project, State: SlotTurn, HolderID: live.ID, Label: "foreman turn",
			Since: since, Events: events}, nil
	}
	return slot, nil
}

// ForemanSlot reads the project's slot: what `brigada status` shows.
func (e *Engine) ForemanSlot(ctx context.Context, project int64) (Slot, error) {
	var slot Slot
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		var err error
		slot, err = slotTx(tx, project, now)
		return err
	})
	return slot, err
}
