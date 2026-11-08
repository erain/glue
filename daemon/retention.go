package daemon

import "time"

func (s *Server) sweepRuns() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.log.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query("SELECT id FROM runs WHERE status!='running' AND updated_at<?", timestamp(s.now().Add(-s.retainRuns)))
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		for _, table := range []string{"events", "permissions", "runs"} {
			column := "run_id"
			if table == "runs" {
				column = "id"
			}
			if _, err := tx.Exec("DELETE FROM "+table+" WHERE "+column+"=?", id); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, id := range ids {
		delete(s.runs, id)
	}
	return nil
}
func (s *Server) sweepLoop(interval time.Duration) {
	defer close(s.sweepDone)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.sweepStop:
			return
		case <-ticker.C:
			if err := s.sweepRuns(); err != nil {
				s.recordError(DiagnosticError{Error: err.Error()})
			}
		}
	}
}

// Close stops retention, cancels active executions, and closes state storage.
// Hosts should stop accepting HTTP requests before calling Close.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.lifecycleMu.Lock()
		s.closing = true
		close(s.sweepStop)
		s.mu.Lock()
		for _, r := range s.runs {
			r.cancel()
		}
		s.mu.Unlock()
		s.lifecycleMu.Unlock()
		<-s.sweepDone
		s.runWG.Wait()
		s.closeErr = s.log.db.Close()
	})
	return s.closeErr
}
