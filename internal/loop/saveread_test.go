package loop

import (
	"sync"
	"testing"
)

// Saving must not fail because something is reading the checkpoint at that
// moment. The page does exactly that: it reloads the conversation list, which
// reads every checkpoint, when a turn starts. On Windows a rename over a file
// another handle has open fails with "Access is denied", so with a model that
// answered within milliseconds, every turn in the v0.6.9 browser check died
// at "checkpoint failed at step 1".
func TestSaveSucceedsWhileTheCheckpointIsBeingRead(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	st := NewState("run_20260924T000000_abcdef", "task")
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = store.LoadCheckpoint(st.RunID)
				}
			}
		}()
	}
	defer func() { close(stop); wg.Wait() }()

	for i := range 300 {
		st.Steps = i
		if err := store.Save(st); err != nil {
			t.Fatalf("save %d failed while the checkpoint was being read: %v", i, err)
		}
	}
	got, err := store.Load(st.RunID)
	if err != nil || got.Steps != 299 {
		t.Fatalf("after the saves: steps %v (err %v), want the last save, 299", got, err)
	}
}
