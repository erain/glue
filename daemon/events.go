package daemon

import (
	"database/sql"

	"github.com/erain/glue/internal/eventlog"
)

type eventLog struct {
	db    *sql.DB
	store *eventlog.Store
}

func openEventLog(dir string) (*eventLog, error) {
	store, err := eventlog.Open(dir, registrySchema+permissionSchema)
	if err != nil {
		return nil, err
	}
	return &eventLog{db: store.DB, store: store}, nil
}
func (l *eventLog) append(e EventEnvelope) error {
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := appendRunEvent(tx, e); err != nil {
		return err
	}
	return tx.Commit()
}
func (l *eventLog) read(id string, after int64) ([]EventEnvelope, error) {
	stored, err := l.store.Read(id, after)
	if err != nil {
		return nil, err
	}
	events := make([]EventEnvelope, len(stored))
	for i, e := range stored {
		events[i] = EventEnvelope(e)
	}
	return events, nil
}
