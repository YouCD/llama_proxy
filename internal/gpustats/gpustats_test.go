package gpustats

import (
	"errors"
	"testing"
)

func TestQueryParse(t *testing.T) {
	run := func() ([]byte, error) {
		return []byte("0, NVIDIA H100 80GB HBM3, 73, 61440, 81520, 55, 62, 350.50\n" +
			"1, NVIDIA H100 80GB HBM3, 0, 1024, 81520, [N/A], 35, 71.20\n"), nil
	}
	snap := query(run)
	if !snap.Available {
		t.Fatalf("expected available snapshot, got error: %s", snap.Error)
	}
	if len(snap.Gpus) != 2 {
		t.Fatalf("expected 2 gpus, got %d", len(snap.Gpus))
	}
	g0 := snap.Gpus[0]
	if g0.Index != 0 || g0.Name != "NVIDIA H100 80GB HBM3" || g0.Util != 73 ||
		g0.MemUsed != 61440 || g0.MemTotal != 81520 || g0.Fan != 55 || g0.Temp != 62 || g0.Power != 350.5 {
		t.Errorf("gpu0 parse mismatch: %+v", g0)
	}
	g1 := snap.Gpus[1]
	// [N/A] 风扇转速解析为 0
	if g1.Fan != 0 || g1.Power != 71.2 {
		t.Errorf("gpu1 parse mismatch: %+v", g1)
	}
}

func TestQueryNoSMI(t *testing.T) {
	run := func() ([]byte, error) { return nil, errors.New("nvidia-smi not found") }
	snap := query(run)
	if snap.Available {
		t.Fatal("expected unavailable snapshot")
	}
	if snap.Gpus == nil {
		t.Fatal("expected non-nil gpus slice (JSON 应输出 [] 而非 null)")
	}
	if snap.Error == "" {
		t.Fatal("expected error message")
	}
}

func TestSignatureChange(t *testing.T) {
	a := &Snapshot{Available: true, Gpus: []GPU{{Index: 0, Util: 10, MemUsed: 100}}}
	b := &Snapshot{Available: true, Gpus: []GPU{{Index: 0, Util: 10, MemUsed: 100}}}
	c := &Snapshot{Available: true, Gpus: []GPU{{Index: 0, Util: 11, MemUsed: 100}}}
	if a.Signature() != b.Signature() {
		t.Fatal("same data must have same signature")
	}
	if a.Signature() == c.Signature() {
		t.Fatal("util change must change signature")
	}
}
