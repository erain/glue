package eventlog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDurableOrderedReplay(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.DB.Close() })
	tx, err := store.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	iteration := 3
	for _, seq := range []int64{2, 1} {
		phase, value := "maker", iteration
		if seq == 1 {
			phase, value = "planner", 0
		}
		if err := Insert(tx, Envelope{RunID: "job-test", Seq: seq, ID: "event", Type: "tool_start", Phase: phase, Iteration: &value}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := store.DB.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.DB.Close() })
	events, err := reopened.Read("job-test", 0)
	if err != nil || len(events) != 2 || events[0].Seq != 1 || events[1].Seq != 2 {
		t.Fatalf("events %v err %v", events, err)
	}
	if events[0].Iteration == nil || *events[0].Iteration != 0 {
		t.Fatal("planner zero iteration was lost")
	}
	resumed, err := reopened.Read("job-test", 1)
	if err != nil || len(resumed) != 1 || resumed[0].Phase != "maker" || resumed[0].Iteration == nil || *resumed[0].Iteration != 3 {
		t.Fatalf("resumed %v err %v", resumed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "events.sqlite")); err != nil {
		t.Fatal(err)
	}
}
func TestEventTransactionRollsBack(t *testing.T) {
	store, err := Open(t.TempDir(), "CREATE TABLE metadata(id TEXT);")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.DB.Close() })
	tx, err := store.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO metadata(id) VALUES('job')"); err != nil {
		t.Fatal(err)
	}
	event := Envelope{RunID: "job", Seq: 1}
	if err := Insert(tx, event); err != nil {
		t.Fatal(err)
	}
	if err := Insert(tx, event); err == nil {
		t.Fatal("accepted duplicate sequence")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	events, err := store.Read("job", 0)
	if err != nil || len(events) != 0 {
		t.Fatalf("events %v err %v", events, err)
	}
	var count int
	if err := store.DB.QueryRow("SELECT COUNT(*) FROM metadata").Scan(&count); err != nil || count != 0 {
		t.Fatalf("count %d err %v", count, err)
	}
}
