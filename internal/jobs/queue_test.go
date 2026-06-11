package jobs

import (
	"testing"
	"time"
)

func TestEnqueueClaimComplete(t *testing.T) {
	q := NewQueue(time.Minute)
	q.Enqueue("A", "B", "", "asm5", 500)

	j, ok := q.Claim("w1")
	if !ok || j.State != Claimed || j.Worker != "w1" {
		t.Fatalf("claim failed: %+v ok=%v", j, ok)
	}
	if _, ok := q.Claim("w2"); ok {
		t.Error("second claim should find nothing pending")
	}
	if !q.Complete(j.ID, "w1", 10, 9) {
		t.Error("complete should succeed")
	}
	list := q.List()
	if list[0].State != Done || list[0].Found != 10 || list[0].Valid != 9 {
		t.Errorf("unexpected job after complete: %+v", list[0])
	}
}

func TestClaimFIFO(t *testing.T) {
	q := NewQueue(time.Minute)
	a := q.Enqueue("A", "B", "", "asm5", 500)
	b := q.Enqueue("C", "D", "", "asm5", 500)
	j1, _ := q.Claim("w1")
	j2, _ := q.Claim("w2")
	if j1.ID != a.ID || j2.ID != b.ID {
		t.Errorf("FIFO order broken: got %s,%s want %s,%s", j1.ID, j2.ID, a.ID, b.ID)
	}
}

func TestHeartbeatGuards(t *testing.T) {
	q := NewQueue(time.Minute)
	q.Enqueue("A", "B", "", "asm5", 500)
	j, _ := q.Claim("w1")
	if q.Heartbeat(j.ID, "w2") {
		t.Error("heartbeat from wrong worker should fail")
	}
	if !q.Heartbeat(j.ID, "w1") {
		t.Error("heartbeat from owner should succeed")
	}
}

func TestReclaimOnTimeout(t *testing.T) {
	q := NewQueue(20 * time.Millisecond)
	q.Enqueue("A", "B", "", "asm5", 500)
	j, _ := q.Claim("w1")
	time.Sleep(40 * time.Millisecond)
	// a new claim should reclaim the abandoned job
	j2, ok := q.Claim("w2")
	if !ok || j2.ID != j.ID || j2.Worker != "w2" {
		t.Errorf("expected reclaimed job for w2, got %+v ok=%v", j2, ok)
	}
}

func TestCompleteWrongWorker(t *testing.T) {
	q := NewQueue(time.Minute)
	q.Enqueue("A", "B", "", "asm5", 500)
	j, _ := q.Claim("w1")
	if q.Complete(j.ID, "w2", 1, 1) {
		t.Error("complete by non-owner should fail")
	}
}
