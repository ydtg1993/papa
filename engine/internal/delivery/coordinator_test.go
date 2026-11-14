package delivery

import "testing"

func TestSharedCapacityWakesOnlyUnfinishedScans(t *testing.T) {
	c := New()
	for _, key := range []string{"blocked", "ended"} {
		c.Wake(key)
		<-c.Notifications(key)
		_, revision, _ := c.Scan(key)
		c.Advance(key, revision, 42, key == "ended")
	}
	c.ContinueAll()
	select {
	case <-c.Notifications("blocked"):
	default:
		t.Fatal("shared capacity did not wake unfinished scan")
	}
	select {
	case <-c.Notifications("ended"):
		t.Fatal("shared capacity restarted exhausted scan")
	default:
	}
	if cursor, revision, end := c.Scan("blocked"); cursor != 42 || revision != 1 || end {
		t.Fatalf("capacity changed durable scan: %d/%d/%v", cursor, revision, end)
	}
}

func TestCapacityContinuesCursorAndBusinessWakeRestarts(t *testing.T) {
	c := New()
	c.Wake("s")
	<-c.Notifications("s")
	_, revision, _ := c.Scan("s")
	if !c.Advance("s", revision, 42, false) {
		t.Fatal("advance failed")
	}
	c.Continue("s")
	<-c.Notifications("s")
	if cursor, rev, end := c.Scan("s"); cursor != 42 || rev != revision || end {
		t.Fatal("capacity reset scan")
	}
	c.Advance("s", revision, 42, true)
	c.Continue("s")
	select {
	case <-c.Notifications("s"):
		t.Fatal("completed scan was woken by capacity")
	default:
	}
	c.Wake("s")
	if cursor, rev, end := c.Scan("s"); cursor != 0 || rev == revision || end {
		t.Fatal("business event did not restart scan")
	}
	if c.Advance("s", revision, 99, true) {
		t.Fatal("old scan overwrote business reset")
	}
}

func TestDeadlineRestartDoesNotQueueSpuriousWake(t *testing.T) {
	c := New()
	c.Restart("s")
	if cursor, revision, end := c.Scan("s"); cursor != 0 || revision != 1 || end {
		t.Fatal("deadline did not reset scan")
	}
	select {
	case <-c.Notifications("s"):
		t.Fatal("deadline queued duplicate scan")
	default:
	}
}

func TestWaitingClaimAcknowledgesOldEventsWithoutLosingNewEvent(t *testing.T) {
	for _, newer := range []bool{false, true} {
		c := New()
		c.Wake("s")
		<-c.Notifications("s")
		if !c.AcquireForStage(7, "s") {
			t.Fatal("acquire")
		}
		c.Wake("s")
		<-c.Notifications("s")
		c.SkipOwned(7, "s")
		_, observed, _ := c.Scan("s")
		if newer {
			c.Wake("s")
			<-c.Notifications("s")
		}
		c.ObservedWaiting(7, "s", observed)
		c.SkipOwned(7, "s")
		c.Release(7)
		select {
		case <-c.Notifications("s"):
			if !newer {
				t.Fatal("waiting claim replayed already observed event")
			}
		default:
			if newer {
				t.Fatal("concurrent business event lost")
			}
		}
	}
}
