package storage

import (
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbe(t *testing.T) {
	s := newStore(t)
	d := &fakeDisk{free: 110}
	s.statfs = d.statfs
	if free, err := s.Probe(time.Second); err != nil || free != 110 {
		t.Fatalf("probe: %d %v", free, err)
	}

	moved := s.root + "-moved"
	if err := os.Rename(s.root, moved); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Probe(time.Second); !errors.Is(err, ErrRootMoved) {
		t.Errorf("moved root: %v", err)
	}
	if err := os.Mkdir(s.root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Probe(time.Second); !errors.Is(err, ErrRootMoved) {
		t.Errorf("replaced root: %v", err)
	}
	if err := os.Remove(s.root); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(moved, s.root); err != nil {
		t.Fatal(err)
	}

	// A failing statfs is a failing root; a platform without one is not.
	d.err = errors.New("input/output error")
	if _, err := s.Probe(time.Second); !errors.Is(err, d.err) {
		t.Errorf("statfs error: %v", err)
	}
	d.err = errNoDiskSpace
	if free, err := s.Probe(time.Second); err != nil || free != -1 {
		t.Errorf("no statfs: %d %v", free, err)
	}
}

func TestProbeHungRoot(t *testing.T) {
	s := newStore(t)
	release, started := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	var once sync.Once
	s.probeRead = func() (int64, error) {
		reads.Add(1)
		once.Do(func() { close(started) })
		<-release
		return 0, nil
	}
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			if _, err := s.Probe(10 * time.Millisecond); !errors.Is(err, ErrProbeTimeout) {
				t.Errorf("hung probe: %v", err)
			}
		})
	}
	wg.Wait()
	<-started
	if n := reads.Load(); n != 1 {
		t.Errorf("%d reads of a hung root, want 1", n)
	}
	close(release)
	if _, err := s.Probe(time.Second); err != nil {
		t.Errorf("after the root answers: %v", err)
	}
}

func TestListBucketsWithinHungMount(t *testing.T) {
	s := newStore(t)
	release, started := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	var once sync.Once
	list := s.listRead
	s.listRead = func() ([]BucketInfo, error) {
		calls.Add(1)
		once.Do(func() { close(started) })
		<-release
		return list()
	}
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			if _, err := s.ListBucketsWithin(10 * time.Millisecond); !errors.Is(err, ErrProbeTimeout) {
				t.Errorf("hung listing: %v", err)
			}
		})
	}
	wg.Wait()
	<-started
	if n := calls.Load(); n != 1 {
		t.Errorf("%d listings on a hung mount, want 1", n)
	}
	close(release)
	if b, err := s.ListBucketsWithin(time.Second); err != nil || len(b) != 1 || b[0].Name != "bkt" {
		t.Errorf("after the mount answers: %v %v", b, err)
	}
}

func TestDiskSpaceWithinHungMount(t *testing.T) {
	s := newStore(t)
	release, started := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	var once sync.Once
	s.statfs = func(string) (int64, int64, error) {
		calls.Add(1)
		once.Do(func() { close(started) })
		<-release
		return 10, 20, nil
	}
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			if _, _, err := s.DiskSpaceWithin(10 * time.Millisecond); !errors.Is(err, ErrProbeTimeout) {
				t.Errorf("hung statfs: %v", err)
			}
		})
	}
	wg.Wait()
	<-started
	if n := calls.Load(); n != 1 {
		t.Errorf("%d statfs calls on a hung mount, want 1", n)
	}
	close(release)
	if free, size, err := s.DiskSpaceWithin(time.Second); err != nil || free != 10 || size != 20 {
		t.Errorf("after the mount answers: %d %d %v", free, size, err)
	}
}
